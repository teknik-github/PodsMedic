// Package detect turns raw pod state into a list of concrete problems.
package detect

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Kind classifies what went wrong with a pod.
type Kind string

const (
	KindOOMKilled          Kind = "OOMKilled"
	KindCrashLoopBackOff   Kind = "CrashLoopBackOff"
	KindImagePullBackOff   Kind = "ImagePullBackOff"
	KindCreateContainerErr Kind = "CreateContainerError"
	KindContainerError     Kind = "ContainerError"
	KindUnschedulable      Kind = "Unschedulable"
	KindEvicted            Kind = "Evicted"
	KindNotReady           Kind = "NotReady"
	KindRestartStorm       Kind = "RestartStorm"
	// KindMemoryPressure is predictive, not a current failure: a container's live
	// memory usage is sustained near its limit, so an OOM kill is likely soon.
	KindMemoryPressure Kind = "MemoryPressure"
	// KindCPUPressure is predictive: a container's live CPU usage is sustained
	// near its limit (heavy throttling), so the workload likely needs more
	// replicas or a higher CPU limit.
	KindCPUPressure Kind = "CPUPressure"

	// KindPVCPending is a pod the scheduler cannot place because a volume it
	// needs is not bound — a missing claim, a storage class that does not exist,
	// no matching PersistentVolume, or a node-affinity conflict on an existing
	// one. It is a more specific reading of Unschedulable, raised instead of it.
	KindPVCPending Kind = "PVCPending"
	// KindVolumeMountFailed is a pod that *was* scheduled but whose kubelet
	// cannot make its volumes available, leaving it stuck in ContainerCreating.
	// The decisive reason lives in the pod's events (FailedMount,
	// FailedAttachVolume), which the evidence bundle carries.
	KindVolumeMountFailed Kind = "VolumeMountFailed"
)

// Predictive reports whether a kind is a forecast (usage trending toward a
// limit) rather than a confirmed failure. Predictive problems must never drive a
// heal rollback: a lingering forecast is not proof a heal failed to hold.
func (k Kind) Predictive() bool {
	return k == KindMemoryPressure || k == KindCPUPressure
}

// Storage reports whether a kind is a storage fault.
//
// These are diagnosed and alerted on but never healed, and that is enforced
// structurally in heal.Validate rather than left to configuration: every repair
// a storage failure actually needs — editing a claim, provisioning a volume,
// freeing a disk — is either irreversible or destroys data, which is exactly
// what podsmedic's bounded-patch model is built to stay away from.
func (k Kind) Storage() bool {
	return k == KindPVCPending || k == KindVolumeMountFailed
}

// Problem is a single detected fault on a single pod.
type Problem struct {
	Namespace    string    `json:"namespace"`
	Pod          string    `json:"pod"`
	Container    string    `json:"container,omitempty"`
	Kind         Kind      `json:"kind"`
	Message      string    `json:"message"`
	RestartCount int32     `json:"restartCount,omitempty"`
	ExitCode     int32     `json:"exitCode,omitempty"`
	DetectedAt   time.Time `json:"detectedAt"`
}

// Fingerprint identifies the problem across polling cycles, so the same fault
// is not re-alerted on every tick.
func (p Problem) Fingerprint() string {
	return fmt.Sprintf("%s/%s/%s/%s", p.Namespace, p.Pod, p.Container, p.Kind)
}

func (p Problem) String() string {
	if p.Container != "" {
		return fmt.Sprintf("%s/%s [%s] %s", p.Namespace, p.Pod, p.Container, p.Kind)
	}
	return fmt.Sprintf("%s/%s %s", p.Namespace, p.Pod, p.Kind)
}

// Options tunes detection sensitivity.
type Options struct {
	// MinRestarts is the restart count above which a pod is flagged even if it
	// is currently running.
	MinRestarts int32
	// NotReadyGrace is how long a pod may stay not-ready before it is flagged.
	NotReadyGrace time.Duration
	// RestartWindow bounds how recent the last restart must be for a restart
	// storm to count. RestartCount is cumulative over a pod's whole life, so
	// without this a pod that flapped once months ago alerts forever.
	RestartWindow time.Duration
	// VolumeMountGrace is how long a scheduled pod may sit in ContainerCreating
	// before its volumes are presumed stuck. Attaching a cloud disk legitimately
	// takes tens of seconds, so this must not be aggressive.
	VolumeMountGrace time.Duration
}

// DefaultOptions returns sensible detection defaults.
func DefaultOptions() Options {
	return Options{
		MinRestarts:      3,
		NotReadyGrace:    10 * time.Minute,
		RestartWindow:    time.Hour,
		VolumeMountGrace: 2 * time.Minute,
	}
}

// Pods scans a pod list and returns every problem found.
func Pods(pods []corev1.Pod, opts Options) []Problem {
	now := time.Now()
	var out []Problem
	for i := range pods {
		out = append(out, pod(&pods[i], opts, now)...)
	}
	return out
}

func pod(p *corev1.Pod, opts Options, now time.Time) []Problem {
	var out []Problem

	// Pod-level failures.
	if p.Status.Phase == corev1.PodFailed && strings.EqualFold(p.Status.Reason, "Evicted") {
		out = append(out, Problem{
			Namespace: p.Namespace, Pod: p.Name, Kind: KindEvicted,
			Message: p.Status.Message, DetectedAt: now,
		})
	}

	if p.Status.Phase == corev1.PodPending {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled &&
				cond.Status == corev1.ConditionFalse &&
				cond.Reason == corev1.PodReasonUnschedulable {
				// A volume that will not bind is also "unschedulable", but the
				// remedy is completely different from too little CPU, so it gets
				// its own kind rather than both.
				kind := KindUnschedulable
				if storageBlocked(cond.Message) {
					kind = KindPVCPending
				}
				out = append(out, Problem{
					Namespace: p.Namespace, Pod: p.Name, Kind: kind,
					Message: cond.Message, DetectedAt: now,
				})
			}
		}
	}

	// Scheduled, but the kubelet cannot bring its volumes up.
	if vm, ok := volumeMountStuck(p, opts, now); ok {
		out = append(out, vm)
	}

	// Container-level failures. Init containers count too — a wedged init
	// container is the usual cause of a pod that never starts.
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		out = append(out, container(p, cs, opts, now)...)
	}

	// Running but never became ready.
	if p.Status.Phase == corev1.PodRunning && len(out) == 0 {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionFalse &&
				now.Sub(cond.LastTransitionTime.Time) > opts.NotReadyGrace {
				out = append(out, Problem{
					Namespace: p.Namespace, Pod: p.Name, Kind: KindNotReady,
					Message:    fmt.Sprintf("pod not ready for %s: %s", now.Sub(cond.LastTransitionTime.Time).Round(time.Minute), cond.Message),
					DetectedAt: now,
				})
			}
		}
	}

	return out
}

// storageMarkers are the phrases the scheduler uses when a volume, rather than
// compute capacity, is what prevents placement. Matching on the message is
// unavoidable: the scheduler reports every rejection through the same
// Unschedulable reason, so the message is the only thing that distinguishes
// "no room" from "no volume".
var storageMarkers = []string{
	"persistentvolumeclaim",
	"unbound immediate",
	"volume node affinity conflict",
	"had volume node affinity",
	"no persistent volumes available",
	"pod has unbound",
}

func storageBlocked(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range storageMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// volumeMountStuck reports a pod that the scheduler placed but the kubelet
// cannot start, because a volume will not attach or mount.
//
// The signal is a pod bound to a node whose containers are still
// ContainerCreating well past the point where that is normal, on a pod that
// actually mounts something mountable. That last condition matters: a pod stuck
// in ContainerCreating with only an emptyDir is almost always a CNI or image
// problem, and calling it a volume failure would send the reader in the wrong
// direction. The specific error (FailedMount, FailedAttachVolume) lives in the
// pod's events, which detection deliberately does not read — it stays pure over
// pod state, and the evidence bundle supplies the reason.
func volumeMountStuck(p *corev1.Pod, opts Options, now time.Time) (Problem, bool) {
	if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodPending {
		return Problem{}, false
	}
	if opts.VolumeMountGrace <= 0 || !mountsRealVolume(p) {
		return Problem{}, false
	}

	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	creating := false
	for _, cs := range statuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating" {
			creating = true
			break
		}
	}
	if !creating {
		return Problem{}, false
	}

	since, ok := scheduledAt(p)
	if !ok {
		return Problem{}, false
	}
	waited := now.Sub(since)
	if waited <= opts.VolumeMountGrace {
		return Problem{}, false
	}

	return Problem{
		Namespace: p.Namespace, Pod: p.Name, Kind: KindVolumeMountFailed,
		Message: fmt.Sprintf("scheduled to %s %s ago but still ContainerCreating: a volume is not attaching or mounting (claims: %s)",
			p.Spec.NodeName, waited.Round(time.Second), strings.Join(claimNames(p), ", ")),
		DetectedAt: now,
	}, true
}

// scheduledAt is when the pod was bound to its node — the point from which a
// mount has had a chance to succeed. StartTime is the fallback; it is set at
// admission, so it is close enough when the condition is missing.
func scheduledAt(p *corev1.Pod) (time.Time, bool) {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue && !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime.Time, true
		}
	}
	if p.Status.StartTime != nil && !p.Status.StartTime.IsZero() {
		return p.Status.StartTime.Time, true
	}
	return time.Time{}, false
}

// mountsRealVolume reports whether the pod mounts anything whose absence would
// wedge the kubelet. The projected service-account token every pod carries is
// not one of those, so it is not counted.
func mountsRealVolume(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil, v.CSI != nil, v.Secret != nil,
			v.ConfigMap != nil, v.NFS != nil, v.ISCSI != nil:
			return true
		}
	}
	return false
}

// claimNames lists the PVCs a pod mounts, for the problem message. Returns a
// placeholder rather than an empty string so the message never trails off.
func claimNames(p *corev1.Pod) []string {
	var out []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(out) == 0 {
		return []string{"none; a secret, configMap, or CSI volume"}
	}
	return out
}

func container(p *corev1.Pod, cs corev1.ContainerStatus, opts Options, now time.Time) []Problem {
	var out []Problem
	base := Problem{
		Namespace: p.Namespace, Pod: p.Name, Container: cs.Name,
		RestartCount: cs.RestartCount, DetectedAt: now,
	}

	// A terminated state carries the most specific signal (OOMKilled, exit code).
	// A *past* termination only counts while the container is still unhealthy —
	// otherwise every container that ever restarted alerts forever.
	if term, historical := terminated(cs); term != nil && !(historical && cs.Ready) {
		switch {
		case term.Reason == "OOMKilled":
			pr := base
			pr.Kind = KindOOMKilled
			pr.ExitCode = term.ExitCode
			pr.Message = fmt.Sprintf("container OOMKilled (exit %d) after %d restarts", term.ExitCode, cs.RestartCount)
			out = append(out, pr)
		case term.ExitCode != 0:
			pr := base
			pr.Kind = KindContainerError
			pr.ExitCode = term.ExitCode
			pr.Message = fmt.Sprintf("container exited with code %d (%s): %s", term.ExitCode, term.Reason, term.Message)
			out = append(out, pr)
		}
	}

	if w := cs.State.Waiting; w != nil {
		var kind Kind
		switch w.Reason {
		case "CrashLoopBackOff":
			kind = KindCrashLoopBackOff
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			kind = KindImagePullBackOff
		case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
			kind = KindCreateContainerErr
		}
		if kind != "" {
			pr := base
			pr.Kind = kind
			pr.Message = fmt.Sprintf("%s: %s", w.Reason, w.Message)
			out = append(out, pr)
		}
	}

	// Restart storm: currently up, but flapping recently.
	if len(out) == 0 && opts.MinRestarts > 0 && cs.RestartCount >= opts.MinRestarts && restartedRecently(cs, opts, now) {
		pr := base
		pr.Kind = KindRestartStorm
		pr.Message = fmt.Sprintf("container restarted %d times, most recently %s ago",
			cs.RestartCount, now.Sub(cs.LastTerminationState.Terminated.FinishedAt.Time).Round(time.Minute))
		out = append(out, pr)
	}

	return out
}

// restartedRecently reports whether the container's last restart falls inside
// the configured window. A container with no recorded termination time is
// treated as not recent — the restarts predate any state we can see.
func restartedRecently(cs corev1.ContainerStatus, opts Options, now time.Time) bool {
	if opts.RestartWindow <= 0 {
		return true
	}
	term := cs.LastTerminationState.Terminated
	if term == nil || term.FinishedAt.IsZero() {
		return false
	}
	return now.Sub(term.FinishedAt.Time) <= opts.RestartWindow
}

// terminated returns the most recent terminated state for a container. The
// second return value reports whether it came from a previous instance rather
// than the container's current state.
func terminated(cs corev1.ContainerStatus) (*corev1.ContainerStateTerminated, bool) {
	if cs.State.Terminated != nil {
		return cs.State.Terminated, false
	}
	if cs.LastTerminationState.Terminated != nil {
		return cs.LastTerminationState.Terminated, true
	}
	return nil, false
}
