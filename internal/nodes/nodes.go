// Package nodes reports on the health of the machines under the workloads.
//
// Everything else in podsmedic reasons from pods, which means it learns about a
// sick node only after that node's pods have already fallen over. A node with a
// full disk stops accepting new pods and starts evicting the ones it has; a node
// that goes NotReady strands everything on it for five minutes before the
// controller manager begins rescheduling. In both cases the node itself said so
// first, and acting on that is a stage earlier than any pod-derived signal.
//
// This package only ever reports. podsmedic has no write permission on nodes and
// must not acquire any: cordoning or draining a node is a decision with a blast
// radius far larger than patching one workload, and it is exactly the decision
// an LLM working from log text should not be making. The finding goes to a human.
//
// Check is a pure function of the node states handed to it, so every rule below
// is table-tested.
package nodes

import (
	"fmt"
	"sort"
	"time"
)

// Kind names a node-level fault.
type Kind string

const (
	// KindNotReady is the kubelet no longer reporting healthy. Everything on the
	// node is at risk, and nothing new will be scheduled there.
	KindNotReady Kind = "NodeNotReady"
	// KindDiskPressure means the kubelet is short of disk. It stops admitting
	// pods and begins evicting to reclaim space, so it degrades workloads on its
	// own initiative.
	KindDiskPressure Kind = "NodeDiskPressure"
	// KindMemoryPressure means the kubelet is short of memory and will evict
	// best-effort pods.
	KindMemoryPressure Kind = "NodeMemoryPressure"
	// KindPIDPressure means process IDs are running out — rarer, and usually a
	// runaway workload rather than the node itself.
	KindPIDPressure Kind = "NodePIDPressure"
	// KindNetworkUnavailable means the node's network is not correctly
	// configured. Often transient at boot, which is why it is graced like the
	// rest.
	KindNetworkUnavailable Kind = "NodeNetworkUnavailable"
	// KindCordoned is an operator's own doing far more often than a fault, so it
	// is only reported when asked for. It still matters, because a cordoned node
	// silently removes its headroom from every scale-up decision.
	KindCordoned Kind = "NodeCordoned"
)

// Severity ranks a finding. Only two levels: a node problem is either taking
// workloads down now or it is not, and a third level would just invite
// everything to land in the middle.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// Condition is one node condition as the API reported it.
type Condition struct {
	Type    string
	Active  bool
	Reason  string
	Message string
	// Since is the last transition time — how long the condition has held.
	Since time.Time
}

// State is one node, reduced to what the rules need.
type State struct {
	Name string
	// Unschedulable mirrors spec.unschedulable: the node is cordoned.
	Unschedulable bool
	// Conditions are the node's reported conditions. A Ready condition that is
	// missing entirely counts as not ready — a node that will not say it is
	// healthy is not healthy.
	Conditions []Condition
	// Pods is how many pods are currently placed here, which is what turns
	// "a node is sick" into "this much is at risk".
	Pods int
	// KubeletVersion is carried for the message only.
	KubeletVersion string
}

// Finding is one node-level fault worth telling a human about.
type Finding struct {
	Node     string
	Kind     Kind
	Severity string
	// Summary is a complete sentence: these go straight to Telegram, where
	// there is no surrounding context to lean on.
	Summary string
	// Since is when the condition last transitioned, or zero if unknown.
	Since time.Time
	// Pods is how many pods sit on the node.
	Pods int
}

// Key identifies the fault for deduplication. Deliberately excludes Since and
// Pods: a node that has been NotReady for an hour is the same fact it was
// fifty-nine minutes ago, and re-alerting because a pod count moved would make
// the feature unusable.
func (f Finding) Key() string { return f.Node + "|" + string(f.Kind) }

// Options tunes the checks.
type Options struct {
	// Grace is how long a condition must have held before it is reported. Node
	// conditions flap — a kubelet restart briefly drops Ready, and
	// NetworkUnavailable is normal for the first seconds of a node's life — so
	// reporting instantly would mean reporting noise.
	Grace time.Duration
	// ReportCordoned includes deliberately cordoned nodes. Off by default,
	// because during a planned drain it would alert on the operator's own work.
	ReportCordoned bool
}

// DefaultOptions are the settings a cluster gets if it configures nothing.
func DefaultOptions() Options {
	return Options{Grace: 3 * time.Minute}
}

// Check returns every node-level fault worth reporting, worst first.
func Check(states []State, opts Options, now time.Time) []Finding {
	var out []Finding
	for _, s := range states {
		out = append(out, checkNode(s, opts, now)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == SeverityCritical
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func checkNode(s State, opts Options, now time.Time) []Finding {
	var out []Finding

	ready, hasReady := s.condition("Ready")
	// A node reporting nothing is worse than one reporting a problem, not
	// better: treat a missing Ready condition as not ready.
	switch {
	case !hasReady:
		out = append(out, Finding{
			Node: s.Name, Kind: KindNotReady, Severity: SeverityCritical, Pods: s.Pods,
			Summary: fmt.Sprintf("Node %s reports no Ready condition at all — the kubelet is not talking to the API server. %s at risk.",
				s.Name, podsPhrase(s.Pods)),
		})
	case !ready.Active && held(ready, opts.Grace, now):
		out = append(out, Finding{
			Node: s.Name, Kind: KindNotReady, Severity: SeverityCritical, Pods: s.Pods, Since: ready.Since,
			Summary: fmt.Sprintf("Node %s has been NotReady for %s (%s). %s on it; nothing new will schedule there, and the existing pods are rescheduled only after the eviction timeout.",
				s.Name, since(ready.Since, now), detail(ready), podsPhrase(s.Pods)),
		})
	}

	for _, p := range []struct {
		condition string
		kind      Kind
		severity  string
		effect    string
	}{
		{"DiskPressure", KindDiskPressure, SeverityCritical,
			"the kubelet has stopped admitting pods and will start evicting to reclaim disk"},
		{"MemoryPressure", KindMemoryPressure, SeverityCritical,
			"the kubelet will start evicting best-effort pods"},
		{"PIDPressure", KindPIDPressure, SeverityWarning,
			"process IDs are running short, usually because one workload is spawning without limit"},
		{"NetworkUnavailable", KindNetworkUnavailable, SeverityWarning,
			"the node's network is not correctly configured, so its pods may be unreachable"},
	} {
		c, ok := s.condition(p.condition)
		if !ok || !c.Active || !held(c, opts.Grace, now) {
			continue
		}
		out = append(out, Finding{
			Node: s.Name, Kind: p.kind, Severity: p.severity, Pods: s.Pods, Since: c.Since,
			Summary: fmt.Sprintf("Node %s has reported %s for %s (%s) — %s. %s on it.",
				s.Name, p.condition, since(c.Since, now), detail(c), p.effect, podsPhrase(s.Pods)),
		})
	}

	// A cordoned node is reported last and only on request: it is usually
	// deliberate. It is worth knowing about because its capacity silently
	// disappears from every scale-up decision the validator makes.
	if opts.ReportCordoned && s.Unschedulable {
		out = append(out, Finding{
			Node: s.Name, Kind: KindCordoned, Severity: SeverityWarning, Pods: s.Pods,
			Summary: fmt.Sprintf("Node %s is cordoned. %s still running on it, and its free capacity no longer counts toward any scale-up.",
				s.Name, podsPhrase(s.Pods)),
		})
	}
	return out
}

func (s State) condition(name string) (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == name {
			return c, true
		}
	}
	return Condition{}, false
}

// held reports whether a condition has persisted long enough to be believed.
// A condition with no transition time is believed immediately: an unknown age
// is not evidence that it is new.
func held(c Condition, grace time.Duration, now time.Time) bool {
	if c.Since.IsZero() {
		return true
	}
	return now.Sub(c.Since) >= grace
}

func since(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// detail prefers the condition's reason, falling back to its message. Both come
// from the kubelet rather than from a workload, so neither is untrusted input in
// the way a pod log is.
func detail(c Condition) string {
	if c.Reason != "" {
		return c.Reason
	}
	if c.Message != "" {
		return c.Message
	}
	return "no reason given"
}

func podsPhrase(n int) string {
	switch n {
	case 0:
		return "No pods"
	case 1:
		return "1 pod"
	default:
		return fmt.Sprintf("%d pods", n)
	}
}
