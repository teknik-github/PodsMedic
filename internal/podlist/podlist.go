// Package podlist turns a pod list into something readable in a chat message.
//
// It exists because "what is running, and is it healthy" is the question people
// actually ask, and answering it well is more than printing a phase. A pod in
// phase Running can be crash-looping; a pod in phase Failed may simply have
// been evicted; a Job's pod in phase Succeeded is finished, not broken. Getting
// those three wrong makes the answer worse than useless, so the derivation lives
// here as a pure function with a table test rather than inline in the agent.
//
// The rendering lives here too. A chat reply that lists thirty pods has to lead
// with what is wrong and summarise the rest, or the useful line scrolls off the
// top of someone's phone.
package podlist

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Status is one pod, reduced to what a human scanning a list needs.
type Status struct {
	Namespace string
	Name      string
	// State is the word kubectl would show: Running, CrashLoopBackOff,
	// Completed, Pending, Terminating, and so on.
	State    string
	Ready    int
	Total    int
	Restarts int32
	Node     string
	Age      time.Duration
	// Healthy is the judgement the summary counts on. Deliberately not
	// "phase == Running": see stateOf.
	Healthy bool
}

// Options selects what to list.
type Options struct {
	// Filter matches a namespace, a pod name, or any substring of either. Empty
	// lists everything.
	Filter string
	// Max bounds how many pods are spelled out. The rest are counted.
	Max int
}

// DefaultMax keeps a reply inside what a phone can show without endless
// scrolling.
const DefaultMax = 30

// Listing is the answer.
type Listing struct {
	// Filter echoes what was asked for, so a short list reads as "that is all
	// that matched" rather than "that is all there is".
	Filter string
	// Matched is everything the filter selected, unhealthy first.
	Matched []Status
	// Unhealthy is the subset worth looking at.
	Unhealthy []Status
	// Total is the whole cluster's pod count, before filtering.
	Total int
	// ByState counts every matched pod by its state word.
	ByState map[string]int
	max     int
}

// Summarize derives the listing. Pure: no cluster calls, no clock beyond the
// `now` handed in.
func Summarize(pods []corev1.Pod, opts Options, now time.Time) Listing {
	if opts.Max <= 0 {
		opts.Max = DefaultMax
	}
	filter := strings.ToLower(strings.TrimSpace(opts.Filter))
	if filter == "all" {
		filter = ""
	}

	out := Listing{Filter: strings.TrimSpace(opts.Filter), Total: len(pods),
		ByState: map[string]int{}, max: opts.Max}
	if out.Filter == "all" {
		out.Filter = ""
	}

	for i := range pods {
		p := &pods[i]
		if filter != "" &&
			!strings.Contains(strings.ToLower(p.Namespace), filter) &&
			!strings.Contains(strings.ToLower(p.Name), filter) {
			continue
		}
		s := statusOf(p, now)
		out.Matched = append(out.Matched, s)
		out.ByState[s.State]++
		if !s.Healthy {
			out.Unhealthy = append(out.Unhealthy, s)
		}
	}

	// Unhealthy first, then oldest trouble first within that — a pod that has
	// been broken for a day matters more than one that just started failing.
	sort.SliceStable(out.Matched, func(i, j int) bool {
		a, b := out.Matched[i], out.Matched[j]
		if a.Healthy != b.Healthy {
			return !a.Healthy
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.SliceStable(out.Unhealthy, func(i, j int) bool {
		return out.Unhealthy[i].Restarts > out.Unhealthy[j].Restarts
	})
	return out
}

// statusOf derives one pod's state and whether it counts as healthy.
//
// The order of these checks is the whole of the logic. Deletion beats
// everything, because a terminating pod's containers report whatever they
// happened to be doing when the kubelet started killing them. A waiting reason
// beats the phase, because that is where CrashLoopBackOff and ImagePullBackOff
// live while the phase still says Running or Pending.
func statusOf(p *corev1.Pod, now time.Time) Status {
	s := Status{
		Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName,
		Age: now.Sub(p.CreationTimestamp.Time), Total: len(p.Spec.Containers),
	}
	for _, cs := range p.Status.ContainerStatuses {
		s.Restarts += cs.RestartCount
		if cs.Ready {
			s.Ready++
		}
	}

	switch {
	case p.DeletionTimestamp != nil:
		s.State, s.Healthy = "Terminating", true // on its way out on purpose
		return s
	case p.Status.Phase == corev1.PodSucceeded:
		// A finished Job is not a broken pod. Counting it as one paints every
		// completed Job as permanently degraded — a mistake this project has
		// already made once, in the live view.
		s.State, s.Healthy = "Completed", true
		return s
	case p.Status.Phase == corev1.PodFailed:
		s.State = "Failed"
		if strings.EqualFold(p.Status.Reason, "Evicted") {
			s.State = "Evicted"
		}
		return s
	}

	// A waiting or recently-terminated container is more specific than the
	// phase, and is what someone actually needs to read.
	if reason := containerTrouble(p); reason != "" {
		s.State = reason
		return s
	}

	if p.Status.Phase == corev1.PodPending {
		s.State = "Pending"
		return s
	}

	s.State = "Running"
	s.Healthy = s.Ready == s.Total && s.Total > 0
	if !s.Healthy {
		s.State = "NotReady"
	}
	return s
}

// containerTrouble returns the first container-level reason worth surfacing.
func containerTrouble(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return w.Reason
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return "Init:" + w.Reason
		}
	}
	// A container that is running now but died badly last time still deserves
	// naming — OOMKilled is invisible otherwise until it happens again.
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" && !cs.Ready {
			return "OOMKilled"
		}
	}
	return ""
}

// Text renders the listing for a chat reply.
func (l Listing) Text() string {
	var b strings.Builder

	if len(l.Matched) == 0 {
		if l.Filter != "" {
			fmt.Fprintf(&b, "No pods match %q. I am watching %d pod(s) — try /pods with a namespace, or /pods all.",
				l.Filter, l.Total)
			return b.String()
		}
		return "No pods in the last sweep. Either the cluster is empty or I cannot list pods — check RBAC."
	}

	if l.Filter != "" {
		fmt.Fprintf(&b, "%d of %d pod(s) match %q", len(l.Matched), l.Total, l.Filter)
	} else {
		fmt.Fprintf(&b, "%d pod(s)", len(l.Matched))
	}
	fmt.Fprintf(&b, " — %s.\n", statesPhrase(l.ByState))

	if len(l.Unhealthy) > 0 {
		fmt.Fprintf(&b, "\nNeeds attention (%d):\n", len(l.Unhealthy))
		for _, s := range capped(l.Unhealthy, l.max) {
			b.WriteString(line(s))
		}
		if n := len(l.Unhealthy) - l.max; n > 0 {
			fmt.Fprintf(&b, "…and %d more.\n", n)
		}
	}

	// The full list only when asked for something specific. Unfiltered, the
	// summary plus the failures is the useful answer; thirty healthy lines would
	// bury it.
	if l.Filter != "" {
		healthy := l.Matched[len(l.Unhealthy):]
		if len(healthy) > 0 {
			b.WriteString("\nHealthy:\n")
			for _, s := range capped(healthy, l.max) {
				b.WriteString(line(s))
			}
			if n := len(healthy) - l.max; n > 0 {
				fmt.Fprintf(&b, "…and %d more.\n", n)
			}
		}
	} else if len(l.Unhealthy) == 0 {
		b.WriteString("\nNothing is failing. /pods <namespace> lists a namespace in full.")
	} else {
		b.WriteString("\n/pods <namespace> lists a namespace in full.")
	}
	return b.String()
}

func capped(list []Status, max int) []Status {
	if len(list) > max {
		return list[:max]
	}
	return list
}

func line(s Status) string {
	mark := "•"
	if !s.Healthy {
		mark = "⚠"
	}
	out := fmt.Sprintf("%s %s/%s\n   %s · %d/%d ready", mark, s.Namespace, s.Name, s.State, s.Ready, s.Total)
	if s.Restarts > 0 {
		out += fmt.Sprintf(" · %d restart%s", s.Restarts, plural(int(s.Restarts)))
	}
	return out + " · " + age(s.Age) + "\n"
}

// statesPhrase renders the state counts in a stable order, commonest first, so
// two replies about the same cluster read the same.
func statesPhrase(byState map[string]int) string {
	type kv struct {
		state string
		n     int
	}
	list := make([]kv, 0, len(byState))
	for s, n := range byState {
		list = append(list, kv{s, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].state < list[j].state
	})
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%d %s", e.n, e.state))
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func age(d time.Duration) string {
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
