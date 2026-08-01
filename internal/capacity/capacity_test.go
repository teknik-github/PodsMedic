package capacity

import (
	"encoding/json"
	"errors"
	"testing"
)

const gi = int64(1) << 30

// node builds a schedulable node with the given allocatable and used amounts.
func node(name string, allocCPU, usedCPU int64, allocMemGi, usedMemGi int64, allocPods, usedPods int64) Node {
	return Node{
		Name:          name,
		AllocCPUMilli: allocCPU, UsedCPUMilli: usedCPU,
		AllocMemBytes: allocMemGi * gi, UsedMemBytes: usedMemGi * gi,
		AllocPods: allocPods, UsedPods: usedPods,
		Schedulable: true,
	}
}

func TestFitAdditionalBinPacksPerNode(t *testing.T) {
	// Two nodes with 1500m free each. A 1000m pod fits once per node, not three
	// times: summing free CPU cluster-wide and dividing would wrongly say 3.
	s := Snapshot{Nodes: []Node{
		node("a", 2000, 500, 8, 0, 110, 5),
		node("b", 2000, 500, 8, 0, 110, 5),
	}}

	if got := s.FitAdditional(Requests{CPUMilli: 1000}); got != 2 {
		t.Fatalf("expected 2 pods to fit (one per node), got %d", got)
	}
}

func TestFitAdditionalBoundedByPodSlots(t *testing.T) {
	// Vast CPU and memory, but only 3 pod slots left. Pod count is a resource:
	// exhausting it is what makes a kubelet stop being responsive.
	s := Snapshot{Nodes: []Node{node("a", 64000, 0, 256, 0, 110, 107)}}

	if got := s.FitAdditional(Requests{CPUMilli: 10, MemBytes: 1 << 20}); got != 3 {
		t.Fatalf("expected the pod-slot bound of 3, got %d", got)
	}
}

func TestFitAdditionalBestEffortStillBoundedBySlots(t *testing.T) {
	// A pod requesting nothing is not free: it still occupies a slot.
	s := Snapshot{Nodes: []Node{node("a", 4000, 0, 16, 0, 110, 100)}}

	if got := s.FitAdditional(Requests{}); got != 10 {
		t.Fatalf("expected 10 slots for BestEffort pods, got %d", got)
	}
}

func TestFitAdditionalTakesTightestResource(t *testing.T) {
	// CPU allows 4 more, memory only 2. The tighter bound wins.
	s := Snapshot{Nodes: []Node{node("a", 5000, 1000, 10, 6, 110, 5)}}

	if got := s.FitAdditional(Requests{CPUMilli: 1000, MemBytes: 2 * gi}); got != 2 {
		t.Fatalf("expected memory to bound the fit at 2, got %d", got)
	}
}

func TestFitAdditionalSkipsUnschedulableNodes(t *testing.T) {
	drained := node("cordoned", 8000, 0, 32, 0, 110, 0)
	drained.Schedulable = false
	s := Snapshot{Nodes: []Node{drained, node("ok", 2000, 1000, 8, 4, 110, 5)}}

	// Only the healthy node contributes: 1000m free / 500m = 2.
	if got := s.FitAdditional(Requests{CPUMilli: 500}); got != 2 {
		t.Fatalf("expected the cordoned node to be excluded, got %d", got)
	}
}

func TestReserveHoldsBackHeadroom(t *testing.T) {
	// 4000m allocatable, nothing used. Without a reserve, four 1000m pods fit.
	// A 25% reserve holds back 1000m, so only three do.
	nodes := []Node{node("a", 4000, 0, 64, 0, 110, 0)}

	if got := (Snapshot{Nodes: nodes}).FitAdditional(Requests{CPUMilli: 1000}); got != 4 {
		t.Fatalf("expected 4 with no reserve, got %d", got)
	}
	if got := (Snapshot{Nodes: nodes, Reserve: 0.25}).FitAdditional(Requests{CPUMilli: 1000}); got != 3 {
		t.Fatalf("expected 3 with a 25%% reserve, got %d", got)
	}
}

func TestReserveIsClamped(t *testing.T) {
	nodes := []Node{node("a", 4000, 0, 64, 0, 110, 0)}

	// A negative reserve must not manufacture extra headroom.
	if got := (Snapshot{Nodes: nodes, Reserve: -1}).FitAdditional(Requests{CPUMilli: 1000}); got != 4 {
		t.Fatalf("negative reserve should behave as zero, got %d", got)
	}
	// An absurd reserve must not silently disable scaling entirely.
	if got := (Snapshot{Nodes: nodes, Reserve: 5}).FitAdditional(Requests{CPUMilli: 100}); got == 0 {
		t.Fatal("a reserve above the clamp should still leave some headroom")
	}
}

func TestFitsRejectsOversizedPod(t *testing.T) {
	// 8Gi of memory free in total, but split across two nodes: a 6Gi pod fits on
	// neither, and aggregate arithmetic would wrongly say it does.
	s := Snapshot{Nodes: []Node{
		node("a", 4000, 0, 4, 0, 110, 0),
		node("b", 4000, 0, 4, 0, 110, 0),
	}}

	if err := s.Fits(Requests{MemBytes: 6 * gi}); err == nil {
		t.Fatal("expected a 6Gi pod to be rejected when no single node has 6Gi free")
	} else if !errors.Is(err, ErrNoHeadroom) {
		t.Fatalf("expected ErrNoHeadroom, got %v", err)
	}

	if err := s.Fits(Requests{MemBytes: 3 * gi}); err != nil {
		t.Fatalf("expected a 3Gi pod to fit on one node: %v", err)
	}
}

func TestSummaryAppliesReserveAndFindsLargestNode(t *testing.T) {
	s := Snapshot{
		Nodes: []Node{
			node("small", 2000, 1000, 8, 4, 110, 10),
			node("big", 8000, 1000, 32, 8, 110, 10),
		},
		Reserve: 0.10,
	}

	sum := s.Summary()
	if sum.Nodes != 2 || sum.SchedulableNodes != 2 {
		t.Fatalf("expected 2 nodes both schedulable, got %d/%d", sum.Nodes, sum.SchedulableNodes)
	}
	// small: 2000-1000-200 = 800; big: 8000-1000-800 = 6200.
	if sum.CPUFreeMilli != 7000 {
		t.Fatalf("expected 7000m free after the reserve, got %d", sum.CPUFreeMilli)
	}
	if sum.ReservePercent != 10 {
		t.Fatalf("expected the reserve to be reported as 10%%, got %d", sum.ReservePercent)
	}
	if sum.LargestFreeNode == "" || sum.LargestFreeNode[:3] != "big" {
		t.Fatalf("expected the largest free node to be %q, got %q", "big", sum.LargestFreeNode)
	}
}

func TestSummaryNeverReportsNegativeFree(t *testing.T) {
	// An overcommitted node (requests above allocatable) must clamp to zero, not
	// subtract from another node's headroom.
	s := Snapshot{Nodes: []Node{
		node("over", 1000, 4000, 4, 16, 110, 200),
		node("ok", 4000, 0, 16, 0, 110, 0),
	}}

	sum := s.Summary()
	if sum.CPUFreeMilli != 4000 {
		t.Fatalf("expected the overcommitted node to contribute 0, got total %dm", sum.CPUFreeMilli)
	}
	if sum.PodSlotsFree != 110 {
		t.Fatalf("expected 110 free slots, got %d", sum.PodSlotsFree)
	}
}

func TestTargetReplicasUsesUtilisationRatio(t *testing.T) {
	// Four replicas averaging 95% of a 1000m limit, target 70%:
	// ceil(4 × 0.95/0.70) = ceil(5.43) = 6.
	got, why, err := TargetReplicas(Load{
		Replicas: 4, Sampled: 4,
		CPUMilli: 3800, RefMilli: 4000, RefIsLimit: true,
	}, 0.70)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 6 {
		t.Fatalf("expected 6 replicas, got %d", got)
	}
	if why == "" {
		t.Fatal("expected a human explanation of the arithmetic")
	}
}

func TestTargetReplicasDeclinesWhenAlreadyBelowTarget(t *testing.T) {
	_, _, err := TargetReplicas(Load{
		Replicas: 3, Sampled: 3,
		CPUMilli: 1200, RefMilli: 3000, RefIsLimit: true,
	}, 0.70)
	if !errors.Is(err, ErrNotComputable) {
		t.Fatalf("expected a refusal at 40%% utilisation, got %v", err)
	}
}

func TestTargetReplicasRefusesWithoutEvidence(t *testing.T) {
	cases := []struct {
		name string
		load Load
	}{
		{"no replica count", Load{Replicas: 0, Sampled: 1, CPUMilli: 100, RefMilli: 100}},
		{"no samples", Load{Replicas: 3, Sampled: 0, CPUMilli: 0, RefMilli: 3000}},
		{"no cpu reference", Load{Replicas: 3, Sampled: 3, CPUMilli: 2000, RefMilli: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := TargetReplicas(tc.load, 0.70); !errors.Is(err, ErrNotComputable) {
				t.Fatalf("expected ErrNotComputable, got %v", err)
			}
		})
	}
}

func TestTargetReplicasRejectsBadTarget(t *testing.T) {
	l := Load{Replicas: 2, Sampled: 2, CPUMilli: 1900, RefMilli: 2000, RefIsLimit: true}
	for _, target := range []float64{0, -0.5, 1.5} {
		if _, _, err := TargetReplicas(l, target); !errors.Is(err, ErrNotComputable) {
			t.Fatalf("target %.2f should be rejected, got %v", target, err)
		}
	}
}

func TestTargetReplicasAlwaysIncreasesWhenOverTarget(t *testing.T) {
	// A single replica barely over target must still round up to 2, never stall
	// at the current count (which would leave the pressure unaddressed forever).
	got, _, err := TargetReplicas(Load{
		Replicas: 1, Sampled: 1,
		CPUMilli: 701, RefMilli: 1000, RefIsLimit: true,
	}, 0.70)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("expected 2 replicas, got %d", got)
	}
}

func TestSnapshotMarshalsAsSummary(t *testing.T) {
	// The bundle must not carry one JSON object per node: a 200-node cluster
	// would swamp the evidence with tokens that add no signal.
	s := Snapshot{Nodes: []Node{node("a", 1000, 0, 4, 0, 110, 0)}}
	raw, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); !contains(got, "schedulableNodes") || contains(got, `"Nodes"`) {
		t.Fatalf("expected a summary, got %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSnapshotSerialisesThroughPointer(t *testing.T) {
	// The evidence bundle holds *Snapshot, so the compact form must survive
	// json.Marshal on a pointer, not just a direct MarshalJSON call.
	s := &Snapshot{Nodes: []Node{node("a", 2000, 500, 8, 2, 110, 5)}, Reserve: 0.2}
	raw, err := json.Marshal(struct {
		Cap *Snapshot `json:"clusterCapacity"`
	}{s})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if !contains(got, "cpuFreeMillicores") || contains(got, `"Nodes"`) {
		t.Fatalf("expected the embedded pointer to marshal as a summary, got %s", got)
	}
}
