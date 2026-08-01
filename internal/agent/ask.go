package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teknik-github/PodsMedic/internal/audit"
	"github.com/teknik-github/PodsMedic/internal/capacity"
	"github.com/teknik-github/PodsMedic/internal/chat"
	"github.com/teknik-github/PodsMedic/internal/detect"
	"github.com/teknik-github/PodsMedic/internal/heal"
	"github.com/teknik-github/PodsMedic/internal/k8s"
	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/metrics"

	"github.com/teknik-github/PodsMedic/internal/playbook"
	"github.com/teknik-github/PodsMedic/internal/report"
	"github.com/teknik-github/PodsMedic/internal/rightsize"
	corev1 "k8s.io/api/core/v1"
)

// This file implements the inbound half of the Telegram integration: answering
// an operator's questions from what the agent already knows.
//
// Two rules shape it. First, the chat path is strictly read-only — it reads
// state and calls the model, and there is no branch from here into heal.Execute,
// so no message can talk podsmedic into changing the cluster. Second, the cheap
// questions are answered locally: /status, /incidents, /capacity, /heals and
// /playbook are formatted from state the sweep already holds, costing no tokens,
// and only a free-form question reaches the model.

// Answer implements chat.Answerer.
func (a *Agent) Answer(ctx context.Context, q chat.Question) (chat.Reply, error) {
	switch q.Command {
	case chat.CmdHelp:
		return chat.Say(chat.HelpText), nil
	case chat.CmdStatus:
		return chat.Say(a.statusText()), nil
	case chat.CmdIncidents:
		return chat.Say(a.incidentsText()), nil
	case chat.CmdCapacity:
		return chat.Say(a.capacityText()), nil
	case chat.CmdHeals:
		return chat.Say(a.healsText(ctx)), nil
	case chat.CmdPlaybook:
		return chat.Say(a.playbookText()), nil
	case chat.CmdExport:
		return a.exportPlaybook(ctx, q.Text), nil
	case chat.CmdRightsize:
		return a.rightsizeReply(q.Text), nil
	case chat.CmdNodes:
		return chat.Say(a.nodesText()), nil
	case chat.CmdDigest:
		return chat.Say(a.digestPreview()), nil
	}
	text, err := a.askModel(ctx, q.Text)
	if err != nil {
		return chat.Reply{}, err
	}
	return chat.Say(text), nil
}

// exportPlaybook renders the learned playbook and heal history as a document to
// study. Built entirely from stored state, so it costs no tokens and can never
// describe a remedy that was not actually learned.
func (a *Agent) exportPlaybook(ctx context.Context, arg string) chat.Reply {
	if a.playbook == nil {
		return chat.Say("There is no playbook to export: it fills up only when auto-heal is enabled and a fix has passed verification.")
	}
	events, err := a.audit.List(ctx)
	if err != nil {
		// The trail is a bonus section, not the point of the document.
		a.log.Warn("export: audit trail unreadable", "err", err)
	}

	format := report.ParseFormat(arg)
	doc := report.Render(report.Input{
		Entries:     a.playbook.Snapshot(),
		Events:      events,
		GeneratedAt: time.Now(),
		Scope:       strings.Join(a.cfg.Namespaces, ", "),
		Applying:    a.cfg.AutoHeal && a.cfg.HealApply,
	}, format)

	caption := fmt.Sprintf("podsmedic playbook — %d remedy(ies), %d change(s) recorded.",
		a.playbook.Count(), len(events))
	if format == report.HTML {
		caption += " Open it in a browser and use Print → Save as PDF."
	}
	return chat.Reply{
		Text:     caption,
		Document: &chat.Document{Filename: doc.Filename, MIMEType: doc.MIMEType, Content: doc.Content},
	}
}

// rightsizeReply answers /rightsize. A bare command summarises in chat; adding
// "md" or "html" produces the full document, because a table of thirty
// containers is unreadable in a chat bubble.
func (a *Agent) rightsizeReply(arg string) chat.Reply {
	if a.rightsize == nil {
		return chat.Say("Rightsizing is off. Set PODSMEDIC_RIGHTSIZE=true to have me watch what containers actually use against what they reserve.")
	}
	findings := a.rightsize.Findings(a.rightsizeOptions(), time.Now())
	metrics.RightsizeFindings.Set(float64(len(findings)))

	if strings.TrimSpace(arg) == "" {
		return chat.Say(a.rightsizeSummary(findings))
	}
	doc := report.RenderRightsize(report.RightsizeInput{
		Findings:    findings,
		GeneratedAt: time.Now(),
		Scope:       strings.Join(a.cfg.Namespaces, ", "),
		Tracked:     a.rightsize.Tracking(),
		MinWindow:   a.cfg.RightsizeMinWindow,
		MinSamples:  a.cfg.RightsizeMinSamples,
	}, report.ParseFormat(arg))

	caption := fmt.Sprintf("podsmedic rightsizing — %d suggestion(s) across %d container(s) under observation. These are suggestions; I never apply them.",
		len(findings), a.rightsize.Tracking())
	return chat.Reply{
		Text:     caption,
		Document: &chat.Document{Filename: doc.Filename, MIMEType: doc.MIMEType, Content: doc.Content},
	}
}

// rightsizeCountShown bounds the chat summary. The document is there for the
// rest.
const rightsizeCountShown = 8

func (a *Agent) rightsizeSummary(findings []rightsize.Finding) string {
	tracked := a.rightsize.Tracking()
	if len(findings) == 0 {
		return fmt.Sprintf("No sizing suggestions yet.\n\nI am watching %d container(s). A container is only judged after %d samples over at least %s — every workload has a quiet ten minutes, and sizing one from that would be worse than saying nothing.",
			tracked, a.cfg.RightsizeMinSamples, a.cfg.RightsizeMinWindow)
	}

	var b strings.Builder
	cpu, mem := rightsize.Totals(findings)
	fmt.Fprintf(&b, "%d sizing suggestion(s) across %d container(s) watched.\n", len(findings), tracked)
	if cpu > 0 || mem > 0 {
		fmt.Fprintf(&b, "Applying every reduction would return %s CPU and %s memory to the cluster.\n",
			rightsize.FormatCPU(cpu), rightsize.FormatMem(mem))
	}
	b.WriteString("\n")

	shown := findings
	if len(shown) > rightsizeCountShown {
		shown = shown[:rightsizeCountShown]
	}
	for _, f := range shown {
		fmt.Fprintf(&b, "• %s/%s (%s) %s: %s → %s  [%s]\n",
			f.Namespace, f.Workload, f.Container, f.Resource,
			amountOf(f.Resource, f.Current), amountOf(f.Resource, f.Recommended), f.Kind)
	}
	if len(findings) > len(shown) {
		fmt.Fprintf(&b, "…and %d more.\n", len(findings)-len(shown))
	}
	b.WriteString("\nThese are suggestions — I never apply them. Automated healing only ever raises a value; lowering a request changes where a pod schedules and how early it is evicted, so that call is yours. Use /rightsize html for the full document.")
	return b.String()
}

func amountOf(r rightsize.Resource, v int64) string {
	if r == rightsize.CPU {
		return rightsize.FormatCPU(v)
	}
	return rightsize.FormatMem(v)
}

// --- Locally answered commands (no LLM call) -----------------------------

func (a *Agent) statusText() string {
	snap := a.latestSweep()
	if snap == nil {
		return "No sweep has completed yet. Ask again in a moment."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last sweep %s ago: %d pods scanned, %d problem(s) detected, %d incident(s) open.\n",
		roundAge(time.Since(snap.at)), snap.pods, len(snap.problems), a.incidents.OpenCount())

	if len(snap.problems) > 0 {
		byKind := map[detect.Kind]int{}
		for _, p := range snap.problems {
			byKind[p.Kind]++
		}
		b.WriteString("\nBy kind:\n")
		for _, k := range sortedKinds(byKind) {
			fmt.Fprintf(&b, "- %s: %d\n", k, byKind[detect.Kind(k)])
		}
	}

	b.WriteString("\n")
	switch {
	case !a.cfg.AutoHeal:
		b.WriteString("Auto-heal is off: I alert only.")
	case !a.cfg.HealApply:
		b.WriteString("Auto-heal is in dry-run: I validate fixes but change nothing.")
	default:
		fmt.Fprintf(&b, "Auto-heal is applying fixes for: %s.", strings.Join(a.cfg.HealAllowedKinds, ", "))
	}
	return b.String()
}

func (a *Agent) incidentsText() string {
	open := a.incidents.Snapshot()
	if len(open) == 0 {
		return "No open incidents."
	}
	sort.Slice(open, func(i, j int) bool { return open[i].FirstSeen.Before(open[j].FirstSeen) })

	var b strings.Builder
	fmt.Fprintf(&b, "%d open incident(s), oldest first:\n\n", len(open))
	for _, inc := range open {
		fmt.Fprintf(&b, "- %s/%s", inc.Namespace, inc.Pod)
		if inc.Container != "" {
			fmt.Fprintf(&b, " (%s)", inc.Container)
		}
		fmt.Fprintf(&b, ": %s, open %s", inc.Trigger.Kind, roundAge(time.Since(inc.FirstSeen)))
		if others := inc.OtherKinds(); len(others) > 0 {
			fmt.Fprintf(&b, ", also %s", strings.Join(others, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (a *Agent) capacityText() string {
	snap := a.latestSweep()
	if snap == nil || snap.state == nil || snap.state.capacity == nil {
		if !a.cfg.AutoHeal {
			return "Cluster capacity is only read when auto-heal is enabled (PODSMEDIC_AUTOHEAL=true)."
		}
		return "Cluster capacity is unreadable — the node or pod list was denied. Heals that would add or enlarge pods are being declined."
	}

	sum := snap.state.capacity.Summary()
	var b strings.Builder
	fmt.Fprintf(&b, "%d node(s), %d schedulable. Holding back %d%% of allocatable as reserve.\n\n",
		sum.Nodes, sum.SchedulableNodes, sum.ReservePercent)
	fmt.Fprintf(&b, "Free after reserve:\n- CPU: %dm of %dm allocatable\n- Memory: %dMi of %dMi\n- Pod slots: %d of %d\n",
		sum.CPUFreeMilli, sum.CPUAllocMilli, sum.MemFreeBytes>>20, sum.MemAllocBytes>>20, sum.PodSlotsFree, sum.PodSlotsTotal)
	if sum.LargestFreeNode != "" {
		fmt.Fprintf(&b, "\nLargest single node still free: %s\n", sum.LargestFreeNode)
	}
	b.WriteString("\nThese are requests-based figures: what the scheduler could still place, not what is idle.")
	return b.String()
}

// healsRecentLimit is how many audit entries a /heals reply shows. The trail
// holds hundreds; a chat message wants the last few.
const healsRecentLimit = 10

func (a *Agent) healsText(ctx context.Context) string {
	events, err := a.audit.List(ctx)
	if err != nil {
		return "Could not read the audit trail: " + err.Error()
	}
	if len(events) == 0 {
		if !a.cfg.AutoHeal {
			return "No heals recorded — auto-heal is off."
		}
		return "No heals recorded yet."
	}

	shown := events
	if len(shown) > healsRecentLimit {
		shown = shown[len(shown)-healsRecentLimit:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last %d of %d recorded change(s), newest first:\n\n", len(shown), len(events))
	for i := len(shown) - 1; i >= 0; i-- {
		e := shown[i]
		fmt.Fprintf(&b, "- %s ago | %s | %s/%s: %s\n",
			roundAge(time.Since(e.Time)), e.Outcome, e.Namespace, e.Controller, summaryOf(e))
	}
	return b.String()
}

// playbookEntriesShown bounds the list. The book holds up to 500; a chat reply
// wants the ones actually earning their keep, so it shows the most-replayed
// first and says how many were left out.
const playbookEntriesShown = 15

func (a *Agent) playbookText() string {
	if a.playbook == nil {
		if !a.cfg.AutoHeal {
			return "The playbook only fills up when auto-heal is enabled (PODSMEDIC_AUTOHEAL=true) — it remembers fixes that were applied and then verified."
		}
		return "The playbook is disabled (PODSMEDIC_PLAYBOOK_CONFIGMAP is empty)."
	}

	entries := a.playbook.Snapshot()
	if len(entries) == 0 {
		return "No remedies learned yet. A fix is remembered only after it has been applied and then passed verification, so the first one appears about " +
			a.cfg.HealVerifyAfter.String() + " after the first successful heal."
	}

	// Most-replayed first: hits are what make an entry worth having, since each
	// one is a diagnosis that cost no tokens. Ties break on recency.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Hits != entries[j].Hits {
			return entries[i].Hits > entries[j].Hits
		}
		return entries[i].LastVerified.After(entries[j].LastVerified)
	})

	var totalHits int
	for _, e := range entries {
		totalHits += e.Hits
	}

	shown := entries
	if len(shown) > playbookEntriesShown {
		shown = shown[:playbookEntriesShown]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d verified remedy(ies), %d replay(s) served without an LLM call.\n\n", len(entries), totalHits)
	for _, e := range shown {
		fmt.Fprintf(&b, "- %s on %s\n    %s\n", e.Kind, e.Controller, playbookAction(e))
		fmt.Fprintf(&b, "    verified %s ago, %s\n", roundAge(time.Since(e.LastVerified)), hitsPhrase(e))
	}
	if len(entries) > len(shown) {
		fmt.Fprintf(&b, "\n(%d more not shown)\n", len(entries)-len(shown))
	}
	b.WriteString("\nEach of these is re-validated against current cluster state before it replays, so one that no longer fits is declined rather than forced.")
	return b.String()
}

// playbookAction decodes the stored remedy and renders it. The stored form is
// raw heal.Action JSON; showing that verbatim would be unreadable in chat.
func playbookAction(e playbook.Entry) string {
	var act heal.Action
	if err := json.Unmarshal([]byte(e.ActionJSON), &act); err != nil {
		return "(stored remedy could not be decoded)"
	}
	return act.Describe()
}

func hitsPhrase(e playbook.Entry) string {
	switch e.Hits {
	case 0:
		return "not replayed yet"
	case 1:
		return fmt.Sprintf("replayed once, %s ago", roundAge(time.Since(e.LastHit)))
	default:
		return fmt.Sprintf("replayed %d times, last %s ago", e.Hits, roundAge(time.Since(e.LastHit)))
	}
}

func summaryOf(e audit.Event) string {
	if e.Summary != "" {
		return e.Summary
	}
	return e.Action
}

// --- Model-answered questions --------------------------------------------

// askContext is the JSON snapshot a free-form question is answered from. It is
// assembled per question rather than kept around, so an answer always describes
// the cluster as of the last sweep rather than whenever the chat started.
type askContext struct {
	Now           string             `json:"now"`
	LastSweep     *sweepFacts        `json:"lastSweep,omitempty"`
	OpenIncidents []incidentFacts    `json:"openIncidents,omitempty"`
	Capacity      *capacity.Snapshot `json:"clusterCapacity,omitempty"`
	RecentHeals   []audit.Event      `json:"recentHeals,omitempty"`
	Config        configFacts        `json:"podsmedicConfiguration"`
	Pod           *k8s.Bundle        `json:"podInQuestion,omitempty"`
	Missing       []string           `json:"unavailableEvidence,omitempty"`
}

type sweepFacts struct {
	At          string         `json:"at"`
	AgeSeconds  int            `json:"ageSeconds"`
	PodsScanned int            `json:"podsScanned"`
	Problems    []problemFacts `json:"problems,omitempty"`
	ProblemsBy  map[string]int `json:"problemsByKind,omitempty"`
}

type problemFacts struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
	Kind      string `json:"kind"`
	Message   string `json:"message,omitempty"`
}

type incidentFacts struct {
	Namespace   string   `json:"namespace"`
	Pod         string   `json:"pod"`
	Container   string   `json:"container,omitempty"`
	Kind        string   `json:"kind"`
	OtherKinds  []string `json:"otherKinds,omitempty"`
	OpenSeconds int      `json:"openSeconds"`
	Healed      bool     `json:"alreadyHealed"`
}

type configFacts struct {
	AutoHeal      bool     `json:"autoHealEnabled"`
	Applying      bool     `json:"applyingForReal"`
	HealKinds     []string `json:"healableProblemKinds,omitempty"`
	Predicting    bool     `json:"predictionEnabled"`
	ReplicaPolicy string   `json:"replicaScalingPolicy"`
	Namespaces    []string `json:"watchedNamespaces,omitempty"`
	SweepInterval string   `json:"sweepInterval"`
}

// maxProblemsInContext bounds how many problems go into one question's context.
// A cluster-wide outage produces hundreds; the model needs a representative
// sample, not a bill.
const maxProblemsInContext = 40

func (a *Agent) askModel(ctx context.Context, question string) (string, error) {
	snap := a.latestSweep()
	actx := askContext{
		Now:    time.Now().UTC().Format(time.RFC3339),
		Config: a.configFacts(),
	}

	if snap == nil {
		actx.Missing = append(actx.Missing, "no sweep has completed yet")
	} else {
		actx.LastSweep = sweepFactsFrom(snap)
		if snap.state != nil {
			actx.Capacity = snap.state.capacity
			if snap.state.capacity == nil && a.cfg.AutoHeal {
				actx.Missing = append(actx.Missing, "cluster capacity unreadable (node or pod list denied)")
			}
		}
	}

	for _, inc := range a.incidents.Snapshot() {
		_, _, healed, _ := a.incidents.HealProposal(inc.Key)
		actx.OpenIncidents = append(actx.OpenIncidents, incidentFacts{
			Namespace: inc.Namespace, Pod: inc.Pod, Container: inc.Container,
			Kind:        string(inc.Trigger.Kind),
			OtherKinds:  inc.OtherKinds(),
			OpenSeconds: int(time.Since(inc.FirstSeen).Seconds()),
			Healed:      healed,
		})
	}

	if events, err := a.audit.List(ctx); err == nil && len(events) > 0 {
		if len(events) > healsRecentLimit {
			events = events[len(events)-healsRecentLimit:]
		}
		actx.RecentHeals = events
	}

	// A question that names a pod gets that pod's full evidence, which is the
	// difference between "web is crashing" and the actual reason.
	if p, ok := a.podNamedIn(question, snap); ok {
		bundle, err := a.kube.Collect(ctx, p, k8s.CollectOptions{
			LogTailLines: a.cfg.LogTailLines,
			MaxEvents:    a.cfg.MaxEvents,
		})
		if err == nil {
			if snap != nil {
				a.enrich(ctx, bundle, snap.state)
			}
			actx.Pod = bundle
		}
	}

	payload, err := json.MarshalIndent(actx, "", "  ")
	if err != nil {
		return "", fmt.Errorf("build cluster snapshot: %w", err)
	}

	start := time.Now()
	answer, err := a.brain.Answer(ctx, question, payload)
	metrics.LLMLatency.Observe(time.Since(start).Seconds(), a.cfg.Provider)
	if err != nil {
		metrics.LLMRequests.Inc(a.cfg.Provider, "error")
		metrics.ChatAnswers.Inc("error")
		return "", err
	}
	metrics.LLMRequests.Inc(a.cfg.Provider, "ok")
	metrics.ChatAnswers.Inc("ok")
	a.recordUsage(a.log.With("path", "chat"), answer.Usage)
	return answer.Text, nil
}

func (a *Agent) configFacts() configFacts {
	policy := "disabled"
	if opts := a.healOpts; opts.AutoReplicas {
		policy = fmt.Sprintf("derived from measured CPU utilisation (target %d%%) and free cluster capacity", int(opts.TargetCPURatio*100))
		if opts.MaxReplicas > 0 {
			policy += fmt.Sprintf(", never above the configured backstop of %d", opts.MaxReplicas)
		}
	} else if a.healOpts.MaxReplicas > 0 {
		policy = fmt.Sprintf("proposed by the model, capped at %d", a.healOpts.MaxReplicas)
	}
	return configFacts{
		AutoHeal:      a.cfg.AutoHeal,
		Applying:      a.cfg.AutoHeal && a.cfg.HealApply,
		HealKinds:     a.cfg.HealAllowedKinds,
		Predicting:    a.cfg.Predict,
		ReplicaPolicy: policy,
		Namespaces:    a.cfg.Namespaces,
		SweepInterval: a.cfg.Interval.String(),
	}
}

func sweepFactsFrom(snap *sweepSnapshot) *sweepFacts {
	f := &sweepFacts{
		At:          snap.at.UTC().Format(time.RFC3339),
		AgeSeconds:  int(time.Since(snap.at).Seconds()),
		PodsScanned: snap.pods,
		ProblemsBy:  map[string]int{},
	}
	for _, p := range snap.problems {
		f.ProblemsBy[string(p.Kind)]++
		if len(f.Problems) >= maxProblemsInContext {
			continue
		}
		f.Problems = append(f.Problems, problemFacts{
			Namespace: p.Namespace, Pod: p.Pod, Container: p.Container,
			Kind: string(p.Kind), Message: p.Message,
		})
	}
	return f
}

// kindOperatorQuestion labels the evidence bundle collected for a healthy pod
// an operator asked about. It is not a detector output and never reaches the
// heal path — it only tells the model why this bundle is in the context.
const kindOperatorQuestion detect.Kind = "OperatorQuestion"

// podNamedIn finds a pod from the last sweep whose name appears in the
// question, so "why does web-7d9f keep dying?" pulls that pod's evidence.
//
// Matching is against names podsmedic has actually seen, never against
// arbitrary text: the question cannot make it fetch something outside the
// watched namespaces. A problem pod wins over a healthy one, since that is
// almost always what is being asked about.
func (a *Agent) podNamedIn(question string, snap *sweepSnapshot) (detect.Problem, bool) {
	if snap == nil {
		return detect.Problem{}, false
	}
	lower := strings.ToLower(question)

	best := detect.Problem{}
	found := false
	for _, p := range snap.problems {
		if len(p.Pod) >= 4 && strings.Contains(lower, strings.ToLower(p.Pod)) {
			// Prefer the longest matching name, so "web" does not shadow "web-canary".
			if !found || len(p.Pod) > len(best.Pod) {
				best, found = p, true
			}
		}
	}
	if found {
		return best, true
	}

	// No failing pod matched; fall back to any pod from the sweep, so a question
	// about a healthy workload still gets its real state.
	if snap.state != nil {
		for i := range snap.state.pods {
			pod := &snap.state.pods[i]
			if len(pod.Name) >= 4 && strings.Contains(lower, strings.ToLower(pod.Name)) {
				if !found || len(pod.Name) > len(best.Pod) {
					best = detect.Problem{Namespace: pod.Namespace, Pod: pod.Name, Kind: kindOperatorQuestion}
					found = true
				}
			}
		}
	}
	return best, found
}

func sortedKinds(m map[detect.Kind]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

func roundAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}

// latestSweep returns the most recent sweep snapshot, or nil before the first
// sweep completes.
func (a *Agent) latestSweep() *sweepSnapshot {
	a.sweepMu.RLock()
	defer a.sweepMu.RUnlock()
	return a.lastSweep
}

// sweepSnapshot is the cluster picture the chat path reads. It is replaced
// wholesale at the end of each sweep, so a reader either sees the previous
// sweep or the new one, never a half-updated mix.
type sweepSnapshot struct {
	at       time.Time
	pods     int
	problems []detect.Problem
	state    *sweepState
	// workloads maps "namespace/pod" to its controller, so an event about a pod
	// can be attributed to the workload the view draws.
	workloads map[string]string
}

func (a *Agent) publishSweep(pods int, problems []detect.Problem, st *sweepState) {
	index := map[string]string{}
	if st != nil {
		for i := range st.pods {
			p := &st.pods[i]
			if w := live.WorkloadOf(p); w != "" {
				index[p.Namespace+"/"+p.Name] = w
			}
		}
	}
	a.sweepMu.Lock()
	a.lastSweep = &sweepSnapshot{
		at: time.Now(), pods: pods, problems: problems, state: st, workloads: index,
	}
	a.sweepMu.Unlock()
}

// compile-time check that the agent satisfies the chat contract.
var _ chat.Answerer = (*Agent)(nil)

// LiveSnapshot implements live.Source: the picture a freshly-opened view needs
// before the event feed starts meaning anything.
//
// Workloads are grouped from the sweep's pod list rather than queried, so this
// costs nothing and always matches what the sweep actually saw.
func (a *Agent) LiveSnapshot() live.Snapshot {
	snap := a.latestSweep()
	out := live.Snapshot{
		At:          time.Now(),
		Healing:     a.cfg.AutoHeal && a.cfg.HealApply,
		IntervalSec: int(a.cfg.Interval.Seconds()),
	}
	if snap == nil {
		return out
	}

	out.Pods = snap.pods
	out.Problems = len(snap.problems)
	out.Incidents = a.incidents.OpenCount()
	out.SweepAgeSec = int(time.Since(snap.at).Seconds())
	if snap.state != nil && snap.state.capacity != nil {
		out.Nodes = snap.state.capacity.SchedulableNodes()
	}

	// Which workloads currently have a problem, so the view can mark them before
	// any event arrives.
	failing := map[string]int{}
	for _, p := range snap.problems {
		key := p.Namespace + "/" + snap.workloads[p.Namespace+"/"+p.Pod]
		if snap.workloads[p.Namespace+"/"+p.Pod] == "" {
			key = p.Namespace + "/" + p.Pod
		}
		failing[key]++
	}

	byKey := map[string]*live.Workload{}
	if snap.state != nil {
		for i := range snap.state.pods {
			pod := &snap.state.pods[i]
			name := snap.workloads[pod.Namespace+"/"+pod.Name]
			if name == "" {
				name = pod.Name // a bare pod stands alone in the ring
			}
			key := pod.Namespace + "/" + name
			w, ok := byKey[key]
			if !ok {
				w = &live.Workload{Key: key, Namespace: pod.Namespace, Name: name, Problems: failing[key]}
				byKey[key] = w
			}
			w.Pods++
			switch {
			case pod.Status.Phase == corev1.PodSucceeded:
				w.Done++
			case podIsReady(pod):
				w.Ready++
			}
		}
	}

	out.Workloads = make([]live.Workload, 0, len(byKey))
	for _, w := range byKey {
		out.Workloads = append(out.Workloads, *w)
	}
	sort.Slice(out.Workloads, func(i, j int) bool { return out.Workloads[i].Key < out.Workloads[j].Key })
	return out
}

func podIsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
