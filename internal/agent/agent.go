// Package agent runs the detect → collect → diagnose → notify loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teknik-github/PodsMedic/internal/audit"
	"github.com/teknik-github/PodsMedic/internal/breaker"
	"github.com/teknik-github/PodsMedic/internal/capacity"
	"github.com/teknik-github/PodsMedic/internal/config"
	"github.com/teknik-github/PodsMedic/internal/dedupe"
	"github.com/teknik-github/PodsMedic/internal/detect"
	"github.com/teknik-github/PodsMedic/internal/digest"
	"github.com/teknik-github/PodsMedic/internal/heal"
	"github.com/teknik-github/PodsMedic/internal/incident"
	"github.com/teknik-github/PodsMedic/internal/k8s"
	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/llm"
	"github.com/teknik-github/PodsMedic/internal/metrics"
	"github.com/teknik-github/PodsMedic/internal/nodes"
	"github.com/teknik-github/PodsMedic/internal/notify"
	"github.com/teknik-github/PodsMedic/internal/playbook"
	"github.com/teknik-github/PodsMedic/internal/predict"
	"github.com/teknik-github/PodsMedic/internal/rightsize"

	corev1 "k8s.io/api/core/v1"
)

// Agent ties the pipeline stages together.
type Agent struct {
	cfg       *config.Config
	kube      *k8s.Client
	brain     llm.Client
	notifier  notify.Notifier
	incidents *incident.Store
	log       *slog.Logger

	// lastSweep is the most recent sweep's cluster picture, published for the
	// Telegram chat path to answer questions from. Guarded because a chat answer
	// runs concurrently with the sweep loop.
	sweepMu   sync.RWMutex
	lastSweep *sweepSnapshot
	// lastNodes and lastNodeFaults answer /nodes. Guarded by the same lock and
	// for the same reason: they cross sweeps.
	lastNodes      []nodes.State
	lastNodeFaults []nodes.Finding

	// Auto-heal collaborators. healer is nil when auto-heal is disabled.
	healer   *heal.Executor
	healOpts heal.Options
	healSeen *dedupe.Cache
	// Verification. healStore is nil unless heals are applied for real and
	// verification is enabled; it persists applied heals so they can be
	// re-checked (and rolled back) in a later sweep, surviving a restart.
	healStore       heal.Store
	healVerifyAfter time.Duration

	// audit is the durable heal trail. Never nil: NopLog when disabled.
	audit audit.Log

	// rightsize accumulates usage against declared requests. Report-only: see
	// internal/rightsize on why there is no heal for it.
	rightsize   *rightsize.Tracker
	rightsizeCM string

	// Daily digest. tally is never nil — it accumulates whether or not the
	// digest is enabled, so /digest can answer immediately after someone turns
	// it on.
	tally       *digest.Tally
	digestAt    digest.Schedule
	digestOn    bool
	digestLast  time.Time
	digestDirty bool

	// nodeSeen silences a node fault that is already known. A node stays
	// NotReady for as long as it stays NotReady, so without this it would be the
	// same alert every sweep.
	nodeSeen *dedupe.Cache

	// breaker suspends healing a workload that keeps failing its heals. Nil when
	// disabled.
	breaker *breaker.Breaker
	// surgeAnnounced keeps a cluster-wide suspension to one notice rather than
	// one per sweep for as long as the outage lasts.
	surgeAnnounced bool

	// incidentStateCM is the ConfigMap that persists open incidents (with their
	// heal-retry proposals) across restarts. Empty disables persistence.
	incidentStateCM string

	// playbook remembers verified heals so a recurring problem can be fixed by
	// replaying the known remedy — no LLM diagnosis. Nil when disabled.
	playbook   *playbook.Book
	playbookCM string

	// predictor flags containers whose memory sits near the limit, so they can be
	// healed before the OOM kill. Nil when disabled.
	predictor *predict.Predictor

	// live carries what is happening to the visualisation. Nil when the view is
	// off; Stream.Publish tolerates that, so call sites stay unconditional.
	live *live.Stream
	// declines suppresses the heal-retry loop's repeated refusals, which would
	// otherwise be one event per open incident per sweep, forever.
	declines *live.Suppressor
}

// SetLive attaches the event stream the live view reads.
func (a *Agent) SetLive(s *live.Stream) {
	a.live = s
	a.declines = live.NewSuppressor(declineQuiet)
}

// declineQuiet is how long an unchanged refusal stays off the live feed. Long
// enough that an incident nobody can heal does not dominate the display, short
// enough that a refusal which is still true resurfaces occasionally.
const declineQuiet = 30 * time.Minute

// emit publishes one podsmedic-side event.
//
// It resolves the pod's workload from the last sweep so that a heal lands on the
// same node in the view as the crash that prompted it. Without that, an action
// on web-6b4f-xyz would draw a wire to a node separate from Deployment/web and
// the cause-and-effect the display exists to show would be lost.
func (a *Agent) emit(class live.Class, p detect.Problem, reason, detail string) {
	e := live.Event{
		Class: class, Namespace: p.Namespace, Pod: p.Pod, Container: p.Container,
		Workload: a.workloadOf(p.Namespace, p.Pod),
		Reason:   reason, Detail: detail,
	}
	// A refusal repeats every sweep for as long as the incident is open, which is
	// structural rather than newsworthy. Everything else is emitted as it happens.
	if class == live.ClassDeclined && !a.declines.Allow(e, time.Now()) {
		return
	}
	a.live.Publish(e)
}

// emitRecord publishes an event about a stored heal. Verification happens long
// after the original pod may have been replaced, so this keys on the controller
// the record names rather than a pod that might no longer exist.
func (a *Agent) emitRecord(class live.Class, rec heal.HealRecord, reason, detail string) {
	a.live.Publish(live.Event{
		Class: class, Namespace: rec.Namespace, Container: rec.Container,
		Workload: rec.ControllerKind + "/" + rec.ControllerName,
		Reason:   reason, Detail: detail + ": " + rec.Summary,
	})
}

// workloadOf names the controller owning a pod, from the last sweep's index.
func (a *Agent) workloadOf(namespace, pod string) string {
	a.sweepMu.RLock()
	defer a.sweepMu.RUnlock()
	if a.lastSweep == nil {
		return ""
	}
	return a.lastSweep.workloads[namespace+"/"+pod]
}

// New builds an agent from its collaborators.
func New(cfg *config.Config, kube *k8s.Client, brain llm.Client, notifier notify.Notifier, log *slog.Logger) *Agent {
	a := &Agent{
		cfg:       cfg,
		kube:      kube,
		brain:     brain,
		notifier:  notifier,
		incidents: incident.NewStore(cfg.Cooldown),
		log:       log,
		audit:     audit.NopLog{},

		incidentStateCM: cfg.IncidentStateName,
		nodeSeen:        dedupe.New(cfg.NodeCooldown),
		tally:           digest.NewTally(time.Now()),
	}
	// A bad schedule degrades the digest rather than stopping the agent: an
	// operator's typo in a summary time must not cost them cluster monitoring.
	if sched, on, err := digest.ParseSchedule(cfg.DigestAt, digestLocation(cfg.DigestTZ, log)); err != nil {
		log.Error("daily digest disabled: PODSMEDIC_DIGEST_AT is not a time of day", "value", cfg.DigestAt, "err", err)
	} else if on {
		a.digestAt, a.digestOn = sched, true
		// Seeded to now, not zero: a fresh install must not immediately send a
		// summary of a day it did not watch.
		a.digestLast = time.Now()
		log.Info("daily digest scheduled", "at", sched.String())
	}
	// Independent of auto-heal: knowing a workload is three times oversized is
	// useful whether or not podsmedic is allowed to change anything.
	if cfg.Rightsize {
		a.rightsize = rightsize.New(cfg.RightsizeMaxTracked)
		a.rightsizeCM = cfg.RightsizeName
	}
	if cfg.AutoHeal {
		// The trail records dry runs as well as real applies, so it is wired
		// whenever auto-heal is on and a ConfigMap name is configured.
		if cfg.AuditName != "" {
			a.audit = audit.NewConfigMapLog(kube, k8s.Namespace(), cfg.AuditName, cfg.AuditMaxEvents)
		}
		if cfg.HealBreaker {
			a.breaker = breaker.New(breaker.Options{
				Window:       cfg.HealBreakerWindow,
				MaxHeals:     cfg.HealBreakerMaxHeals,
				MaxRollbacks: cfg.HealBreakerMaxRollbacks,
				OpenFor:      cfg.HealBreakerOpenFor,
			})
		}
		if cfg.PlaybookName != "" {
			a.playbook = playbook.New(playbook.Options{
				MaxEntries:    cfg.PlaybookMaxEntries,
				MaxFailures:   cfg.PlaybookMaxFailures,
				QuarantineFor: cfg.PlaybookQuarantineFor,
				FailureDecay:  cfg.PlaybookFailureDecay,
				MaxAge:        cfg.PlaybookMaxAge,
			})
			a.playbookCM = cfg.PlaybookName
		}
		// HealOptions was already validated in config.Load, so the error here
		// is not expected; ignore it and run with whatever parsed.
		a.healOpts, _ = cfg.HealOptions()
		a.healer = heal.NewExecutor(kube, cfg.HealApply, cfg.HealAllowGitOps)
		a.healSeen = dedupe.New(cfg.HealCooldown)

		// Verification only makes sense once heals are applied for real: a dry
		// run changes nothing to verify or undo.
		if cfg.HealApply && cfg.HealVerify {
			a.healStore = heal.NewConfigMapStore(kube, k8s.Namespace(), cfg.HealStateName)
			a.healVerifyAfter = cfg.HealVerifyAfter
		}
	}
	// Prediction is independent of auto-heal: without healing it still alerts on
	// impending OOMs; with healing (and MemoryPressure in HEAL_KINDS) it fixes
	// them pre-emptively.
	if cfg.Predict {
		a.predictor = predict.New(predict.Options{
			HighRatio:      cfg.PredictHighRatio,
			MinConsecutive: cfg.PredictMinChecks,
		})
	}
	return a
}

// Run polls until the context is cancelled. The first sweep runs immediately
// so a freshly started agent does not wait a full interval before reporting.
func (a *Agent) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	// Reload incidents left open by a previous run, so the first sweep continues
	// them instead of re-alerting, and the learned-heal playbook.
	a.loadIncidents(ctx)
	a.loadPlaybook(ctx)
	a.loadRightsize(ctx)

	a.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

// sweepState is the cluster-wide evidence gathered once per sweep and shared by
// every problem handled within it: the capacity snapshot that bounds anything
// which would add or enlarge pods, plus the pod list and usage samples a
// workload's aggregate load is derived from.
//
// It is built before any handler runs and never mutated afterwards, so the
// concurrent handlers in handleAll only ever read it.
type sweepState struct {
	// budget bounds how many heals this one sweep may execute, and suspended is
	// set when the failure pattern looks systemic. Both are cluster-wide, which
	// every other limit is not: the cooldown, the circuit breaker, and the
	// playbook are all per workload, so N distinct workloads failing at once pass
	// all of them.
	budget    *breaker.Budget
	suspended string

	capacity *capacity.Snapshot
	pods     []corev1.Pod
	usage    []k8s.ContainerUsage
	// autoscalers indexes every HPA by the workload it targets, so a scale heal
	// can be refused for a workload something else already scales.
	autoscalers map[string]k8s.AutoscalerRef
}

// newSweepState gathers the cluster-wide evidence for one sweep.
//
// Both reads are best-effort in the sense that they never abort the sweep, but
// they are not interchangeable in what a failure means: missing usage only
// costs prediction and the derived replica target, whereas a missing capacity
// snapshot makes the validator refuse every heal that would add pods. That is
// deliberate — scaling blind is the failure mode the snapshot exists to prevent.
func (a *Agent) newSweepState(ctx context.Context, pods []corev1.Pod) *sweepState {
	st := &sweepState{pods: pods, budget: breaker.NewBudget(a.cfg.HealMaxPerSweep)}

	// Live usage feeds the OOM/CPU predictor, the derived replica target, and
	// the sizing history. Rightsizing has to be in this condition or it silently
	// observes nothing on a cluster with prediction and auto-heal both off,
	// which is the default — the report would just never fill up.
	if a.predictor != nil || a.healer != nil || a.rightsize != nil {
		usage, err := a.kube.UsageSamples(ctx, pods, a.cfg.Namespaces)
		if err != nil {
			a.log.Warn("live usage unavailable (metrics-server?)", "err", err)
		}
		st.usage = usage
	}

	if a.healer == nil {
		return st
	}

	// Who else is scaling? Unlike capacity this does not fail closed: see
	// k8s.ListAutoscalers on why the two differ.
	if index, err := a.kube.ListAutoscalers(ctx); err != nil {
		a.log.Warn("autoscaler list unreadable — a scale heal cannot be checked for an HPA conflict", "err", err)
	} else {
		st.autoscalers = index
	}

	snap, err := a.kube.ClusterCapacity(ctx, a.cfg.HealCapacityReserve)
	if err != nil {
		a.log.Warn("cluster capacity unreadable — heals that would add or enlarge pods will be declined", "err", err)
		return st
	}
	st.capacity = snap
	sum := snap.Summary()
	metrics.ClusterCPUFree.Set(float64(sum.CPUFreeMilli))
	metrics.ClusterMemoryFree.Set(float64(sum.MemFreeBytes))
	metrics.ClusterPodSlotsFree.Set(float64(sum.PodSlotsFree))
	metrics.ClusterNodesSchedulable.Set(float64(sum.SchedulableNodes))
	a.log.Debug("cluster capacity", "free", sum.Describe(), "reservePct", sum.ReservePercent)
	return st
}

// enrich attaches the sweep's cluster capacity and this workload's aggregate
// load to a freshly collected bundle. Both are what let a scale-up be sized
// from measurement and bounded by real headroom instead of a configured number.
func (a *Agent) enrich(ctx context.Context, b *k8s.Bundle, st *sweepState) {
	if st == nil {
		return
	}
	b.Capacity = st.capacity
	if b.Controller == (k8s.ControllerRef{}) {
		return
	}
	b.Autoscaler = k8s.Autoscaler(st.autoscalers, b.Controller)
	if len(st.usage) == 0 {
		return
	}
	load := a.kube.WorkloadLoad(ctx, b.Controller, b.Replicas, st.pods, st.usage)
	if load.Sampled > 0 {
		b.Load = &load
		b.LoadSummary = load.String()
	}
}

// sweep performs one full pass over the cluster.
func (a *Agent) sweep(ctx context.Context) {
	start := time.Now()

	pods, err := a.kube.ListPods(ctx, a.cfg.Namespaces)
	if err != nil {
		a.log.Error("list pods failed", "err", err)
		return
	}
	st := a.newSweepState(ctx, pods)

	opts := detect.DefaultOptions()
	opts.MinRestarts = a.cfg.MinRestarts
	opts.RestartWindow = a.cfg.RestartWindow
	opts.NotReadyGrace = a.cfg.NotReadyGrace
	opts.VolumeMountGrace = a.cfg.VolumeMountGrace
	problems := detect.Pods(pods, opts)

	// Predictive pass: add MemoryPressure problems for containers trending into an
	// OOM, so they can be healed before the kill. Skipped for a container that
	// already has a real problem this sweep — the actual failure takes priority.
	problems = append(problems, a.predictProblems(st, problems)...)

	metrics.SweepsTotal.Inc()
	a.tally.Sweep()
	metrics.PodsScanned.Set(float64(len(pods)))
	metrics.ProblemsDetected.Set(float64(len(problems)))

	now := time.Now()

	// Correlate every problem into its incident. New incidents get the full
	// diagnose+alert path; a new symptom on an existing incident gets a
	// lightweight follow-up; repeats are suppressed.
	var fresh []*incident.Incident
	for _, p := range problems {
		inc, action := a.incidents.Observe(p, now)
		switch action {
		case incident.New:
			fresh = append(fresh, inc)
			metrics.IncidentsTotal.Inc()
			a.tally.IncidentOpened()
		case incident.Update:
			a.notifyIncidentUpdate(ctx, inc, p.Kind)
			a.maybeRetryHeal(ctx, inc, st)
		case incident.Suppress:
			a.maybeRetryHeal(ctx, inc, st)
		}
	}

	// Cluster-wide brake. Counted over distinct workloads rather than pods, so a
	// single Deployment with thirty crashing replicas is one failure, not thirty.
	if a.healer != nil {
		failing, total := workloadFailureCounts(problems, st)
		if tripped, why := breaker.Surge(failing, total, a.clusterOpts()); tripped {
			st.suspended = why
			a.announceSurge(ctx, why)
		} else {
			a.surgeAnnounced = false
		}
	}

	// Make this sweep's picture available to the chat path before any slower
	// work (diagnosis, healing) runs, so a question asked mid-sweep still gets
	// fresh counts.
	a.publishSweep(len(pods), problems, st)
	// Heartbeat, so a healthy cluster still looks alive on the live view.
	a.live.PublishEphemeral(live.Event{
		Class: live.ClassSweep, Reason: "sweep",
		Detail: fmt.Sprintf("%d pods, %d problem(s)", len(pods), len(problems)),
	})

	a.log.Info("sweep complete",
		"pods", len(pods),
		"problems", len(problems),
		"newIncidents", len(fresh),
		"openIncidents", a.incidents.OpenCount(),
		"took", time.Since(start).Round(time.Millisecond))

	// Fold this sweep's usage into the sizing history. Cheap: it reuses the
	// samples prediction already gathered.
	a.observeUsage(st)

	// The machines under the workloads. Report-only, and checked every sweep
	// whether or not any pod is failing — the point is to hear it from the node
	// before the pods start falling over.
	a.checkNodes(ctx, pods)

	// Verify earlier heals against the same live problem set, whether or not
	// there is anything new this cycle.
	a.verifyHeals(ctx, problems)

	// Close and announce incidents that have gone quiet. Their stored heal
	// proposal is dropped with them, so a future recurrence is diagnosed afresh.
	for _, inc := range a.incidents.Reap(now) {
		a.notifyIncidentResolved(ctx, inc)
	}
	metrics.IncidentsOpen.Set(float64(a.incidents.OpenCount()))
	if a.breaker != nil {
		metrics.BreakerOpen.Set(float64(a.breaker.OpenCount(now)))
	}
	if a.playbook != nil {
		a.retirePlaybook(ctx)
		metrics.PlaybookEntries.Set(float64(a.playbook.Count()))
		metrics.PlaybookQuarantined.Set(float64(a.playbook.QuarantineCount(now)))
	}

	// Persist the incident set (and its heal proposals) so a restart does not
	// re-alert open incidents or lose a pending retry. Only writes when changed.
	a.persistIncidents(ctx)
	a.persistPlaybook(ctx)
	a.persistRightsize(ctx)

	// Last, so the summary describes a sweep that has fully finished.
	a.maybeDigest(ctx)

	if len(fresh) == 0 {
		return
	}
	if a.cfg.MaxAlertsPerCycle > 0 && len(fresh) > a.cfg.MaxAlertsPerCycle {
		a.log.Warn("capping alerts for this cycle",
			"found", len(fresh), "cap", a.cfg.MaxAlertsPerCycle)
		fresh = fresh[:a.cfg.MaxAlertsPerCycle]
	}

	a.handleAll(ctx, fresh, st)
}

// predictProblems runs the memory-pressure predictor over live usage and returns
// MemoryPressure problems for containers trending toward an OOM. It excludes any
// container that already has a real problem this sweep, so an actual failure is
// never shadowed by a prediction. Best-effort: a metrics API error yields no
// predictions (and resets streaks) rather than failing the sweep.
func (a *Agent) predictProblems(st *sweepState, existing []detect.Problem) []detect.Problem {
	if a.predictor == nil {
		return nil
	}
	samples := make([]predict.Sample, 0, len(st.usage))
	for _, m := range st.usage {
		samples = append(samples, predict.Sample{
			Namespace: m.Namespace, Pod: m.Pod, Container: m.Container,
			UsageBytes: m.UsageBytes, LimitBytes: m.LimitBytes,
			CPUMilli: m.CPUMilli, CPULimit: m.CPULimit,
		})
	}

	predicted := a.predictor.Observe(samples, time.Now())
	metrics.PredictedPressure.Set(float64(a.predictor.Tracking()))
	if len(predicted) == 0 {
		return nil
	}

	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Namespace+"/"+p.Pod+"/"+p.Container] = true
	}
	var kept []detect.Problem
	for _, p := range predicted {
		if have[p.Namespace+"/"+p.Pod+"/"+p.Container] {
			continue // real problem already covers this container
		}
		kept = append(kept, p)
	}
	metrics.PredictionsTotal.Add(uint64(len(kept)))
	if len(kept) > 0 {
		a.log.Info("predicted memory pressure", "containers", len(kept))
	}
	return kept
}

// notifyIncidentUpdate announces a new symptom on an already-alerted incident,
// without spending another LLM call.
func (a *Agent) notifyIncidentUpdate(ctx context.Context, inc *incident.Incident, newKind detect.Kind) {
	metrics.IncidentUpdates.Inc()
	msg := fmt.Sprintf("Update on %s/%s (container %s): now also %s — same incident, not a new one.",
		inc.Namespace, inc.Pod, inc.Container, newKind)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("incident update notice failed", "err", err)
	}
}

// notifyIncidentResolved announces that a workload's incident has cleared.
func (a *Agent) notifyIncidentResolved(ctx context.Context, inc *incident.Incident) {
	metrics.IncidentsResolved.Inc()
	a.tally.IncidentResolved()
	a.log.Info("incident resolved", "pod", inc.Namespace+"/"+inc.Pod, "container", inc.Container)
	msg := fmt.Sprintf("Resolved: %s/%s (container %s) — no longer failing after %s.",
		inc.Namespace, inc.Pod, inc.Container, inc.LastSeen.Sub(inc.FirstSeen).Round(time.Second))
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("incident resolved notice failed", "err", err)
	}
}

func (a *Agent) handleAll(ctx context.Context, incidents []*incident.Incident, st *sweepState) {
	concurrency := a.cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, inc := range incidents {
		wg.Add(1)
		go func(inc *incident.Incident) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			a.handle(ctx, inc, st)
		}(inc)
	}
	wg.Wait()
}

// handle collects evidence for one problem, diagnoses it, and delivers the
// result. Failures are logged rather than propagated: one bad pod should not
// stop the sweep.
func (a *Agent) handle(ctx context.Context, inc *incident.Incident, st *sweepState) {
	p := inc.Trigger
	log := a.log.With("pod", p.Namespace+"/"+p.Pod, "kind", string(p.Kind))

	bundle, err := a.kube.Collect(ctx, p, k8s.CollectOptions{
		LogTailLines: a.cfg.LogTailLines,
		MaxEvents:    a.cfg.MaxEvents,
	})
	if err != nil {
		log.Error("collect evidence failed", "err", err)
		return
	}
	// Cluster headroom and workload load, so a scale-up is sized from measurement
	// and bounded by space that actually exists.
	a.enrich(ctx, bundle, st)

	// Playbook first: if this workload+kind has a remedy that already held, replay
	// it — no LLM diagnosis. Falls through to the model when there is no entry or
	// the remembered fix no longer validates against the current state.
	if a.tryPlaybook(ctx, log, st, inc, bundle) {
		return
	}

	a.emit(live.ClassDiagnose, p, string(p.Kind), "collecting evidence and asking the model")

	start := time.Now()
	diagnosis, err := a.brain.Diagnose(ctx, bundle)
	metrics.LLMLatency.Observe(time.Since(start).Seconds(), a.cfg.Provider)
	if err != nil {
		metrics.LLMRequests.Inc(a.cfg.Provider, "error")
		log.Error("diagnose failed", "err", err)
		return
	}
	metrics.LLMRequests.Inc(a.cfg.Provider, "ok")
	a.recordUsage(log, diagnosis.Usage)

	healResult := a.maybeHeal(ctx, log, st, bundle, string(p.Kind), diagnosis.Action, diagnosis.Confidence)
	// Remember the proposal so a later Suppressed sighting can retry a heal that
	// was declined or failed this time — without a second LLM call.
	a.rememberHeal(inc.Key, diagnosis.Action, diagnosis.Confidence, healResult)

	if err := a.notifier.Notify(ctx, notify.Alert{Problem: p, Diagnosis: diagnosis, Heal: healResult, CorrelatedKinds: inc.OtherKinds()}); err != nil {
		metrics.AlertsTotal.Inc("failed")
		log.Error("notify failed", "err", err)
		return
	}
	metrics.AlertsTotal.Inc("delivered")

	log.Info("alert delivered", "severity", diagnosis.Severity, "title", diagnosis.Title)
}

// recordUsage meters token consumption and, when per-token prices are
// configured, the estimated dollar cost. Cache-read tokens are priced at the
// input rate (a slight over-estimate — Claude bills them cheaper), while the
// per-kind token counters stay exact so a precise cost can be derived in PromQL.
func (a *Agent) recordUsage(log *slog.Logger, u *llm.Usage) {
	if u == nil {
		return
	}
	p := a.cfg.Provider
	if u.InputTokens > 0 {
		metrics.LLMTokens.Add(uint64(u.InputTokens), p, "input")
	}
	if u.OutputTokens > 0 {
		metrics.LLMTokens.Add(uint64(u.OutputTokens), p, "output")
	}
	if u.CacheReadTokens > 0 {
		metrics.LLMTokens.Add(uint64(u.CacheReadTokens), p, "cache_read")
	}
	cost := (float64(u.InputTokens+u.CacheReadTokens)*a.cfg.PriceInputPerMTok +
		float64(u.OutputTokens)*a.cfg.PriceOutputPerMTok) / 1e6
	if cost > 0 {
		metrics.LLMCost.Add(cost, p)
	}
	// Tallied here rather than at the request site so the digest's call count
	// and its cost figure can never disagree.
	a.tally.LLM(cost)
	log.Info("llm usage",
		"input", u.InputTokens, "output", u.OutputTokens, "cache_read", u.CacheReadTokens, "cost_usd", cost)
}

// maybeHeal validates the model's proposed action and, if it clears every
// safety check, executes it (or dry-runs it). It returns a result for the alert
// footer, or nil when auto-heal is disabled. It never returns an error: a heal
// failure is reported alongside the diagnosis, not in place of it.
func (a *Agent) maybeHeal(ctx context.Context, log *slog.Logger, st *sweepState, bundle *k8s.Bundle, kind string, action heal.Action, confidence string) *notify.HealResult {
	if a.healer == nil {
		return nil
	}
	res := &notify.HealResult{Attempted: true}

	// Cluster-wide brakes, checked before anything else. A systemic outage is not
	// something to heal one workload at a time, and even a healthy sweep should
	// not rewrite the cluster faster than a human can follow.
	if st != nil && st.suspended != "" {
		res.Skipped = "healing suspended cluster-wide: " + st.suspended
		metrics.HealsTotal.Inc("surge_suspended")
		log.Warn("auto-heal skipped", "reason", "cluster-wide surge")
		return res
	}
	if st != nil && !st.budget.Take() {
		res.Skipped = fmt.Sprintf("this sweep's heal allowance of %d is spent; deferred to the next sweep", st.budget.Max())
		metrics.HealsTotal.Inc("budget_spent")
		log.Info("auto-heal deferred", "reason", "per-sweep budget exhausted", "max", st.budget.Max())
		return res
	}

	plan, err := heal.Validate(bundle, confidence, action, a.healOpts)
	if err != nil {
		// A declined proposal is the normal case, not an error condition.
		res.Skipped = err.Error()
		metrics.HealsTotal.Inc("skipped")
		a.tally.Heal("skipped")
		log.Info("auto-heal skipped", "reason", err)
		a.emit(live.ClassDeclined, bundle.Problem, "declined", err.Error())
		return res
	}

	// Circuit breaker: a workload whose heals keep failing is one we cannot fix;
	// suspend it (keyed by controller so it spans pod churn) rather than churn the
	// cluster. Checked before the cooldown so an open breaker does not consume it.
	ctrlKey := ""
	if a.breaker != nil {
		if ctrl, err := a.kube.ResolveController(ctx, plan.Namespace, plan.Pod); err == nil {
			ctrlKey = heal.ControllerKeyFor(ctrl)
		}
		if ctrlKey != "" && !a.breaker.Allowed(ctrlKey, time.Now()) {
			res.Skipped = "heal circuit breaker open for this workload (repeated heal failures) — manual review needed"
			metrics.HealsTotal.Inc("breaker_open")
			log.Warn("auto-heal skipped", "reason", "circuit breaker open", "controller", ctrlKey)
			return res
		}
	}

	// Cooldown: do not re-heal the same workload on every poll while it settles.
	if !a.healSeen.ShouldAlert(plan.WorkloadKey()) {
		res.Skipped = "within heal cooldown for this workload"
		metrics.HealsTotal.Inc("skipped")
		a.tally.Heal("skipped")
		return res
	}

	outcome, err := a.healer.Execute(ctx, plan)
	if err != nil {
		// A GitOps-managed workload is a deliberate skip, not a failure: the fix
		// belongs in the source repository.
		if errors.Is(err, heal.ErrGitOpsManaged) {
			res.Skipped = err.Error()
			metrics.HealsTotal.Inc("skipped")
			a.tally.Heal("skipped")
			log.Info("auto-heal skipped", "reason", err)
			return res
		}
		res.Error = err.Error()
		metrics.HealsTotal.Inc("failed")
		a.tally.Heal("failed")
		log.Error("auto-heal failed", "err", err)
		return res
	}
	if outcome.Applied {
		metrics.HealsTotal.Inc("applied")
		a.tally.Heal("applied")
	} else {
		metrics.HealsTotal.Inc("dryrun")
		a.tally.Heal("dryrun")
	}

	res.Applied = outcome.Applied
	res.Controller = outcome.Controller
	res.Summary = outcome.Summary
	log.Info("auto-heal done", "applied", outcome.Applied, "controller", outcome.Controller, "change", outcome.Summary)
	healReason := "dry run"
	if outcome.Applied {
		healReason = "applied"
	}
	a.emit(live.ClassHeal, bundle.Problem, healReason, outcome.Summary)

	auditOutcome := "dryrun"
	if outcome.Applied {
		auditOutcome = "applied"
	}
	a.recordAudit(ctx, log, auditEventFromPlan(plan, findContainer(bundle, plan.Container), outcome.Ref, auditOutcome, time.Now()))

	// Count the applied heal toward flap detection; a workload healed too often
	// trips the breaker even without a rollback.
	if a.breaker != nil && ctrlKey != "" && outcome.Applied {
		if a.breaker.OnHeal(ctrlKey, time.Now()) {
			a.tripBreaker(ctx, log, ctrlKey, "healed too many times in the breaker window")
		}
	}

	// Persist a real patch (resources or image) for later verification. Dry runs
	// and restarts have nothing to verify or roll back.
	if outcome.Applied && (plan.Kind == heal.ActionPatchResources || plan.Kind == heal.ActionPatchImage || plan.Kind == heal.ActionPatchProbe || plan.Kind == heal.ActionScaleReplicas) && a.healStore != nil {
		rec := heal.RecordFromPlan(plan, outcome.Ref, findContainer(bundle, plan.Container), time.Now(), a.healVerifyAfter)
		rec.Kind = kind // the detected problem kind, used as the playbook key
		if plan.Kind == heal.ActionScaleReplicas {
			rec.OldReplicas = bundle.Replicas // pre-scale count, for rollback
		}
		// Carry the raw action + confidence so a heal that later verifies can be
		// learned into the playbook and replayed without an LLM diagnosis.
		if blob, err := json.Marshal(action); err == nil {
			rec.ActionJSON = string(blob)
		}
		rec.Confidence = confidence
		if err := a.healStore.Save(ctx, rec); err != nil {
			log.Error("auto-heal: persist verification record failed", "err", err)
		}
	}
	return res
}

// rememberHeal stores an incident's validated proposal on the incident (as
// opaque JSON) so a later sweep — or a restart, since the incident is persisted
// — can re-attempt the heal without a fresh diagnosis. Healed is set once a real
// patch has been applied, which stops further retries. A dry run leaves it unset:
// nothing changed on the cluster.
func (a *Agent) rememberHeal(key string, action heal.Action, confidence string, res *notify.HealResult) {
	blob, err := json.Marshal(action)
	if err != nil {
		a.log.Error("remember heal: marshal action failed", "err", err)
		return
	}
	a.incidents.SetHealProposal(key, string(blob), confidence)
	if res != nil && res.Applied {
		a.incidents.MarkHealed(key)
	}
}

// maybeRetryHeal re-attempts a remembered heal for a still-open incident that
// has not yet been healed. It collects a fresh evidence bundle and re-runs the
// pure validator against the current cluster state — so a proposal that was
// declined earlier (a GitOps label since removed, a request that no longer
// strands the pod) can now succeed — with no additional LLM call. The heal
// cooldown inside maybeHeal keeps this from firing every sweep.
func (a *Agent) maybeRetryHeal(ctx context.Context, inc *incident.Incident, st *sweepState) {
	if a.healer == nil {
		return
	}
	blob, confidence, healed, ok := a.incidents.HealProposal(inc.Key)
	if !ok || healed || blob == "" {
		return
	}
	var action heal.Action
	if err := json.Unmarshal([]byte(blob), &action); err != nil {
		a.log.Error("heal retry: unmarshal stored action failed", "err", err)
		return
	}

	p := inc.Trigger
	log := a.log.With("pod", p.Namespace+"/"+p.Pod, "kind", string(p.Kind), "retry", true)
	bundle, err := a.kube.Collect(ctx, p, k8s.CollectOptions{
		LogTailLines: a.cfg.LogTailLines,
		MaxEvents:    a.cfg.MaxEvents,
	})
	if err != nil {
		log.Error("heal retry: collect evidence failed", "err", err)
		return
	}
	// A retry is re-validated against the *current* cluster, so it needs this
	// sweep's capacity, not the snapshot the original proposal was judged on.
	a.enrich(ctx, bundle, st)

	res := a.maybeHeal(ctx, log, st, bundle, string(p.Kind), action, confidence)
	if res == nil || !res.Applied {
		return
	}
	a.rememberHeal(inc.Key, action, confidence, res)
	log.Info("auto-heal succeeded on retry", "controller", res.Controller, "change", res.Summary)
	msg := fmt.Sprintf("Auto-heal of %s/%s (container %s) succeeded on retry: %s.",
		inc.Namespace, inc.Pod, inc.Container, res.Summary)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		log.Error("heal retry: notice failed", "err", err)
	}
}

// The ConfigMap data keys holding the agent's own cross-restart bookkeeping.
// Two keys in one object rather than two objects: the digest cursor is a single
// timestamp, and a whole ConfigMap (plus its RBAC) for one field would be
// ceremony without benefit.
const (
	incidentStateKey = "incidents.json"
	digestStateKey   = "digest.json"
)

// digestState is the digest's cursor, persisted so a restart does not re-send
// today's summary or silently skip tomorrow's.
type digestState struct {
	LastSent time.Time `json:"lastSent"`
}

// loadIncidents restores the incident set persisted by a previous run. A missing
// ConfigMap or a decode error is non-fatal: the agent simply starts with no open
// incidents (the pre-persistence behaviour).
func (a *Agent) loadIncidents(ctx context.Context) {
	if a.incidentStateCM == "" {
		return
	}
	data, err := a.kube.GetConfigMap(ctx, k8s.Namespace(), a.incidentStateCM)
	if err != nil {
		a.log.Error("incident state: load failed", "err", err)
		return
	}
	if blob := data[digestStateKey]; blob != "" && a.digestOn {
		var ds digestState
		if err := json.Unmarshal([]byte(blob), &ds); err != nil {
			a.log.Error("digest state: decode failed", "err", err)
		} else if !ds.LastSent.IsZero() {
			// Restoring an *older* cursor than the seeded start time is the whole
			// point: it is what lets a restart notice that today's digest has not
			// gone out yet.
			a.digestLast = ds.LastSent
			a.log.Info("digest cursor restored", "lastSent", ds.LastSent)
		}
	}
	blob := data[incidentStateKey]
	if blob == "" {
		return
	}
	var list []incident.Incident
	if err := json.Unmarshal([]byte(blob), &list); err != nil {
		a.log.Error("incident state: decode failed", "err", err)
		return
	}
	a.incidents.Restore(list)
	a.log.Info("incident state restored", "incidents", len(list))
}

// persistIncidents writes the incident set and the digest cursor to their
// ConfigMap, but only when something has changed since the last write. A write
// failure is logged and the dirty flags are left set, so the next sweep retries.
func (a *Agent) persistIncidents(ctx context.Context) {
	if a.incidentStateCM == "" || (!a.incidents.Dirty() && !a.digestDirty) {
		return
	}
	blob, err := json.Marshal(a.incidents.Snapshot())
	if err != nil {
		a.log.Error("incident state: encode failed", "err", err)
		return
	}
	data := map[string]string{incidentStateKey: string(blob)}
	// PutConfigMap replaces the whole object, so the digest cursor has to be
	// rewritten every time or it would be erased by the next incident write.
	if a.digestOn {
		cursor, err := json.Marshal(digestState{LastSent: a.digestLast})
		if err != nil {
			a.log.Error("digest state: encode failed", "err", err)
		} else {
			data[digestStateKey] = string(cursor)
		}
	}
	if err := a.kube.PutConfigMap(ctx, k8s.Namespace(), a.incidentStateCM, data); err != nil {
		a.log.Error("incident state: save failed", "err", err)
		return
	}
	a.incidents.ClearDirty()
	a.digestDirty = false
}

// playbookStateKey is the single ConfigMap data key holding the playbook.
const playbookStateKey = "playbook.json"

// loadPlaybook restores learned remedies from a previous run.
func (a *Agent) loadPlaybook(ctx context.Context) {
	if a.playbook == nil || a.playbookCM == "" {
		return
	}
	data, err := a.kube.GetConfigMap(ctx, k8s.Namespace(), a.playbookCM)
	if err != nil {
		a.log.Error("playbook: load failed", "err", err)
		return
	}
	blob := data[playbookStateKey]
	if blob == "" {
		return
	}
	state, err := decodePlaybook(blob)
	if err != nil {
		a.log.Error("playbook: decode failed", "err", err)
		return
	}
	a.playbook.Restore(state)
	a.log.Info("playbook restored", "remedies", len(state.Entries), "scars", len(state.Scars))
}

// decodePlaybook reads the stored book, accepting both the current object form
// and the bare array written before failure history existed. A first run after
// an upgrade must not throw away everything the cluster has learned, and the
// two are unambiguous: JSON arrays start with '[', objects with '{'.
func decodePlaybook(blob string) (playbook.State, error) {
	trimmed := strings.TrimSpace(blob)
	if strings.HasPrefix(trimmed, "[") {
		var list []playbook.Entry
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return playbook.State{}, err
		}
		return playbook.State{Entries: list}, nil
	}
	var state playbook.State
	if err := json.Unmarshal([]byte(trimmed), &state); err != nil {
		return playbook.State{}, err
	}
	return state, nil
}

// persistPlaybook writes the playbook to its ConfigMap when it has changed.
func (a *Agent) persistPlaybook(ctx context.Context) {
	if a.playbook == nil || a.playbookCM == "" || !a.playbook.Dirty() {
		return
	}
	blob, err := json.Marshal(a.playbook.State())
	if err != nil {
		a.log.Error("playbook: encode failed", "err", err)
		return
	}
	if err := a.kube.PutConfigMap(ctx, k8s.Namespace(), a.playbookCM, map[string]string{playbookStateKey: string(blob)}); err != nil {
		a.log.Error("playbook: save failed", "err", err)
		return
	}
	a.playbook.ClearDirty()
}

// tryPlaybook replays a remembered, previously-verified fix for this workload and
// problem kind, skipping the LLM entirely. It returns true when it handled the
// incident (a remedy applied and an alert was sent). It returns false — falling
// back to the model — when there is no entry, the entry cannot be decoded, or the
// remembered fix no longer applies (state changed, cooldown, or breaker open).
func (a *Agent) tryPlaybook(ctx context.Context, log *slog.Logger, st *sweepState, inc *incident.Incident, bundle *k8s.Bundle) bool {
	if a.playbook == nil || a.healer == nil {
		return false
	}
	ctrl, err := a.kube.ResolveController(ctx, inc.Namespace, inc.Pod)
	if err != nil {
		return false // bare pod or non-healable owner: nothing to replay
	}
	ctrlKey := heal.ControllerKeyFor(ctrl)
	kind := string(inc.Trigger.Kind)
	entry, ok := a.playbook.Lookup(ctrlKey, kind)
	if !ok {
		return false
	}

	var action heal.Action
	if err := json.Unmarshal([]byte(entry.ActionJSON), &action); err != nil {
		log.Error("playbook: decode remedy failed, evicting", "err", err)
		if a.playbook.Evict(ctrlKey, kind) {
			metrics.PlaybookEvictionsTotal.Inc()
		}
		return false
	}

	plog := log.With("source", "playbook")
	res := a.maybeHeal(ctx, plog, st, bundle, kind, action, entry.Confidence)
	if res == nil || !res.Applied {
		// The remembered fix did not apply this time — let the model handle it.
		return false
	}

	a.playbook.MarkHit(ctrlKey, kind, time.Now())
	metrics.PlaybookHitsTotal.Inc()
	a.tally.PlaybookHit()
	a.rememberHeal(inc.Key, action, entry.Confidence, res)

	diag := &llm.Diagnosis{
		Title:      fmt.Sprintf("Recurring %s on %s/%s — healed from playbook", kind, ctrl.Kind, ctrl.Name),
		Severity:   "warning",
		Summary:    fmt.Sprintf("A previously verified fix for this workload was replayed without an LLM diagnosis (%d prior replay(s)): %s", entry.Hits, res.Summary),
		Confidence: entry.Confidence,
		Action:     action,
	}
	if err := a.notifier.Notify(ctx, notify.Alert{Problem: inc.Trigger, Diagnosis: diag, Heal: res, CorrelatedKinds: inc.OtherKinds()}); err != nil {
		metrics.AlertsTotal.Inc("failed")
		plog.Error("notify failed", "err", err)
		return true
	}
	metrics.AlertsTotal.Inc("delivered")
	plog.Info("healed from playbook", "controller", ctrlKey, "kind", kind, "priorHits", entry.Hits, "change", res.Summary)
	return true
}

// tripBreaker records a breaker trip and tells a human, exactly once per trip.
// From here on the workload is skipped until the breaker's open window elapses.
func (a *Agent) tripBreaker(ctx context.Context, log *slog.Logger, ctrlKey, why string) {
	metrics.BreakerTripsTotal.Inc()
	log.Warn("heal circuit breaker tripped", "controller", ctrlKey, "why", why)
	msg := fmt.Sprintf("Heal circuit breaker OPEN for %s: %s. Auto-heal for this workload is suspended until it settles — manual review needed.", ctrlKey, why)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		log.Error("breaker: notice failed", "err", err)
	}
}

// quarantineNotice tells a human that podsmedic has stopped trying to learn a
// fix for this workload. It is not the same message as a breaker trip: the
// breaker suspends *healing*, this only suspends *remembering*, and the
// distinction is what an operator needs to know before going looking.
func (a *Agent) quarantineNotice(ctx context.Context, ctrlKey, kind string) {
	until, ok := a.playbook.Quarantined(ctrlKey, kind, time.Now())
	if !ok {
		return
	}
	a.log.Warn("playbook: quarantined", "controller", ctrlKey, "kind", kind, "until", until)
	msg := fmt.Sprintf("Playbook quarantine for %s (%s): remedies here keep being rolled back, so I will stop "+
		"remembering fixes for this workload until %s and diagnose it fresh each time. The root cause is probably "+
		"not something a resource change can fix.",
		ctrlKey, kind, until.UTC().Format("2006-01-02 15:04 UTC"))
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("playbook: quarantine notice failed", "err", err)
	}
}

// retirePlaybook drops remedies nothing has confirmed for a long time. They did
// not fail — they simply stopped being proven, and replaying an unproven fix is
// how podsmedic would end up treating a cluster that no longer exists.
func (a *Agent) retirePlaybook(ctx context.Context) {
	if a.playbook == nil {
		return
	}
	retired := a.playbook.Retire(time.Now())
	if len(retired) == 0 {
		return
	}
	metrics.PlaybookRetirementsTotal.Add(uint64(len(retired)))
	a.tally.PlaybookRetired(len(retired))
	for _, e := range retired {
		a.log.Info("playbook: retired unconfirmed remedy",
			"controller", e.Controller, "kind", e.Kind, "lastVerified", e.LastVerified)
	}
	msg := fmt.Sprintf("Retired %d playbook remed%s unconfirmed for over %s (oldest: %s / %s). "+
		"Nothing failed — they just stopped being proven, so the next occurrence gets a fresh diagnosis.",
		len(retired), plural(len(retired), "y", "ies"), a.cfg.PlaybookMaxAge,
		retired[0].Controller, retired[0].Kind)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("playbook: retirement notice failed", "err", err)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// recordAudit appends a heal event to the durable trail. A trail write never
// blocks or fails a heal: a persistence error is logged and dropped.
func (a *Agent) recordAudit(ctx context.Context, log *slog.Logger, e audit.Event) {
	if err := a.audit.Append(ctx, e); err != nil {
		log.Error("audit trail append failed", "err", err)
	}
}

// auditEventFromPlan builds a trail entry for an applied or dry-run heal from the
// executed plan and the pre-change container state (nil for a restart, which
// changes no values).
func auditEventFromPlan(p *heal.Plan, before *k8s.ContainerSummary, ctrl k8s.ControllerRef, outcome string, now time.Time) audit.Event {
	e := audit.Event{
		Time:       now,
		Namespace:  ctrl.Namespace,
		Controller: ctrl.Kind + "/" + ctrl.Name,
		Container:  p.Container,
		Action:     string(p.Kind),
		Outcome:    outcome,
		Summary:    p.Summary,
	}
	switch p.Kind {
	case heal.ActionRestartWorkload:
		// A restart changes no values, so there is nothing to record.
	case heal.ActionCreatePVC:
		// A creation has no "old" side. Recording the claim explicitly matters
		// more here than elsewhere: this is the one thing podsmedic makes that it
		// will never unmake, so the trail is the only record of what appeared.
		if p.Claim != nil {
			e.New = map[string]string{
				"pvc":          p.Claim.Name,
				"size":         p.Claim.Size,
				"accessMode":   p.Claim.AccessMode,
				"storageClass": p.Claim.StorageClass,
			}
			if p.Claim.StorageClass == "" {
				e.New["storageClass"] = "(cluster default)"
			}
		}
	default:
		rec := heal.RecordFromPlan(p, ctrl, before, now, 0)
		e.Old, e.New = auditChangedValues(rec)
	}
	return e
}

// auditEventFromRecord builds a trail entry for a verified or rolled-back heal
// from its persisted record, which already holds the before/after values.
func auditEventFromRecord(rec heal.HealRecord, outcome string, now time.Time) audit.Event {
	e := audit.Event{
		Time:       now,
		Namespace:  rec.Namespace,
		Controller: rec.ControllerKind + "/" + rec.ControllerName,
		Container:  rec.Container,
		Action:     auditActionForRecord(rec),
		Outcome:    outcome,
		Summary:    rec.Summary,
	}
	e.Old, e.New = auditChangedValues(rec)
	return e
}

// auditActionForRecord infers the heal action from which fields a record carries.
func auditActionForRecord(rec heal.HealRecord) string {
	switch {
	case rec.NewImage != "":
		return string(heal.ActionPatchImage)
	case rec.ProbeType != "":
		return string(heal.ActionPatchProbe)
	case rec.NewReplicas > 0:
		return string(heal.ActionScaleReplicas)
	default:
		return string(heal.ActionPatchResources)
	}
}

// auditChangedValues flattens a record's before/after values into two string
// maps keyed by a short, self-describing field name.
func auditChangedValues(rec heal.HealRecord) (old, latest map[string]string) {
	old, latest = map[string]string{}, map[string]string{}
	for k, v := range rec.OldLimits {
		old["limit."+k] = v
	}
	for k, v := range rec.NewLimits {
		latest["limit."+k] = v
	}
	for k, v := range rec.OldRequests {
		old["request."+k] = v
	}
	for k, v := range rec.NewRequests {
		latest["request."+k] = v
	}
	if rec.OldImage != "" {
		old["image"] = rec.OldImage
	}
	if rec.NewImage != "" {
		latest["image"] = rec.NewImage
	}
	for k, v := range rec.OldProbe {
		old[k] = strconv.Itoa(int(v))
	}
	for k, v := range rec.NewProbe {
		latest[k] = strconv.Itoa(int(v))
	}
	if rec.OldReplicas > 0 {
		old["replicas"] = strconv.Itoa(int(rec.OldReplicas))
	}
	if rec.NewReplicas > 0 {
		latest["replicas"] = strconv.Itoa(int(rec.NewReplicas))
	}
	if len(old) == 0 {
		old = nil
	}
	if len(latest) == 0 {
		latest = nil
	}
	return old, latest
}

// findContainer returns the pre-heal state of the named container from the
// evidence bundle, so a rollback can restore its exact prior resource values.
func findContainer(b *k8s.Bundle, name string) *k8s.ContainerSummary {
	for i := range b.Pod.Containers {
		if b.Pod.Containers[i].Name == name {
			return &b.Pod.Containers[i]
		}
	}
	return nil
}

// verifyHeals re-checks applied heals whose grace window has elapsed. A workload
// that still has an open problem is rolled back to its recorded prior values; a
// recovered workload retires its record. Cross-referencing the sweep's live
// problem set costs one controller resolution per current problem, only when
// something is actually due.
func (a *Agent) verifyHeals(ctx context.Context, problems []detect.Problem) {
	if a.healStore == nil {
		return
	}
	recs, err := a.healStore.List(ctx)
	if err != nil {
		a.log.Error("heal verify: load state failed", "err", err)
		return
	}

	now := time.Now()
	var due []heal.HealRecord
	for _, r := range recs {
		if !now.Before(r.VerifyAfter) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return
	}

	unhealthy := a.unhealthyControllers(ctx, problems)

	for _, rec := range due {
		stillFailing := unhealthy[rec.ControllerKey()]
		switch heal.VerifyVerdict(rec, now, stillFailing) {
		case heal.VerdictHealthy:
			metrics.VerificationsTotal.Inc("verified")
			a.tally.Verification("verified")
			a.log.Info("heal verified", "controller", rec.ControllerKey(), "change", rec.Summary)
			a.emitRecord(live.ClassVerify, rec, "verified", "the change held")
			a.recordAudit(ctx, a.log, auditEventFromRecord(rec, "verified", now))
			// Learn the remedy: this fix held, so remember it for instant replay.
			// A quarantined workload is the exception — its fixes have not held
			// often enough to be worth replaying, and Record says so.
			if a.playbook != nil && rec.ActionJSON != "" {
				if a.playbook.Record(rec.ControllerKey(), rec.Kind, rec.ActionJSON, rec.Confidence, now) {
					metrics.PlaybookRecordsTotal.Inc()
					a.tally.PlaybookLearned()
				} else {
					a.log.Info("playbook: heal held but the workload is quarantined, not learning it",
						"controller", rec.ControllerKey(), "kind", rec.Kind)
				}
			}
			msg := fmt.Sprintf("Verified heal of %s (container %s): %s — workload healthy, no recurrence.",
				rec.ControllerKind+"/"+rec.ControllerName+" in "+rec.Namespace, rec.Container, rec.Summary)
			if err := a.notifier.Notice(ctx, msg); err != nil {
				a.log.Error("heal verify: notice failed", "err", err)
			}
			if err := a.healStore.Delete(ctx, rec.ControllerKey()); err != nil {
				a.log.Error("heal verify: delete record failed", "err", err)
			}
		case heal.VerdictRollback:
			a.rollback(ctx, rec)
		}
	}
}

// rollback undoes a heal that failed verification and reports it. The record is
// retired only on a successful rollback, so a transient patch error is retried
// next sweep rather than silently abandoned.
func (a *Agent) rollback(ctx context.Context, rec heal.HealRecord) {
	ctrl := rec.ControllerKind + "/" + rec.ControllerName + " in " + rec.Namespace
	a.log.Warn("heal did not hold, rolling back", "controller", rec.ControllerKey(), "change", rec.Summary)

	// A fix that no longer holds must not stay in the playbook. Repeat offenders
	// are additionally quarantined, so podsmedic stops re-learning and
	// re-applying a remedy this workload keeps rejecting.
	if a.playbook != nil {
		removed, quarantined := a.playbook.Fail(rec.ControllerKey(), rec.Kind, time.Now())
		if removed {
			metrics.PlaybookEvictionsTotal.Inc()
			a.log.Info("playbook: dropped remedy that stopped holding", "controller", rec.ControllerKey(), "kind", rec.Kind)
		}
		if quarantined {
			metrics.PlaybookQuarantinesTotal.Inc()
			a.tally.PlaybookQuarantined()
			a.quarantineNotice(ctx, rec.ControllerKey(), rec.Kind)
		}
	}

	metrics.VerificationsTotal.Inc("rolledback")
	a.tally.Verification("rolledback")
	// A rollback means the heal did not hold — the strongest breaker signal.
	if a.breaker != nil {
		if a.breaker.OnRollback(rec.ControllerKey(), time.Now()) {
			a.tripBreaker(ctx, a.log, rec.ControllerKey(), "too many heal rollbacks in the breaker window")
		}
	}
	if err := a.healer.Rollback(ctx, rec); err != nil {
		metrics.RollbacksTotal.Inc("failed")
		a.recordAudit(ctx, a.log, auditEventFromRecord(rec, "rollback_failed", time.Now()))
		a.log.Error("heal rollback failed", "controller", rec.ControllerKey(), "err", err)
		if nerr := a.notifier.Notice(ctx, fmt.Sprintf("Rollback of %s FAILED after the heal did not hold: %v. Manual review needed.", ctrl, err)); nerr != nil {
			a.log.Error("heal verify: notice failed", "err", nerr)
		}
		return
	}

	metrics.RollbacksTotal.Inc("ok")
	a.emitRecord(live.ClassRollback, rec, "rolled back", "the change did not hold; prior values restored")
	a.recordAudit(ctx, a.log, auditEventFromRecord(rec, "rolledback", time.Now()))
	msg := fmt.Sprintf("Rolled back heal of %s (container %s): the workload was still failing after the change, so its prior resource values were restored. Manual review needed.", ctrl, rec.Container)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("heal verify: notice failed", "err", err)
	}
	if err := a.healStore.Delete(ctx, rec.ControllerKey()); err != nil {
		a.log.Error("heal verify: delete record failed", "err", err)
	}
}

// unhealthyControllers maps the current sweep's problems to the set of workload
// controllers that own them, so a persisted heal can be matched to its
// workload's current health regardless of pod churn. Predictive problems
// (MemoryPressure, CPUPressure) are excluded: they are forecasts, not confirmed
// failures, and a prediction that lingers (an old pod mid-rollout, or CPU that
// stays high after a scale) must never be read as "the heal did not hold" and
// trigger a rollback.
func (a *Agent) unhealthyControllers(ctx context.Context, problems []detect.Problem) map[string]bool {
	set := map[string]bool{}
	seen := map[string]bool{}
	for _, p := range problems {
		if p.Kind.Predictive() {
			continue
		}
		podKey := p.Namespace + "/" + p.Pod
		if seen[podKey] {
			continue
		}
		seen[podKey] = true
		ctrl, err := a.kube.ResolveController(ctx, p.Namespace, p.Pod)
		if err != nil {
			continue // bare pod or non-healable owner: nothing we healed
		}
		set[heal.ControllerKeyFor(ctrl)] = true
	}
	return set
}
