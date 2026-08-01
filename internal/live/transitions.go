package live

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Transitions reports what changed between two observations of the same pod.
//
// It is pure over two pod values, which is what makes the interesting part —
// deciding what counts as "something happened" — testable without a cluster. The
// informer supplies the values; this decides whether they are worth drawing.
//
// The bar is deliberately high. A watch fires on every status write, and
// Kubernetes writes pod status constantly: probe results, condition timestamps,
// resource-version churn. Emitting an event for each would produce a display
// that never stops flickering and tells you nothing. So only genuine
// transitions count: a restart that actually incremented, readiness that
// actually flipped, a waiting reason that actually changed.
func Transitions(old, cur *corev1.Pod, now time.Time) []Event {
	switch {
	case cur == nil && old == nil:
		return nil
	case cur == nil:
		return []Event{base(old, ClassGone, "Deleted", "pod removed", now)}
	case old == nil:
		// A pod appearing is not itself news — a rollout creates many. It only
		// matters if it arrives already broken, which the checks below catch on
		// the next update anyway.
		return nil
	}

	var out []Event

	// A restart is the clearest signal there is, and the previous container's
	// termination reason is the diagnosis in miniature.
	for _, e := range restartEvents(old, cur, now) {
		out = append(out, e)
	}

	// Readiness flipping is what a human means by "it broke" / "it came back".
	oldReady, curReady := podReady(old), podReady(cur)
	switch {
	case !oldReady && curReady:
		out = append(out, base(cur, ClassRecovery, "Ready", "pod became ready", now))
	case oldReady && !curReady:
		out = append(out, base(cur, ClassProblem, "NotReady", "pod stopped being ready", now))
	}

	// A container entering a named waiting state — CrashLoopBackOff,
	// ImagePullBackOff, CreateContainerConfigError — is worth drawing once, on
	// the transition into it, not on every re-report.
	for _, e := range waitingEvents(old, cur, now) {
		out = append(out, e)
	}

	// Eviction terminates the pod for a reason outside its own control, which is
	// a different story from a crash.
	if cur.Status.Phase == corev1.PodFailed && old.Status.Phase != corev1.PodFailed {
		reason := cur.Status.Reason
		if reason == "" {
			reason = "Failed"
		}
		out = append(out, base(cur, ClassProblem, reason, cur.Status.Message, now))
	}

	return out
}

// restartEvents reports containers whose restart count actually went up, naming
// why the previous instance died.
func restartEvents(old, cur *corev1.Pod, now time.Time) []Event {
	before := map[string]int32{}
	for _, cs := range allStatuses(old) {
		before[cs.Name] = cs.RestartCount
	}

	var out []Event
	for _, cs := range allStatuses(cur) {
		prev, seen := before[cs.Name]
		if !seen || cs.RestartCount <= prev {
			continue
		}
		reason, detail := "Restarted", fmt.Sprintf("container %q restarted", cs.Name)
		if t := cs.LastTerminationState.Terminated; t != nil {
			if t.Reason != "" {
				reason = t.Reason
			}
			detail = fmt.Sprintf("container %q exited %d (%s)", cs.Name, t.ExitCode, reason)
		}
		// An OOM kill is a failure, not routine churn, so it reads as a problem.
		class := ClassRestart
		if reason == "OOMKilled" {
			class = ClassProblem
		}
		out = append(out, base(cur, class, reason, detail, now))
	}
	return out
}

// waitingEvents reports containers that entered a named waiting reason they were
// not in before.
func waitingEvents(old, cur *corev1.Pod, now time.Time) []Event {
	before := map[string]string{}
	for _, cs := range allStatuses(old) {
		before[cs.Name] = waitingReason(cs)
	}

	var out []Event
	for _, cs := range allStatuses(cur) {
		reason := waitingReason(cs)
		if reason == "" || reason == before[cs.Name] || !notableWaiting(reason) {
			continue
		}
		out = append(out, base(cur, ClassProblem, reason,
			fmt.Sprintf("container %q is %s", cs.Name, reason), now))
	}
	return out
}

// notableWaiting filters out the waiting reasons that are just a pod starting
// up. ContainerCreating and PodInitializing happen on every single rollout.
func notableWaiting(reason string) bool {
	switch reason {
	case "ContainerCreating", "PodInitializing", "":
		return false
	}
	return true
}

func waitingReason(cs corev1.ContainerStatus) string {
	if cs.State.Waiting == nil {
		return ""
	}
	return cs.State.Waiting.Reason
}

func allStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	if p == nil {
		return nil
	}
	out := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	out = append(out, p.Status.InitContainerStatuses...)
	return append(out, p.Status.ContainerStatuses...)
}

func podReady(p *corev1.Pod) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// base fills the fields every event about a pod shares.
func base(p *corev1.Pod, class Class, reason, detail string, now time.Time) Event {
	e := Event{
		At: now, Class: class, Reason: reason, Detail: detail,
	}
	if p != nil {
		e.Namespace, e.Pod, e.Node = p.Namespace, p.Name, p.Spec.NodeName
		e.Workload = WorkloadOf(p)
	}
	return e
}

// WorkloadOf names the thing a pod belongs to, so the view groups replicas
// together instead of scattering them.
//
// It reads the owner reference without following it to the Deployment: that
// would be an API call per pod, and for grouping a ReplicaSet name is nearly as
// good. The pod-template-hash suffix is trimmed so successive rollouts of one
// Deployment collapse into a single node in the view rather than sprouting a new
// one each time.
func WorkloadOf(p *corev1.Pod) string {
	if p == nil {
		return ""
	}
	for _, ref := range p.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind == "ReplicaSet" {
			if name, ok := trimPodTemplateHash(ref.Name); ok {
				return "Deployment/" + name
			}
		}
		return ref.Kind + "/" + ref.Name
	}
	return ""
}

// trimPodTemplateHash removes the "-<hash>" a Deployment appends to its
// ReplicaSet names. It only trims a segment that looks like a generated hash, so
// a ReplicaSet named by hand keeps its name intact.
func trimPodTemplateHash(name string) (string, bool) {
	i := lastDash(name)
	if i <= 0 {
		return name, false
	}
	suffix := name[i+1:]
	if len(suffix) < 5 || len(suffix) > 10 {
		return name, false
	}
	for _, r := range suffix {
		// Kubernetes generates these from an alphanumeric alphabet that excludes
		// the vowels and a few confusable digits, but checking the full
		// alphanumeric range is enough to avoid trimming a real name segment.
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return name, false
		}
	}
	return name[:i], true
}

func lastDash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}
