package breaker

import "testing"

func clusterOpts() ClusterOptions {
	return ClusterOptions{MaxPerSweep: 3, SurgeRatio: 0.25, SurgeMinWorkloads: 8}
}

func TestSurgeTripsOnSystemicFailure(t *testing.T) {
	// The case this exists for: a node dies and takes a third of the cluster with
	// it. Every per-workload limit would happily allow all of those heals,
	// because each workload is a different workload.
	tripped, why := Surge(10, 30, clusterOpts())
	if !tripped {
		t.Fatal("a third of the cluster failing must suspend healing")
	}
	for _, want := range []string{"10 of 30", "33%", "infrastructure"} {
		if !contains(why, want) {
			t.Fatalf("the reason must say what happened and why: %q", why)
		}
	}
}

func TestSurgeIgnoresOrdinaryFailures(t *testing.T) {
	// Two bad workloads out of thirty is a normal Tuesday and must still heal.
	if tripped, _ := Surge(2, 30, clusterOpts()); tripped {
		t.Fatal("ordinary failures must not suspend healing")
	}
}

func TestSurgeNeedsEnoughWorkloadsToMeanAnything(t *testing.T) {
	// One failure out of three is 33%, but on a three-workload cluster that is
	// one broken thing, not an outage. Without the floor, small clusters could
	// never heal at all.
	if tripped, _ := Surge(1, 3, clusterOpts()); tripped {
		t.Fatal("the ratio must not apply below the workload floor")
	}
	// The same ratio above the floor does trip.
	if tripped, _ := Surge(3, 9, clusterOpts()); !tripped {
		t.Fatal("above the floor the ratio should apply")
	}
}

func TestSurgeDisabled(t *testing.T) {
	o := clusterOpts()
	o.SurgeRatio = 0
	if tripped, _ := Surge(30, 30, o); tripped {
		t.Fatal("a zero ratio must disable the check")
	}
	if tripped, _ := Surge(5, 0, clusterOpts()); tripped {
		t.Fatal("no workloads means nothing to conclude")
	}
}

func TestBudgetCapsHealsPerSweep(t *testing.T) {
	b := NewBudget(3)
	for i := 1; i <= 3; i++ {
		if !b.Take() {
			t.Fatalf("heal %d should fit in the allowance", i)
		}
	}
	if b.Take() {
		t.Fatal("the fourth heal must be refused")
	}
	if b.Used() != 3 {
		t.Fatalf("expected 3 used, got %d", b.Used())
	}
}

func TestBudgetUnlimitedAndNil(t *testing.T) {
	// Zero preserves the pre-existing unbounded behaviour for anyone who wants it.
	b := NewBudget(0)
	for i := 0; i < 100; i++ {
		if !b.Take() {
			t.Fatal("a non-positive max means unlimited")
		}
	}
	var nilBudget *Budget
	if !nilBudget.Take() {
		t.Fatal("a nil budget must not block")
	}
	if nilBudget.Used() != 0 || nilBudget.Max() != 0 {
		t.Fatal("a nil budget reports zeroes rather than panicking")
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
