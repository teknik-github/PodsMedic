package heal

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/teknik-github/PodsMedic/internal/capacity"
	"github.com/teknik-github/PodsMedic/internal/detect"
	"github.com/teknik-github/PodsMedic/internal/k8s"
)

func testOpts() Options {
	return Options{
		MaxMemory:      resource.MustParse("4Gi"),
		MaxCPU:         resource.MustParse("4"),
		MaxMultiplier:  4.0,
		MinConfidence:  "high",
		AllowedKinds:   map[detect.Kind]bool{detect.KindOOMKilled: true},
		DenyNamespaces: map[string]bool{"kube-system": true},
	}
}

func oomBundle(container, memLimit string) *k8s.Bundle {
	return &k8s.Bundle{
		Problem: detect.Problem{Namespace: "api", Pod: "web-1", Container: container, Kind: detect.KindOOMKilled},
		Pod: k8s.PodSummary{
			OwnerKind: "ReplicaSet", OwnerName: "web-abc",
			Containers: []k8s.ContainerSummary{{
				Name:   container,
				Limits: map[string]string{"memory": memLimit},
			}},
		},
	}
}

func TestValidateAllowsBoundedMemoryBump(t *testing.T) {
	b := oomBundle("web", "128Mi")
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi", Reason: "heap needs more"}

	plan, err := Validate(b, "high", a, testOpts())
	if err != nil {
		t.Fatalf("expected a valid plan, got %v", err)
	}
	if plan.Kind != ActionPatchResources || plan.Limits["memory"] != "256Mi" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRejectsShrink(t *testing.T) {
	b := oomBundle("web", "512Mi")
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "128Mi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("shrinking a limit must be refused, got %v", err)
	}
}

func TestValidateRejectsOverMultiplier(t *testing.T) {
	b := oomBundle("web", "128Mi") // 4x cap = 512Mi
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "1Gi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a jump beyond the multiplier must be refused, got %v", err)
	}
}

func TestValidateRejectsOverAbsoluteCap(t *testing.T) {
	// Current is high enough that 4x would clear 4Gi; the absolute cap must
	// still bite regardless of the multiplier.
	b := oomBundle("web", "2Gi")
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "8Gi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("exceeding the absolute cap must be refused, got %v", err)
	}
}

func TestValidateRejectsLowConfidence(t *testing.T) {
	b := oomBundle("web", "128Mi")
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi"}

	if _, err := Validate(b, "medium", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a below-threshold confidence must not heal, got %v", err)
	}
}

func TestValidateRejectsDeniedNamespace(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Problem.Namespace = "kube-system"
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a denied namespace must not be healed, got %v", err)
	}
}

func TestValidateRejectsDisallowedKind(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Problem.Kind = detect.KindCrashLoopBackOff // not in the allowlist
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a problem kind outside the allowlist must not heal, got %v", err)
	}
}

func TestValidateNoneIsNoSafeAction(t *testing.T) {
	b := oomBundle("web", "128Mi")
	if _, err := Validate(b, "high", Action{Kind: ActionNone}, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("action none must map to ErrNoSafeAction, got %v", err)
	}
}

func TestValidateRejectsUnknownContainer(t *testing.T) {
	b := oomBundle("web", "128Mi")
	a := Action{Kind: ActionPatchResources, Container: "sidecar", MemoryLimit: "256Mi"}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a container not in the pod must be refused, got %v", err)
	}
}

func TestValidateAllowsBumpFromUnsetLimit(t *testing.T) {
	b := oomBundle("web", "") // no current limit
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi"}

	plan, err := Validate(b, "high", a, testOpts())
	if err != nil {
		t.Fatalf("setting a limit where none existed should be allowed within the cap, got %v", err)
	}
	if plan.Limits["memory"] != "256Mi" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRestartNeedsOwner(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Pod.OwnerKind = "" // bare pod
	a := Action{Kind: ActionRestartWorkload}

	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("restarting a bare pod must be refused, got %v", err)
	}
}

func TestValidateRejectsRequestBumpWhenDisabled(t *testing.T) {
	b := oomBundle("web", "128Mi")
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "256Mi"}

	// Default opts leave AllowRequests false.
	if _, err := Validate(b, "high", a, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a request bump must be refused unless requests are enabled, got %v", err)
	}
}

func TestValidateAllowsRequestBumpWhenEnabled(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Pod.Containers[0].Requests = map[string]string{"memory": "128Mi"}
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "256Mi"}

	opts := testOpts()
	opts.AllowRequests = true
	plan, err := Validate(b, "high", a, opts)
	if err != nil {
		t.Fatalf("an enabled, bounded request bump should pass, got %v", err)
	}
	if plan.Requests["memory"] != "256Mi" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRejectsUnschedulableRequest(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Pod.Containers[0].Requests = map[string]string{"memory": "512Mi"}
	// Node has only 1Gi allocatable; 4x of 512Mi = 2Gi exceeds it.
	b.Node = &k8s.NodeSummary{Name: "n1", Allocatable: map[string]string{"memory": "1Gi"}}
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "2Gi"}

	opts := testOpts()
	opts.AllowRequests = true
	if _, err := Validate(b, "high", a, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a request larger than node allocatable must be refused, got %v", err)
	}
}

func imgBundle(container, image string, kind detect.Kind) *k8s.Bundle {
	return &k8s.Bundle{
		Problem: detect.Problem{Namespace: "api", Pod: "web-1", Container: container, Kind: kind},
		Pod: k8s.PodSummary{
			OwnerKind:  "ReplicaSet",
			Containers: []k8s.ContainerSummary{{Name: container, Image: image}},
		},
	}
}

func imgOpts() Options {
	o := testOpts()
	o.AllowedKinds = map[detect.Kind]bool{detect.KindImagePullBackOff: true}
	return o
}

func TestValidateAllowsSameRepoTagFix(t *testing.T) {
	b := imgBundle("web", "ghcr.io/acme/web:v1.2.3", detect.KindImagePullBackOff)
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "ghcr.io/acme/web:v1.2.4", Reason: "typo'd tag"}

	plan, err := Validate(b, "high", a, imgOpts())
	if err != nil {
		t.Fatalf("a same-repo tag fix should be allowed, got %v", err)
	}
	if plan.Kind != ActionPatchImage || plan.Image != "ghcr.io/acme/web:v1.2.4" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRejectsDifferentRepo(t *testing.T) {
	b := imgBundle("web", "ghcr.io/acme/web:v1.2.3", detect.KindImagePullBackOff)
	// Same-looking name, different repo — the supply-chain attack the guard exists for.
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "ghcr.io/evil/web:v1.2.4"}

	if _, err := Validate(b, "high", a, imgOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a different repository must be refused, got %v", err)
	}
}

func TestValidateRejectsRegistrySwap(t *testing.T) {
	b := imgBundle("web", "ghcr.io/acme/web:v1.2.3", detect.KindImagePullBackOff)
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "docker.io/acme/web:v1.2.4"}

	if _, err := Validate(b, "high", a, imgOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a registry swap must be refused, got %v", err)
	}
}

func TestValidateRejectsLatestTag(t *testing.T) {
	b := imgBundle("web", "ghcr.io/acme/web:v1.2.3", detect.KindImagePullBackOff)
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "ghcr.io/acme/web:latest"}

	if _, err := Validate(b, "high", a, imgOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("the mutable latest tag must be refused, got %v", err)
	}
}

func TestValidateRejectsUnchangedImage(t *testing.T) {
	b := imgBundle("web", "ghcr.io/acme/web:v1.2.3", detect.KindImagePullBackOff)
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "ghcr.io/acme/web:v1.2.3"}

	if _, err := Validate(b, "high", a, imgOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("an identical image is a no-op and must be refused, got %v", err)
	}
}

func TestValidateAllowsDigestPinOnSameRepo(t *testing.T) {
	b := imgBundle("web", "registry.local:5000/acme/web:v1", detect.KindImagePullBackOff)
	// Registry with a port must not be mistaken for a tag; digest pin allowed.
	a := Action{Kind: ActionPatchImage, Container: "web", Image: "registry.local:5000/acme/web@sha256:" + strings.Repeat("a", 64)}

	if _, err := Validate(b, "high", a, imgOpts()); err != nil {
		t.Fatalf("a digest pin on the same repo should be allowed, got %v", err)
	}
}

func TestParseImageRef(t *testing.T) {
	cases := []struct{ in, repo, tag, digest string }{
		{"ghcr.io/acme/web:v1.2.3", "ghcr.io/acme/web", "v1.2.3", ""},
		{"registry.local:5000/acme/web:v1", "registry.local:5000/acme/web", "v1", ""},
		{"acme/web", "acme/web", "", ""},
		{"acme/web@sha256:abc", "acme/web", "", "sha256:abc"},
	}
	for _, c := range cases {
		repo, tag, digest := parseImageRef(c.in)
		if repo != c.repo || tag != c.tag || digest != c.digest {
			t.Fatalf("parseImageRef(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, repo, tag, digest, c.repo, c.tag, c.digest)
		}
	}
}

func probeBundle(container string, p *k8s.ProbeInfo, kind detect.Kind) *k8s.Bundle {
	return &k8s.Bundle{
		Problem: detect.Problem{Namespace: "api", Pod: "web-1", Container: container, Kind: kind},
		Pod: k8s.PodSummary{
			OwnerKind:  "ReplicaSet",
			Containers: []k8s.ContainerSummary{{Name: container, Probes: map[string]*k8s.ProbeInfo{"liveness": p}}},
		},
	}
}

func probeOpts() Options {
	o := testOpts()
	o.AllowedKinds = map[detect.Kind]bool{detect.KindCrashLoopBackOff: true}
	o.MaxProbeInitialDelaySeconds = 600
	o.MaxProbeFailureThreshold = 20
	return o
}

func TestValidateLoosensProbe(t *testing.T) {
	b := probeBundle("web", &k8s.ProbeInfo{Target: "http-get :8080/health", InitialDelaySeconds: 0, FailureThreshold: 3}, detect.KindCrashLoopBackOff)
	a := Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "liveness", ProbeInitialDelaySeconds: 30, Reason: "app needs ~25s to start"}

	plan, err := Validate(b, "high", a, probeOpts())
	if err != nil {
		t.Fatalf("loosening a probe should be allowed, got %v", err)
	}
	if plan.Kind != ActionPatchProbe || plan.ProbeType != "liveness" || plan.Probe["initialDelaySeconds"] != 30 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRejectsProbeTightening(t *testing.T) {
	b := probeBundle("web", &k8s.ProbeInfo{InitialDelaySeconds: 30, FailureThreshold: 3}, detect.KindCrashLoopBackOff)
	// Lowering initialDelay tightens the probe → refuse.
	a := Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "liveness", ProbeInitialDelaySeconds: 5}

	if _, err := Validate(b, "high", a, probeOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("tightening a probe must be refused, got %v", err)
	}
}

func TestValidateRejectsProbeOverCap(t *testing.T) {
	b := probeBundle("web", &k8s.ProbeInfo{InitialDelaySeconds: 10}, detect.KindCrashLoopBackOff)
	a := Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "liveness", ProbeInitialDelaySeconds: 100000} // effectively disabling

	if _, err := Validate(b, "high", a, probeOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a probe value past the cap must be refused, got %v", err)
	}
}

func TestValidateRejectsProbeNoChange(t *testing.T) {
	b := probeBundle("web", &k8s.ProbeInfo{InitialDelaySeconds: 30}, detect.KindCrashLoopBackOff)
	a := Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "liveness", ProbeInitialDelaySeconds: 30} // same value

	if _, err := Validate(b, "high", a, probeOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a no-op probe change must be refused, got %v", err)
	}
}

func TestValidateRejectsMissingProbe(t *testing.T) {
	b := probeBundle("web", &k8s.ProbeInfo{InitialDelaySeconds: 5}, detect.KindCrashLoopBackOff)
	a := Action{Kind: ActionPatchProbe, Container: "web", ProbeType: "readiness", ProbeInitialDelaySeconds: 30} // no readiness probe exists

	if _, err := Validate(b, "high", a, probeOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("adjusting a nonexistent probe must be refused, got %v", err)
	}
}

func TestValidateCPUScalesInMilliUnits(t *testing.T) {
	b := &k8s.Bundle{
		Problem: detect.Problem{Namespace: "api", Pod: "web-1", Container: "web", Kind: detect.KindOOMKilled},
		Pod: k8s.PodSummary{
			OwnerKind:  "ReplicaSet",
			Containers: []k8s.ContainerSummary{{Name: "web", Limits: map[string]string{"cpu": "250m"}}},
		},
	}
	// 250m * 4 = 1000m = "1"; 900m is within bounds, 1500m is not.
	if _, err := Validate(b, "high", Action{Kind: ActionPatchResources, Container: "web", CPULimit: "900m"}, testOpts()); err != nil {
		t.Fatalf("900m is within 4x of 250m, got %v", err)
	}
	if _, err := Validate(b, "high", Action{Kind: ActionPatchResources, Container: "web", CPULimit: "1500m"}, testOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("1500m exceeds 4x of 250m, expected refusal, got %v", err)
	}
}

func scaleOpts() Options {
	return Options{
		MaxMultiplier: 4.0,
		MinConfidence: "high",
		MaxReplicas:   10,
		AllowedKinds:  map[detect.Kind]bool{detect.KindCPUPressure: true},
	}
}

// autoScaleOpts derives the replica target instead of trusting the model's.
func autoScaleOpts() Options {
	o := scaleOpts()
	o.AutoReplicas = true
	o.TargetCPURatio = 0.70
	o.MaxReplicas = 0 // no hand-set backstop: capacity and the multiplier govern
	return o
}

// roomyCluster has space for far more replicas than any test asks for, so a
// test that is not about capacity is not accidentally bounded by it.
func roomyCluster() *capacity.Snapshot {
	return &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 64000, AllocMemBytes: 256 << 30, AllocPods: 110, Schedulable: true},
		{Name: "n2", AllocCPUMilli: 64000, AllocMemBytes: 256 << 30, AllocPods: 110, Schedulable: true},
	}}
}

func scaleBundle(replicas int32) *k8s.Bundle {
	return &k8s.Bundle{
		Problem:     detect.Problem{Namespace: "api", Pod: "web-1", Container: "web", Kind: detect.KindCPUPressure},
		Pod:         k8s.PodSummary{OwnerKind: "ReplicaSet", OwnerName: "web-abc"},
		Replicas:    replicas,
		Capacity:    roomyCluster(),
		PodRequests: capacity.Requests{CPUMilli: 500, MemBytes: 512 << 20},
	}
}

// loadedBundle is a workload whose replicas are pinned near their CPU limit.
func loadedBundle(replicas int32, usedMilli, limitMilli int64) *k8s.Bundle {
	b := scaleBundle(replicas)
	b.Load = &capacity.Load{
		Replicas: replicas, Sampled: replicas,
		CPUMilli: usedMilli, RefMilli: limitMilli, RefIsLimit: true,
	}
	return b
}

func TestValidateScaleUpBounded(t *testing.T) {
	b := scaleBundle(2)
	a := Action{Kind: ActionScaleReplicas, Replicas: 4, Reason: "cpu throttled"}
	plan, err := Validate(b, "high", a, scaleOpts())
	if err != nil {
		t.Fatalf("expected a valid plan, got %v", err)
	}
	if plan.Kind != ActionScaleReplicas || plan.Replicas != 4 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateScaleRejectsDown(t *testing.T) {
	b := scaleBundle(4)
	a := Action{Kind: ActionScaleReplicas, Replicas: 2}
	if _, err := Validate(b, "high", a, scaleOpts()); err == nil {
		t.Fatal("scaling down must be rejected")
	}
}

func TestValidateScaleRejectsOverCap(t *testing.T) {
	b := scaleBundle(3)
	a := Action{Kind: ActionScaleReplicas, Replicas: 12} // cap 10
	if _, err := Validate(b, "high", a, scaleOpts()); err == nil {
		t.Fatal("scaling past MaxReplicas must be rejected")
	}
}

func TestValidateScaleRejectsOverMultiplier(t *testing.T) {
	b := scaleBundle(2)
	a := Action{Kind: ActionScaleReplicas, Replicas: 9} // >4x of 2
	if _, err := Validate(b, "high", a, scaleOpts()); err == nil {
		t.Fatal("scaling past the multiplier must be rejected")
	}
}

func TestValidateScaleRejectsUnknownReplicas(t *testing.T) {
	b := scaleBundle(0) // not scalable / unknown
	a := Action{Kind: ActionScaleReplicas, Replicas: 3}
	if _, err := Validate(b, "high", a, scaleOpts()); err == nil {
		t.Fatal("an unknown replica count must be rejected")
	}
}

func TestValidateScaleDisabledWhenNoCap(t *testing.T) {
	b := scaleBundle(2)
	a := Action{Kind: ActionScaleReplicas, Replicas: 4}
	opts := scaleOpts()
	opts.MaxReplicas = 0 // and AutoReplicas is off: nothing left to derive a target from
	if _, err := Validate(b, "high", a, opts); err == nil {
		t.Fatal("scaling with no cap and no auto target must be rejected")
	}
}

// --- Capacity gate -------------------------------------------------------
//
// The gate exists so a scale-up can never add pods the cluster cannot place.
// Its most important property is that missing evidence refuses rather than
// assumes room.

func TestValidateScaleRefusesWithoutCapacityEvidence(t *testing.T) {
	b := scaleBundle(2)
	b.Capacity = nil // node or pod list denied
	a := Action{Kind: ActionScaleReplicas, Replicas: 4}

	if _, err := Validate(b, "high", a, scaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("scaling without capacity evidence must be refused, got %v", err)
	}
}

func TestValidateScaleRefusesWhenClusterIsFull(t *testing.T) {
	b := scaleBundle(3)
	// Both nodes are effectively full: 4000m allocatable, 3900m already reserved.
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 4000, UsedCPUMilli: 3900, AllocMemBytes: 64 << 30, AllocPods: 110, Schedulable: true},
	}}
	a := Action{Kind: ActionScaleReplicas, Replicas: 5}

	if _, err := Validate(b, "high", a, scaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("scaling into a full cluster must be refused, got %v", err)
	}
}

func TestValidateScaleTrimsTargetToClusterHeadroom(t *testing.T) {
	b := loadedBundle(4, 3800, 4000) // 95% of limit → derives ceil(4 × .95/.70) = 6

	// Room for exactly 2 more 500m replicas: the derived 6 is reachable.
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 1000, AllocMemBytes: 64 << 30, AllocPods: 110, Schedulable: true},
	}}
	plan, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts())
	if err != nil {
		t.Fatalf("expected a plan, got %v", err)
	}
	if plan.Replicas != 6 {
		t.Fatalf("expected 6 replicas (4 current + 2 that fit), got %d", plan.Replicas)
	}

	// Tighten to a single free slot: the same demand must now be trimmed to 5.
	b.Capacity.Nodes[0].AllocCPUMilli = 500
	plan, err = Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts())
	if err != nil {
		t.Fatalf("expected a trimmed plan, got %v", err)
	}
	if plan.Replicas != 5 {
		t.Fatalf("expected the target trimmed to 5 by headroom, got %d", plan.Replicas)
	}
	if !strings.Contains(plan.Summary, "headroom") {
		t.Fatalf("expected the summary to say the target was trimmed, got %q", plan.Summary)
	}
}

func TestValidateScaleRespectsPodSlotExhaustion(t *testing.T) {
	// Plenty of CPU and memory, but the node is at its pod limit. Pod count is
	// the resource that actually makes a kubelet unresponsive.
	b := loadedBundle(4, 3800, 4000)
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 64000, AllocMemBytes: 256 << 30, AllocPods: 110, UsedPods: 110, Schedulable: true},
	}}

	if _, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("scaling with no pod slots left must be refused, got %v", err)
	}
}

func TestValidateScaleIgnoresCordonedNodeCapacity(t *testing.T) {
	b := loadedBundle(2, 1900, 2000)
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "drained", AllocCPUMilli: 64000, AllocMemBytes: 256 << 30, AllocPods: 110, Schedulable: false},
	}}

	if _, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a cordoned node's free space is not real headroom, got %v", err)
	}
}

// --- Derived replica target ----------------------------------------------

func TestValidateScaleDerivesTargetFromLoad(t *testing.T) {
	// 4 replicas at 95% of their CPU limit, 70% target → 6.
	b := loadedBundle(4, 3800, 4000)

	// The model proposes nothing at all: the number must still be derived.
	plan, err := Validate(b, "high", Action{Kind: ActionScaleReplicas, Reason: "throttled"}, autoScaleOpts())
	if err != nil {
		t.Fatalf("expected a derived plan, got %v", err)
	}
	if plan.Replicas != 6 {
		t.Fatalf("expected 6 replicas from the utilisation formula, got %d", plan.Replicas)
	}
	if !strings.Contains(plan.Summary, "95%") {
		t.Fatalf("expected the summary to show the arithmetic, got %q", plan.Summary)
	}
}

func TestValidateScaleRefusesWithoutLoadEvidence(t *testing.T) {
	b := scaleBundle(4) // no Load: metrics-server absent
	a := Action{Kind: ActionScaleReplicas, Replicas: 8}

	if _, err := Validate(b, "high", a, autoScaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a derived target with no measurements must be refused, got %v", err)
	}
}

func TestValidateScaleDeclinesWhenLoadIsBelowTarget(t *testing.T) {
	// 30% utilisation needs no more replicas, whatever the model says.
	b := loadedBundle(4, 1200, 4000)
	a := Action{Kind: ActionScaleReplicas, Replicas: 8}

	if _, err := Validate(b, "high", a, autoScaleOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("scaling a workload that is not under pressure must be refused, got %v", err)
	}
}

func TestValidateScaleModelMayLowerButNotRaiseDerivedTarget(t *testing.T) {
	b := loadedBundle(4, 3800, 4000) // derives 6

	// A more conservative proposal is honoured.
	plan, err := Validate(b, "high", Action{Kind: ActionScaleReplicas, Replicas: 5}, autoScaleOpts())
	if err != nil {
		t.Fatalf("expected a plan, got %v", err)
	}
	if plan.Replicas != 5 {
		t.Fatalf("expected the model's more conservative 5, got %d", plan.Replicas)
	}

	// An inflated proposal — the shape prompt injection would take — is ignored.
	plan, err = Validate(b, "high", Action{Kind: ActionScaleReplicas, Replicas: 500}, autoScaleOpts())
	if err != nil {
		t.Fatalf("expected a plan, got %v", err)
	}
	if plan.Replicas != 6 {
		t.Fatalf("the model must not raise the derived target: got %d, want 6", plan.Replicas)
	}
}

func TestValidateScaleClampsDerivedTargetToMultiplier(t *testing.T) {
	// 1 replica at 100% of limit, 10% target → wants 10, but the 4x step limit
	// allows only 4. A derived target clamps (partial relief now) rather than
	// refusing outright.
	b := loadedBundle(1, 1000, 1000)
	opts := autoScaleOpts()
	opts.TargetCPURatio = 0.10

	plan, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, opts)
	if err != nil {
		t.Fatalf("expected a clamped plan, got %v", err)
	}
	if plan.Replicas != 4 {
		t.Fatalf("expected the 4x step limit to clamp to 4, got %d", plan.Replicas)
	}
}

func TestValidateScaleHonoursExplicitBackstop(t *testing.T) {
	b := loadedBundle(4, 3800, 4000) // derives 6
	opts := autoScaleOpts()
	opts.MaxReplicas = 5 // hand-set backstop stays a hard ceiling over the derived target

	plan, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, opts)
	if err != nil {
		t.Fatalf("expected a capped plan, got %v", err)
	}
	if plan.Replicas != 5 {
		t.Fatalf("expected the backstop of 5 to win over the derived 6, got %d", plan.Replicas)
	}
}

// --- Request raises are capacity-gated too -------------------------------

func TestValidateRequestRaiseRejectedWhenClusterHasNoRoom(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Pod.Containers[0].Requests = map[string]string{"memory": "512Mi"}
	b.PodRequests = capacity.Requests{MemBytes: 512 << 20}
	// 1Gi allocatable, 900Mi already committed: a 2Gi pod fits nowhere.
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 4000, AllocMemBytes: 1 << 30, UsedMemBytes: 900 << 20, AllocPods: 110, Schedulable: true},
	}}
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "2Gi"}

	opts := testOpts()
	opts.AllowRequests = true
	if _, err := Validate(b, "high", a, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a request raise with no cluster headroom must be refused, got %v", err)
	}
}

func TestValidateRequestRaiseAllowedWhenClusterHasRoom(t *testing.T) {
	b := oomBundle("web", "128Mi")
	b.Pod.Containers[0].Requests = map[string]string{"memory": "512Mi"}
	b.PodRequests = capacity.Requests{MemBytes: 512 << 20}
	b.Capacity = roomyCluster()
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "1Gi"}

	opts := testOpts()
	opts.AllowRequests = true
	plan, err := Validate(b, "high", a, opts)
	if err != nil {
		t.Fatalf("a request raise the cluster can absorb should pass, got %v", err)
	}
	if plan.Requests["memory"] != "1Gi" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRequestRaiseUsesProjectedPodTotal(t *testing.T) {
	// The container's own new request (600Mi) fits easily, but the pod already
	// reserves 3Gi for its other containers, so the projected 3.5Gi does not.
	// Checking per-resource-per-container would wrongly approve this.
	b := oomBundle("web", "128Mi")
	b.Pod.Containers[0].Requests = map[string]string{"memory": "100Mi"}
	b.PodRequests = capacity.Requests{MemBytes: 3 << 30}
	b.Capacity = &capacity.Snapshot{Nodes: []capacity.Node{
		{Name: "n1", AllocCPUMilli: 8000, AllocMemBytes: 3200 << 20, AllocPods: 110, Schedulable: true},
	}}
	a := Action{Kind: ActionPatchResources, Container: "web", MemoryRequest: "600Mi"}

	opts := testOpts()
	opts.AllowRequests = true
	if _, err := Validate(b, "high", a, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("the pod's projected total must be what is checked, got %v", err)
	}
}

// --- Storage is never healed ---------------------------------------------

func storageBundle(kind detect.Kind, claims ...k8s.ClaimSummary) *k8s.Bundle {
	return &k8s.Bundle{
		Problem:  detect.Problem{Namespace: "data", Pod: "db-0", Kind: kind},
		Pod:      k8s.PodSummary{OwnerKind: "ReplicaSet", OwnerName: "db-abc"},
		Replicas: 1,
		Claims:   claims,
	}
}

func storageOpts() Options {
	o := testOpts()
	o.AllowedKinds = map[detect.Kind]bool{detect.KindPVCPending: true, detect.KindVolumeMountFailed: true}
	o.AllowNamespaces = map[string]bool{"data": true}
	o.PVCAutoCreate = true
	o.PVCDefaultSize = "1Gi"
	o.PVCMaxSize = resource.MustParse("10Gi")
	return o
}

func TestValidateRefusesMutatingActionsOnStorageFaults(t *testing.T) {
	// Everything that would touch the volume itself stays refused, even with the
	// kind explicitly allowlisted. Only restart and create survive.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Phase: "Bound"})
	b.Pod.Containers = []k8s.ContainerSummary{{Name: "db", Limits: map[string]string{"memory": "128Mi"}}}
	b.Problem.Container = "db"

	opts := storageOpts()
	opts.AllowRequests = true
	for _, a := range []Action{
		{Kind: ActionPatchResources, Container: "db", MemoryLimit: "256Mi"},
		{Kind: ActionScaleReplicas, Replicas: 4},
		{Kind: ActionPatchProbe, Container: "db", ProbeType: "liveness", ProbeInitialDelaySeconds: 60},
		{Kind: ActionPatchImage, Container: "db", Image: "x/y:2"},
	} {
		_, err := Validate(b, "high", a, opts)
		if !errors.Is(err, ErrNoSafeAction) {
			t.Fatalf("%s on a storage fault must be refused, got %v", a.Kind, err)
		}
	}
}

// --- Recovery by restart, once the storage is healthy again ---------------

func TestValidateRestartRecoversWhenClaimsBound(t *testing.T) {
	// The volume came back; the pod is holding a stale mount. Restarting touches
	// no storage object at all, which is what makes it safe.
	b := storageBundle(detect.KindVolumeMountFailed, k8s.ClaimSummary{ClaimName: "db-data", Phase: "Bound"})

	plan, err := Validate(b, "high", Action{Kind: ActionRestartWorkload, Reason: "stale mount"}, storageOpts())
	if err != nil {
		t.Fatalf("expected a restart plan, got %v", err)
	}
	if plan.Kind != ActionRestartWorkload {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestValidateRestartRefusedWhileClaimStillUnbound(t *testing.T) {
	// Restarting now would just recreate a pod that cannot start — pure churn
	// that buries the real problem.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Phase: "Pending"})

	if _, err := Validate(b, "high", Action{Kind: ActionRestartWorkload}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a restart with an unbound claim must be refused, got %v", err)
	}
}

func TestValidateRestartRefusedWhenOneOfSeveralClaimsUnbound(t *testing.T) {
	b := storageBundle(detect.KindVolumeMountFailed,
		k8s.ClaimSummary{ClaimName: "a", Phase: "Bound"},
		k8s.ClaimSummary{ClaimName: "b", Phase: "Pending"},
	)

	if _, err := Validate(b, "high", Action{Kind: ActionRestartWorkload}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("one unbound claim is enough to refuse, got %v", err)
	}
}

func TestValidateRestartRefusedWithoutClaimEvidence(t *testing.T) {
	// No claim evidence (RBAC denied) means no way to confirm recovery, so the
	// restart is refused rather than fired hopefully.
	b := storageBundle(detect.KindVolumeMountFailed)

	if _, err := Validate(b, "high", Action{Kind: ActionRestartWorkload}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a restart without claim evidence must be refused, got %v", err)
	}
}

func TestValidateRestartUnaffectedForNonStorageProblems(t *testing.T) {
	// The bound-claims precondition applies only to storage faults; an ordinary
	// crash-loop restart must not start requiring claim evidence.
	b := oomBundle("web", "128Mi")
	b.Problem.Kind = detect.KindCrashLoopBackOff
	opts := testOpts()
	opts.AllowedKinds = map[detect.Kind]bool{detect.KindCrashLoopBackOff: true}

	if _, err := Validate(b, "high", Action{Kind: ActionRestartWorkload}, opts); err != nil {
		t.Fatalf("a non-storage restart should still pass, got %v", err)
	}
}

// --- Creating a missing claim --------------------------------------------

func TestValidateCreatesMissingClaim(t *testing.T) {
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})

	plan, err := Validate(b, "high", Action{Kind: ActionCreatePVC, Reason: "claim never created"}, storageOpts())
	if err != nil {
		t.Fatalf("expected a create plan, got %v", err)
	}
	if plan.Claim == nil {
		t.Fatal("plan carries no claim spec")
	}
	if plan.Claim.Name != "db-data" || plan.Claim.Namespace != "data" {
		t.Fatalf("claim identity must come from the pod spec: %+v", plan.Claim)
	}
	if plan.Claim.Size != "1Gi" || plan.Claim.AccessMode != "ReadWriteOnce" {
		t.Fatalf("unexpected claim spec: %+v", plan.Claim)
	}
}

func TestValidateCreateDisabledByDefault(t *testing.T) {
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})
	opts := storageOpts()
	opts.PVCAutoCreate = false

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("creating claims must be off unless enabled, got %v", err)
	}
}

func TestValidateCreateRequiresExplicitNamespaceAllowlist(t *testing.T) {
	// Stricter than every other action: elsewhere an empty allowlist means "all
	// namespaces", but provisioning storage cluster-wide by default is not a
	// reasonable reading of any configuration.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})
	opts := storageOpts()
	opts.AllowNamespaces = nil

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("an empty allowlist must refuse creation, got %v", err)
	}
}

func TestValidateNeverRecreatesAnExistingClaim(t *testing.T) {
	// The claim exists but is Pending. Creating is the only storage write there
	// is; a claim that exists is never touched, whatever state it is in.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Phase: "Pending"})

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("an existing claim must never be recreated, got %v", err)
	}
}

func TestValidateCreateRefusesAmbiguousMultipleClaims(t *testing.T) {
	b := storageBundle(detect.KindPVCPending,
		k8s.ClaimSummary{ClaimName: "a", Missing: true},
		k8s.ClaimSummary{ClaimName: "b", Missing: true},
	)

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("two missing claims are too ambiguous to create, got %v", err)
	}
}

func TestValidateCreateRefusesSharedClaimAcrossReplicas(t *testing.T) {
	// ReadWriteOnce would bind and then block every replica but one — worse than
	// the Pending pod being fixed.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})
	b.Replicas = 3

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, storageOpts()); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("a claim shared by several replicas has an ambiguous access mode, got %v", err)
	}
}

func TestValidateCreateCapsSizeAtMaximum(t *testing.T) {
	// A default above the ceiling is a misconfiguration, capped rather than
	// honoured — the ceiling is what bounds the cloud bill.
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})
	opts := storageOpts()
	opts.PVCDefaultSize = "500Gi"
	opts.PVCMaxSize = resource.MustParse("10Gi")

	plan, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, opts)
	if err != nil {
		t.Fatalf("expected a capped plan, got %v", err)
	}
	if plan.Claim.Size != "10Gi" {
		t.Fatalf("expected the size capped to 10Gi, got %s", plan.Claim.Size)
	}
}

func TestValidateCreateRejectedForNonStorageProblem(t *testing.T) {
	// Creating a volume is never the answer to an OOM.
	b := oomBundle("web", "128Mi")
	opts := storageOpts()
	opts.AllowedKinds[detect.KindOOMKilled] = true

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("create_pvc on an OOM must be refused, got %v", err)
	}
}

func TestValidateCreateStillHonoursNamespaceDeny(t *testing.T) {
	b := storageBundle(detect.KindPVCPending, k8s.ClaimSummary{ClaimName: "db-data", Missing: true})
	b.Problem.Namespace = "kube-system"
	opts := storageOpts()
	opts.AllowNamespaces = map[string]bool{"kube-system": true} // even if allowlisted

	if _, err := Validate(b, "high", Action{Kind: ActionCreatePVC}, opts); !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("the deny list must still win, got %v", err)
	}
}

// --- HPA guard -------------------------------------------------------------
//
// An HPA owns spec.replicas. Writing it too means two controllers overwriting
// each other every reconcile, so a scale heal must stand down — the same answer
// a GitOps-managed workload gets, for the same reason.

func TestValidateScaleRefusedWhenAnHPAOwnsReplicas(t *testing.T) {
	b := loadedBundle(4, 3800, 4000) // would otherwise derive 6
	b.Autoscaler = &k8s.AutoscalerRef{Name: "web", MinReplicas: 2, MaxReplicas: 10, Targets: "cpu 70%"}

	_, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts())
	if !errors.Is(err, ErrNoSafeAction) {
		t.Fatalf("scaling a workload an HPA manages must be refused, got %v", err)
	}
	// The refusal has to name the autoscaler and point at the real lever, or an
	// operator just sees a heal that mysteriously never fires.
	if !strings.Contains(err.Error(), "HorizontalPodAutoscaler") || !strings.Contains(err.Error(), "maxReplicas") {
		t.Fatalf("the refusal should name the HPA and the actual fix, got %q", err)
	}
}

func TestValidateScaleAllowedWithoutAnHPA(t *testing.T) {
	b := loadedBundle(4, 3800, 4000)
	b.Autoscaler = nil

	if _, err := Validate(b, "high", Action{Kind: ActionScaleReplicas}, autoScaleOpts()); err != nil {
		t.Fatalf("an unmanaged workload should still scale, got %v", err)
	}
}

func TestHPAOnlyBlocksScaling(t *testing.T) {
	// The conflict is over spec.replicas specifically. An HPA is no reason to
	// refuse a memory limit raise, an image fix, or a restart — those touch
	// fields it does not manage.
	b := oomBundle("web", "128Mi")
	b.Autoscaler = &k8s.AutoscalerRef{Name: "web", MaxReplicas: 10, Targets: "cpu 70%"}

	if _, err := Validate(b, "high", Action{Kind: ActionPatchResources, Container: "web", MemoryLimit: "256Mi"}, testOpts()); err != nil {
		t.Fatalf("an HPA must not block a resource patch, got %v", err)
	}

	b.Problem.Kind = detect.KindCrashLoopBackOff
	opts := testOpts()
	opts.AllowedKinds = map[detect.Kind]bool{detect.KindCrashLoopBackOff: true}
	if _, err := Validate(b, "high", Action{Kind: ActionRestartWorkload}, opts); err != nil {
		t.Fatalf("an HPA must not block a restart, got %v", err)
	}
}

func TestAutoscalerRefReadsWell(t *testing.T) {
	// The string lands in an alert an operator reads at 3am, so it has to carry
	// the name, what it scales on, and its ceiling.
	got := (&k8s.AutoscalerRef{Name: "web", MinReplicas: 2, MaxReplicas: 10, Targets: "cpu 70%"}).String()
	for _, want := range []string{"web", "cpu 70%", "2–10 replicas"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
}
