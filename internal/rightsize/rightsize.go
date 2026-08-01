// Package rightsize watches what containers actually use and reports where the
// declared requests are wrong.
//
// # Why this is a report and never a heal
//
// heal.Validate holds one invariant above all others: a value may only ever
// increase. That is what makes an untrusted model's proposal safe to act on —
// the worst it can do is give a workload too much. Rightsizing is the opposite
// direction. Lowering a request moves a workload's scheduling floor and its
// eviction priority; get it wrong and the pod is evicted under pressure it used
// to survive, or fails to schedule at all. There is no safe automatic version of
// that, so this package produces a document for a human and stops.
//
// The measurement is a long game. A container's peak over ten minutes says
// nothing — every workload has a quiet ten minutes. Findings therefore require
// both a minimum number of samples and a minimum observation window, and the
// observations are persisted so a restart does not reset the clock.
//
// Everything here is pure: Observe folds samples into state, Findings is a
// function of that state and the options. No cluster calls, no model.
package rightsize

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sample is one container's measured usage alongside what it asked for.
type Sample struct {
	Namespace string
	// Workload is the controller key, not the pod: pods churn and a
	// per-pod history would reset on every rollout.
	Workload  string
	Container string

	CPUMilli int64
	MemBytes int64

	RequestCPUMilli int64
	RequestMemBytes int64
	LimitCPUMilli   int64
	LimitMemBytes   int64
}

func (s Sample) key() string { return s.Namespace + "/" + s.Workload + "/" + s.Container }

// Observation is the accumulated history for one container.
//
// Peak and mean are both kept because they answer different questions: the peak
// is what the container must never be denied, the mean is how much of the
// reservation is idle most of the time. A recommendation built on the mean
// alone would throttle every bursty workload in the cluster.
type Observation struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Container string `json:"container"`

	Samples      int   `json:"samples"`
	PeakCPUMilli int64 `json:"peakCPUMilli"`
	PeakMemBytes int64 `json:"peakMemBytes"`
	SumCPUMilli  int64 `json:"sumCPUMilli"`
	SumMemBytes  int64 `json:"sumMemBytes"`

	// The most recently seen spec. Kept current rather than averaged: a
	// recommendation must be measured against what the workload asks for now.
	RequestCPUMilli int64 `json:"requestCPUMilli"`
	RequestMemBytes int64 `json:"requestMemBytes"`
	LimitCPUMilli   int64 `json:"limitCPUMilli"`
	LimitMemBytes   int64 `json:"limitMemBytes"`

	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Window is how long this container has been under observation.
func (o Observation) Window() time.Duration { return o.Last.Sub(o.First) }

// MeanCPUMilli and MeanMemBytes are the averages over every sample taken.
func (o Observation) MeanCPUMilli() int64 { return div(o.SumCPUMilli, int64(o.Samples)) }
func (o Observation) MeanMemBytes() int64 { return div(o.SumMemBytes, int64(o.Samples)) }

func div(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// Kind names what is wrong with a container's sizing.
type Kind string

const (
	// KindOversized is a container reserving far more than it has ever used.
	// The cost is real but indirect: the reservation is subtracted from every
	// scheduling decision, so the cluster runs out of room while the nodes sit
	// idle.
	KindOversized Kind = "Oversized"
	// KindUndersized is a container whose peak use exceeds its request. It runs
	// on borrowed capacity: the scheduler placed it as though it needed less, so
	// the node is overcommitted and this pod is a likely eviction victim.
	KindUndersized Kind = "Undersized"
	// KindNoRequests is a container that declares no requests at all. It is
	// scheduled as best-effort, evicted first under pressure, and invisible to
	// every capacity calculation podsmedic makes — including the one that
	// decides whether a scale-up fits.
	KindNoRequests Kind = "NoRequests"
)

// Resource distinguishes a CPU finding from a memory one.
type Resource string

const (
	CPU    Resource = "cpu"
	Memory Resource = "memory"
)

// Finding is one sizing problem, with the evidence behind it.
type Finding struct {
	Namespace string
	Workload  string
	Container string
	Kind      Kind
	Resource  Resource

	// Current is the declared request; Peak and Mean are what was measured.
	// CPU values are millicores, memory values are bytes.
	Current int64
	Peak    int64
	Mean    int64
	// Recommended is the suggested request. Zero for KindNoRequests on CPU,
	// where a peak of nothing recommends nothing.
	Recommended int64
	// Delta is Recommended - Current: negative means the reservation shrinks.
	Delta int64

	Samples int
	Window  time.Duration
	Summary string
}

// Options tunes what counts as wrong, and how sure we have to be.
type Options struct {
	// MinSamples and MinWindow are both required before a container is judged.
	// Samples alone is not enough — a fast sweep interval would reach a hundred
	// samples inside an hour and call a nightly batch job oversized.
	MinSamples int
	MinWindow  time.Duration

	// OverRatio: peak below this fraction of the request means oversized.
	OverRatio float64
	// Headroom multiplies the observed peak to get the recommendation, so the
	// suggestion leaves room for a busier day than the one we watched.
	Headroom float64

	// MinCPUSavingMilli and MinMemSavingBytes suppress findings too small to be
	// worth anyone's attention. A report full of 5m savings is a report nobody
	// reads.
	MinCPUSavingMilli int64
	MinMemSavingBytes int64
}

// DefaultOptions are deliberately conservative: a wrong "you can halve this" is
// far more expensive than a missed saving.
func DefaultOptions() Options {
	return Options{
		MinSamples:        60,
		MinWindow:         24 * time.Hour,
		OverRatio:         0.40,
		Headroom:          1.5,
		MinCPUSavingMilli: 50,
		MinMemSavingBytes: 64 << 20, // 64Mi
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.MinSamples <= 0 {
		o.MinSamples = d.MinSamples
	}
	if o.MinWindow <= 0 {
		o.MinWindow = d.MinWindow
	}
	if o.OverRatio <= 0 {
		o.OverRatio = d.OverRatio
	}
	if o.Headroom < 1 {
		// Below 1 the "recommendation" would be under the observed peak, which
		// is not a recommendation, it is a promise of an OOM kill.
		o.Headroom = d.Headroom
	}
	return o
}

// DefaultMaxTracked bounds the history so its ConfigMap stays well inside the
// 1MiB object limit.
const DefaultMaxTracked = 400

// Tracker accumulates observations across sweeps.
type Tracker struct {
	mu         sync.Mutex
	obs        map[string]*Observation
	maxTracked int
	dirty      bool
}

// New builds an empty tracker.
func New(maxTracked int) *Tracker {
	if maxTracked <= 0 {
		maxTracked = DefaultMaxTracked
	}
	return &Tracker{obs: map[string]*Observation{}, maxTracked: maxTracked}
}

// Observe folds one sweep's samples into the history.
//
// A sample whose declared request has changed does not reset the history: the
// measured usage is a property of the workload, not of what it asked for, and
// throwing away a week of measurement because someone edited a manifest would
// make the report permanently unavailable on an actively maintained cluster.
func (t *Tracker) Observe(samples []Sample, now time.Time) {
	if len(samples) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, s := range samples {
		if s.Workload == "" || s.Container == "" {
			continue // an unowned pod has no stable identity to accumulate against
		}
		o, ok := t.obs[s.key()]
		if !ok {
			if len(t.obs) >= t.maxTracked {
				t.forgetStalest()
			}
			o = &Observation{
				Namespace: s.Namespace, Workload: s.Workload, Container: s.Container,
				First: now,
			}
			t.obs[s.key()] = o
		}
		o.Samples++
		o.SumCPUMilli += s.CPUMilli
		o.SumMemBytes += s.MemBytes
		if s.CPUMilli > o.PeakCPUMilli {
			o.PeakCPUMilli = s.CPUMilli
		}
		if s.MemBytes > o.PeakMemBytes {
			o.PeakMemBytes = s.MemBytes
		}
		o.RequestCPUMilli = s.RequestCPUMilli
		o.RequestMemBytes = s.RequestMemBytes
		o.LimitCPUMilli = s.LimitCPUMilli
		o.LimitMemBytes = s.LimitMemBytes
		o.Last = now
	}
	t.dirty = true
}

// Findings reports every sizing problem worth acting on, largest saving first.
func (t *Tracker) Findings(opts Options, now time.Time) []Finding {
	opts = opts.withDefaults()
	t.mu.Lock()
	obs := make([]Observation, 0, len(t.obs))
	for _, o := range t.obs {
		obs = append(obs, *o)
	}
	t.mu.Unlock()

	var out []Finding
	for _, o := range obs {
		if o.Samples < opts.MinSamples || o.Window() < opts.MinWindow {
			continue // not yet enough evidence to say anything
		}
		out = append(out, judge(o, opts)...)
	}
	sort.Slice(out, func(i, j int) bool {
		// Biggest reduction first, so the top of the report is where the room is.
		if out[i].Delta != out[j].Delta {
			return out[i].Delta < out[j].Delta
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}

func judge(o Observation, opts Options) []Finding {
	var out []Finding
	base := Finding{
		Namespace: o.Namespace, Workload: o.Workload, Container: o.Container,
		Samples: o.Samples, Window: o.Window(),
	}

	for _, r := range []struct {
		res         Resource
		request     int64
		peak, mean  int64
		minSaving   int64
		format      func(int64) string
		recommender func(int64) int64
	}{
		{CPU, o.RequestCPUMilli, o.PeakCPUMilli, o.MeanCPUMilli(), opts.MinCPUSavingMilli, FormatCPU, roundCPU},
		{Memory, o.RequestMemBytes, o.PeakMemBytes, o.MeanMemBytes(), opts.MinMemSavingBytes, FormatMem, roundMem},
	} {
		f := base
		f.Resource = r.res
		f.Current, f.Peak, f.Mean = r.request, r.peak, r.mean

		switch {
		case r.request == 0:
			// No request at all. Reporting this even when usage is tiny is the
			// point: the harm is not the size, it is that the scheduler and every
			// capacity calculation are working from a zero that is not true.
			f.Kind = KindNoRequests
			f.Recommended = r.recommender(scale(r.peak, opts.Headroom))
			f.Delta = f.Recommended
			f.Summary = fmt.Sprintf("%s/%s (%s) declares no %s request. Measured peak %s over %s: it is scheduled as best-effort, evicted first under pressure, and counts as zero in every capacity check podsmedic makes.",
				o.Namespace, o.Workload, o.Container, r.res, r.format(r.peak), roundWindow(o.Window()))
			out = append(out, f)

		case r.peak > r.request:
			f.Kind = KindUndersized
			f.Recommended = r.recommender(scale(r.peak, opts.Headroom))
			f.Delta = f.Recommended - r.request
			f.Summary = fmt.Sprintf("%s/%s (%s) peaked at %s %s against a %s request — it is running on capacity the scheduler did not reserve. Consider raising the request to %s.",
				o.Namespace, o.Workload, o.Container, r.format(r.peak), r.res, r.format(r.request), r.format(f.Recommended))
			out = append(out, f)

		case float64(r.peak) < float64(r.request)*opts.OverRatio:
			f.Recommended = r.recommender(scale(r.peak, opts.Headroom))
			if f.Recommended >= r.request {
				continue // rounding and headroom ate the whole saving
			}
			f.Delta = f.Recommended - r.request
			if -f.Delta < r.minSaving {
				continue // too small to be worth anyone's attention
			}
			f.Kind = KindOversized
			f.Summary = fmt.Sprintf("%s/%s (%s) reserves %s %s but peaked at %s (mean %s) over %s across %d samples. Request could drop to %s, returning %s to the cluster.",
				o.Namespace, o.Workload, o.Container, r.format(r.request), r.res,
				r.format(r.peak), r.format(r.mean), roundWindow(o.Window()), o.Samples,
				r.format(f.Recommended), r.format(-f.Delta))
			out = append(out, f)
		}
	}
	return out
}

func scale(v int64, by float64) int64 { return int64(float64(v) * by) }

// roundCPU rounds up to the nearest 10 millicores; roundMem to the nearest
// 16MiB. Nobody writes a request of 137m or 331Mi, and a recommendation people
// will not copy is a recommendation that does not get applied.
func roundCPU(v int64) int64 {
	const step = 10
	if v <= 0 {
		return 0
	}
	return ((v + step - 1) / step) * step
}

func roundMem(v int64) int64 {
	const step = 16 << 20
	if v <= 0 {
		return 0
	}
	return ((v + step - 1) / step) * step
}

// FormatCPU and FormatMem render values the way a manifest spells them, so a
// reader can paste the number straight into one.
func FormatCPU(milli int64) string {
	if milli%1000 == 0 && milli > 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%dm", milli)
}

func FormatMem(b int64) string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGi", b>>30)
	case b >= 1<<20:
		return fmt.Sprintf("%dMi", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dKi", b>>10)
	default:
		return fmt.Sprintf("%d", b)
	}
}

func roundWindow(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Totals sums what the findings would return to the cluster. Only reductions
// count: an undersized container's increase is a correction, not a cost saving,
// and netting the two would let one hide the other.
func Totals(fs []Finding) (cpuMilli, memBytes int64) {
	for _, f := range fs {
		if f.Delta >= 0 {
			continue
		}
		switch f.Resource {
		case CPU:
			cpuMilli += -f.Delta
		case Memory:
			memBytes += -f.Delta
		}
	}
	return cpuMilli, memBytes
}

// Tracking is the number of containers under observation, for metrics.
func (t *Tracker) Tracking() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.obs)
}

// Forget drops containers not seen since cutoff, so a deleted workload does not
// sit in the history forever.
func (t *Tracker) Forget(cutoff time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k, o := range t.obs {
		if o.Last.Before(cutoff) {
			delete(t.obs, k)
			n++
		}
	}
	if n > 0 {
		t.dirty = true
	}
	return n
}

// forgetStalest makes room for a new container. Caller holds the lock.
func (t *Tracker) forgetStalest() {
	var stalestKey string
	var stalest time.Time
	for k, o := range t.obs {
		if stalestKey == "" || o.Last.Before(stalest) {
			stalestKey, stalest = k, o.Last
		}
	}
	if stalestKey != "" {
		delete(t.obs, stalestKey)
	}
}

// Dirty reports whether the history changed since the last ClearDirty.
func (t *Tracker) Dirty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dirty
}

// ClearDirty marks the current state as persisted.
func (t *Tracker) ClearDirty() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirty = false
}

// Snapshot copies every observation, for persistence.
func (t *Tracker) Snapshot() []Observation {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Observation, 0, len(t.obs))
	for _, o := range t.obs {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+out[i].Workload+out[i].Container <
			out[j].Namespace+out[j].Workload+out[j].Container
	})
	return out
}

// Restore loads a persisted history. It does not mark the tracker dirty: the
// restored state already matches what is stored.
func (t *Tracker) Restore(list []Observation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.obs = make(map[string]*Observation, len(list))
	for i := range list {
		o := list[i]
		t.obs[o.Namespace+"/"+o.Workload+"/"+o.Container] = &o
	}
}
