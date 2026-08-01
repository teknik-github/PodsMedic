package detect

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOOMKilledDetected(t *testing.T) {
	pod := basePod("api", "web-7d9f")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "web",
		RestartCount: 4,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
		},
	}}

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())

	if !hasKind(problems, KindOOMKilled) {
		t.Fatalf("expected OOMKilled, got %v", kinds(problems))
	}
	if !hasKind(problems, KindCrashLoopBackOff) {
		t.Fatalf("expected CrashLoopBackOff alongside OOMKilled, got %v", kinds(problems))
	}
}

func TestUnschedulableDetected(t *testing.T) {
	pod := basePod("api", "worker-0")
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: "0/3 nodes are available: 3 Insufficient memory.",
	}}

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())

	if len(problems) != 1 || problems[0].Kind != KindUnschedulable {
		t.Fatalf("expected single Unschedulable problem, got %v", kinds(problems))
	}
}

func TestHealthyPodProducesNoProblems(t *testing.T) {
	pod := basePod("api", "web-healthy")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "web",
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Hour))},
		},
	}}

	if problems := Pods([]corev1.Pod{pod}, DefaultOptions()); len(problems) != 0 {
		t.Fatalf("expected no problems for a healthy pod, got %v", kinds(problems))
	}
}

// A container that crashed once long ago but is running and ready now is not a
// problem. Without this rule every long-lived cluster alerts on stale history.
func TestRecoveredContainerIsNotAlerted(t *testing.T) {
	pod := basePod("longhorn-system", "csi-attacher-866df4b764-8569n")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "csi-attacher",
		Ready:        true,
		RestartCount: 1,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-72 * time.Hour))},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "Error",
				ExitCode:   1,
				Message:    "Lost connection to CSI driver, exiting",
				FinishedAt: metav1.NewTime(time.Now().Add(-72 * time.Hour)),
			},
		},
	}}

	if problems := Pods([]corev1.Pod{pod}, DefaultOptions()); len(problems) != 0 {
		t.Fatalf("a recovered container must not alert, got %v", kinds(problems))
	}
}

func TestRestartStormOnlyAboveThreshold(t *testing.T) {
	pod := basePod("api", "flappy")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "web",
		Ready:        true,
		RestartCount: 2,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now())},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "Error",
				ExitCode:   1,
				FinishedAt: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
			},
		},
	}}

	opts := DefaultOptions() // MinRestarts = 3
	if problems := Pods([]corev1.Pod{pod}, opts); len(problems) != 0 {
		t.Fatalf("2 restarts is below the threshold, got %v", kinds(problems))
	}

	pod.Status.ContainerStatuses[0].RestartCount = 5
	if problems := Pods([]corev1.Pod{pod}, opts); !hasKind(problems, KindRestartStorm) {
		t.Fatalf("expected RestartStorm at 5 restarts, got %v", kinds(problems))
	}
}

// RestartCount is cumulative for the pod's whole life. A pod that flapped days
// ago and has been stable since is not an incident.
func TestStaleRestartStormIsIgnored(t *testing.T) {
	pod := basePod("api", "long-lived")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "web",
		Ready:        true,
		RestartCount: 9,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour))},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "Error",
				ExitCode:   1,
				FinishedAt: metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour)),
			},
		},
	}}

	if problems := Pods([]corev1.Pod{pod}, DefaultOptions()); len(problems) != 0 {
		t.Fatalf("restarts outside the window must not alert, got %v", kinds(problems))
	}
}

func TestNotReadyRespectsGracePeriod(t *testing.T) {
	pod := basePod("api", "slow-start")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		Message:            "containers with unready status: [web]",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
	}}

	opts := DefaultOptions() // NotReadyGrace = 10m
	if problems := Pods([]corev1.Pod{pod}, opts); len(problems) != 0 {
		t.Fatalf("2 minutes not-ready is within grace, got %v", kinds(problems))
	}

	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	if problems := Pods([]corev1.Pod{pod}, opts); !hasKind(problems, KindNotReady) {
		t.Fatalf("expected NotReady after 30 minutes, got %v", kinds(problems))
	}
}

func TestInitContainerFailureDetected(t *testing.T) {
	pod := basePod("api", "needs-migration")
	pod.Status.Phase = corev1.PodPending
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "migrate",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, Message: "connection refused"},
		},
	}}

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())
	if !hasKind(problems, KindContainerError) {
		t.Fatalf("expected ContainerError from the init container, got %v", kinds(problems))
	}
	if problems[0].Container != "migrate" {
		t.Fatalf("expected container name %q, got %q", "migrate", problems[0].Container)
	}
}

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	a := Problem{Namespace: "api", Pod: "web", Container: "app", Kind: KindOOMKilled}
	b := Problem{Namespace: "api", Pod: "web", Container: "app", Kind: KindOOMKilled, Message: "differs"}
	c := Problem{Namespace: "api", Pod: "web", Container: "app", Kind: KindCrashLoopBackOff}

	if a.Fingerprint() != b.Fingerprint() {
		t.Error("fingerprint must ignore the message so repeats are suppressed")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("fingerprint must distinguish problem kinds")
	}
}

func basePod(namespace, name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

func hasKind(problems []Problem, k Kind) bool {
	for _, p := range problems {
		if p.Kind == k {
			return true
		}
	}
	return false
}

func kinds(problems []Problem) []Kind {
	out := make([]Kind, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.Kind)
	}
	return out
}

// --- Storage ---------------------------------------------------------------

func pendingPod(ns, name string) corev1.Pod {
	p := basePod(ns, name)
	p.Status.Phase = corev1.PodPending
	return p
}

func withPVC(p *corev1.Pod, volume, claim string) {
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
		Name:         volume,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
	})
}

func unschedulable(p *corev1.Pod, message string) {
	p.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: message,
	}}
}

func TestUnboundClaimIsPVCPendingNotUnschedulable(t *testing.T) {
	// A volume that will not bind and a node that is out of CPU are both
	// "Unschedulable" to the scheduler, but the fixes have nothing in common, so
	// they must not collapse into one kind.
	pod := pendingPod("data", "db-0")
	withPVC(&pod, "data", "db-data-0")
	unschedulable(&pod, "0/3 nodes are available: 3 pod has unbound immediate PersistentVolumeClaims.")

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())

	if !hasKind(problems, KindPVCPending) {
		t.Fatalf("expected PVCPending, got %v", kinds(problems))
	}
	if hasKind(problems, KindUnschedulable) {
		t.Fatalf("PVCPending must replace Unschedulable, not accompany it: %v", kinds(problems))
	}
}

func TestCapacityShortfallStaysUnschedulable(t *testing.T) {
	pod := pendingPod("api", "web-1")
	unschedulable(&pod, "0/3 nodes are available: 3 Insufficient cpu.")

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())

	if !hasKind(problems, KindUnschedulable) {
		t.Fatalf("expected Unschedulable, got %v", kinds(problems))
	}
	if hasKind(problems, KindPVCPending) {
		t.Fatalf("a CPU shortfall is not a storage fault: %v", kinds(problems))
	}
}

func TestVolumeNodeAffinityConflictIsStorage(t *testing.T) {
	pod := pendingPod("data", "db-0")
	withPVC(&pod, "data", "db-data-0")
	unschedulable(&pod, "0/3 nodes are available: 3 node(s) had volume node affinity conflict.")

	if !hasKind(Pods([]corev1.Pod{pod}, DefaultOptions()), KindPVCPending) {
		t.Fatal("a volume node affinity conflict is a storage fault")
	}
}

// creatingPod is scheduled to a node but still setting up its containers.
func creatingPod(ns, name string, scheduledAgo time.Duration) corev1.Pod {
	p := pendingPod(ns, name)
	p.Spec.NodeName = "node-1"
	p.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-scheduledAgo)),
	}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}
	return p
}

func TestStuckContainerCreatingWithClaimIsVolumeMountFailed(t *testing.T) {
	pod := creatingPod("data", "db-0", 10*time.Minute)
	withPVC(&pod, "data", "db-data-0")

	problems := Pods([]corev1.Pod{pod}, DefaultOptions())

	if !hasKind(problems, KindVolumeMountFailed) {
		t.Fatalf("expected VolumeMountFailed, got %v", kinds(problems))
	}
	for _, p := range problems {
		if p.Kind == KindVolumeMountFailed && !strings.Contains(p.Message, "db-data-0") {
			t.Fatalf("the message should name the claim, got %q", p.Message)
		}
	}
}

func TestContainerCreatingWithinGraceIsNotFlagged(t *testing.T) {
	// Attaching a cloud disk legitimately takes tens of seconds. Flagging that
	// would make every rollout of a stateful workload alert.
	pod := creatingPod("data", "db-0", 30*time.Second)
	withPVC(&pod, "data", "db-data-0")

	if hasKind(Pods([]corev1.Pod{pod}, DefaultOptions()), KindVolumeMountFailed) {
		t.Fatal("a pod still inside the mount grace must not be flagged")
	}
}

func TestContainerCreatingWithoutMountableVolumeIsNotStorage(t *testing.T) {
	// A pod stuck in ContainerCreating with only an emptyDir is almost always a
	// CNI or image problem. Calling it a volume failure sends the reader the
	// wrong way.
	pod := creatingPod("api", "web-1", 10*time.Minute)
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}

	if hasKind(Pods([]corev1.Pod{pod}, DefaultOptions()), KindVolumeMountFailed) {
		t.Fatalf("an emptyDir-only pod is not a storage fault: %v", kinds(Pods([]corev1.Pod{pod}, DefaultOptions())))
	}
}

func TestSecretVolumeCountsAsMountable(t *testing.T) {
	// A missing secret wedges the kubelet exactly like a missing PVC does.
	pod := creatingPod("api", "web-1", 10*time.Minute)
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "creds",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "api-creds"}},
	}}

	if !hasKind(Pods([]corev1.Pod{pod}, DefaultOptions()), KindVolumeMountFailed) {
		t.Fatal("a stuck secret volume mount is a storage fault")
	}
}

func TestRunningPodIsNeverVolumeMountFailed(t *testing.T) {
	pod := basePod("data", "db-0")
	pod.Spec.NodeName = "node-1"
	pod.Status.Phase = corev1.PodRunning
	withPVC(&pod, "data", "db-data-0")

	if hasKind(Pods([]corev1.Pod{pod}, DefaultOptions()), KindVolumeMountFailed) {
		t.Fatal("a running pod has already mounted its volumes")
	}
}

func TestStorageKinds(t *testing.T) {
	for _, k := range []Kind{KindPVCPending, KindVolumeMountFailed} {
		if !k.Storage() {
			t.Fatalf("%s must be classified as storage", k)
		}
	}
	for _, k := range []Kind{KindOOMKilled, KindUnschedulable, KindCPUPressure, KindEvicted} {
		if k.Storage() {
			t.Fatalf("%s must not be classified as storage", k)
		}
	}
}
