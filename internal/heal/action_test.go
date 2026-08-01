package heal

import (
	"strings"
	"testing"
)

func TestActionDescribe(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		want   string
	}{
		{
			"resource raise",
			Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "512Mi"},
			"patch_resources: container web, memory limit → 512Mi",
		},
		{
			"several resources keep a stable order",
			Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "512Mi", CPULimit: "1", MemoryRequest: "256Mi"},
			"patch_resources: container web, memory limit → 512Mi, cpu limit → 1, memory request → 256Mi",
		},
		{
			"image correction",
			Action{Kind: ActionPatchImage, Container: "web", Image: "ghcr.io/acme/web:1.4.2"},
			"patch_image: container web, image → ghcr.io/acme/web:1.4.2",
		},
		{
			"probe loosening",
			Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "liveness", ProbeInitialDelaySeconds: 60},
			"patch_probe: container web, loosen liveness probe",
		},
		{
			"restart carries no fields",
			Action{Kind: ActionRestartWorkload},
			"restart_workload",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.action.Describe(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActionDescribeHidesStaleReplicaCount(t *testing.T) {
	// A remembered scale_replicas must not advertise the count it once used: the
	// number is re-derived from live load at replay, so printing "→ 6" would tell
	// an operator something that is not going to happen.
	got := Action{Kind: ActionScaleReplicas, Replicas: 6}.Describe()
	if strings.Contains(got, "6") {
		t.Fatalf("the stale replica count must not appear, got %q", got)
	}
	if !strings.Contains(got, "re-derived") {
		t.Fatalf("expected the description to say the count is re-derived, got %q", got)
	}
}
