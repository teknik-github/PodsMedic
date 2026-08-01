package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func reqs(cpu, mem string) corev1.ResourceRequirements {
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return corev1.ResourceRequirements{Requests: rl}
}

func TestPodRequestsSumsContainers(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "app", Resources: reqs("250m", "256Mi")},
		{Name: "sidecar", Resources: reqs("100m", "64Mi")},
	}}}

	got := PodRequests(pod)
	if got.CPUMilli != 350 {
		t.Fatalf("expected 350m, got %dm", got.CPUMilli)
	}
	if got.MemBytes != 320<<20 {
		t.Fatalf("expected 320Mi, got %dMi", got.MemBytes>>20)
	}
}

func TestPodRequestsTakesInitContainerPeakNotSum(t *testing.T) {
	// Ordinary init containers run one at a time before the app starts, so the
	// pod needs the largest of them — not their sum. Summing would overstate the
	// pod's footprint and wrongly shrink the cluster headroom we compute.
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     []corev1.Container{{Name: "app", Resources: reqs("100m", "128Mi")}},
		InitContainers: []corev1.Container{{Name: "a", Resources: reqs("2", "1Gi")}, {Name: "b", Resources: reqs("500m", "256Mi")}},
	}}

	got := PodRequests(pod)
	if got.CPUMilli != 2000 {
		t.Fatalf("expected the 2000m init peak to dominate, got %dm", got.CPUMilli)
	}
	if got.MemBytes != 1<<30 {
		t.Fatalf("expected the 1Gi init peak, got %dMi", got.MemBytes>>20)
	}
}

func TestPodRequestsAddsNativeSidecars(t *testing.T) {
	// An init container with restartPolicy: Always is a native sidecar — it runs
	// for the pod's whole life, so it is additive rather than a peak.
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Resources: reqs("500m", "512Mi")}},
		InitContainers: []corev1.Container{
			{Name: "proxy", RestartPolicy: &always, Resources: reqs("200m", "128Mi")},
			{Name: "migrate", Resources: reqs("100m", "64Mi")},
		},
	}}

	got := PodRequests(pod)
	// app 500m + sidecar 200m = 700m; the migrate peak (200+100=300m) is lower.
	if got.CPUMilli != 700 {
		t.Fatalf("expected 700m, got %dm", got.CPUMilli)
	}
	if got.MemBytes != 640<<20 {
		t.Fatalf("expected 640Mi, got %dMi", got.MemBytes>>20)
	}
}

func TestPodRequestsIncludesOverhead(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Resources: reqs("100m", "128Mi")}},
		Overhead:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
	}}

	got := PodRequests(pod)
	if got.CPUMilli != 150 || got.MemBytes != 160<<20 {
		t.Fatalf("expected overhead to be added, got %dm / %dMi", got.CPUMilli, got.MemBytes>>20)
	}
}

func TestPodRequestsBestEffortIsZero(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	if got := PodRequests(pod); !got.IsZero() {
		t.Fatalf("expected zero requests, got %+v", got)
	}
}

func TestNodeSchedulable(t *testing.T) {
	ready := func(status corev1.ConditionStatus) []corev1.NodeCondition {
		return []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	}
	cases := []struct {
		name string
		node corev1.Node
		want bool
	}{
		{"ready", corev1.Node{Status: corev1.NodeStatus{Conditions: ready(corev1.ConditionTrue)}}, true},
		{"not ready", corev1.Node{Status: corev1.NodeStatus{Conditions: ready(corev1.ConditionFalse)}}, false},
		{"cordoned", corev1.Node{
			Spec:   corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{Conditions: ready(corev1.ConditionTrue)},
		}, false},
		{"no ready condition", corev1.Node{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeSchedulable(&tc.node); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestHoldsResources(t *testing.T) {
	// Terminal pods release their node's capacity; everything else, including a
	// terminating pod, still holds it.
	cases := map[corev1.PodPhase]bool{
		corev1.PodRunning:   true,
		corev1.PodPending:   true,
		corev1.PodSucceeded: false,
		corev1.PodFailed:    false,
	}
	for phase, want := range cases {
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		if got := holdsResources(pod); got != want {
			t.Fatalf("phase %s: expected %v, got %v", phase, want, got)
		}
	}
}
