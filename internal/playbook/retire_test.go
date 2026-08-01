package playbook

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// retiring builds a book with short, legible windows so the tests read as
// intent rather than arithmetic.
func retiring() *Book {
	return New(Options{
		MaxEntries:    10,
		MaxFailures:   2,
		QuarantineFor: time.Hour,
		FailureDecay:  24 * time.Hour,
		MaxAge:        7 * 24 * time.Hour,
	})
}

func TestFailDropsTheRemedyImmediately(t *testing.T) {
	// One rollback is enough to stop replaying: whatever the failure history
	// says, a remedy that just did not hold must not be handed out again.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)

	removed, quarantined := b.Fail(ctrl, oom, t0.Add(time.Hour))
	if !removed {
		t.Fatal("the failing entry should have been removed")
	}
	if quarantined {
		t.Fatal("a single failure must not quarantine; that is what re-learning is for")
	}
	if _, ok := b.Lookup(ctrl, oom); ok {
		t.Fatal("a rolled-back remedy must not remain replayable")
	}
}

func TestOneFailureStillAllowsRelearning(t *testing.T) {
	// The common case: a fix stopped fitting because the workload changed. The
	// model diagnoses it afresh, the new fix holds, and we learn it.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0.Add(time.Hour))

	if !b.Record(ctrl, oom, `{"a":2}`, "high", t0.Add(2*time.Hour)) {
		t.Fatal("after a single failure the book must still be willing to learn")
	}
	if _, ok := b.Lookup(ctrl, oom); !ok {
		t.Fatal("the replacement remedy should be stored")
	}
}

func TestRepeatedFailuresQuarantineTheWorkload(t *testing.T) {
	// The case this feature exists for: podsmedic learns a fix, it fails, it
	// learns it again, it fails again. Each cycle is a real patch to a real
	// cluster, so the loop has to stop.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0.Add(time.Hour))
	b.Record(ctrl, oom, `{"a":2}`, "high", t0.Add(2*time.Hour))

	_, quarantined := b.Fail(ctrl, oom, t0.Add(3*time.Hour))
	if !quarantined {
		t.Fatal("the second failure inside the decay window must quarantine")
	}
	if b.Record(ctrl, oom, `{"a":3}`, "high", t0.Add(3*time.Hour+30*time.Minute)) {
		t.Fatal("a quarantined pair must not be learned, however well the heal went")
	}
	if b.Count() != 0 {
		t.Fatalf("nothing should be stored for a quarantined pair, got %d", b.Count())
	}
}

func TestQuarantineLiftsAndLearningResumes(t *testing.T) {
	// A quarantine is a pause, not a life sentence — the root cause may well be
	// fixed by the time it expires.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0.Add(time.Hour))
	b.Record(ctrl, oom, `{"a":2}`, "high", t0.Add(2*time.Hour))
	b.Fail(ctrl, oom, t0.Add(3*time.Hour))

	after := t0.Add(3*time.Hour + 61*time.Minute)
	if _, still := b.Quarantined(ctrl, oom, after); still {
		t.Fatal("the quarantine should have expired")
	}
	if !b.Record(ctrl, oom, `{"a":4}`, "high", after) {
		t.Fatal("learning must resume once the quarantine lifts")
	}
}

func TestQuarantineLengthensWithEachFailure(t *testing.T) {
	// A workload automated healing cannot fix should consume attempts at a
	// falling rate, not a fixed one.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0)
	b.Fail(ctrl, oom, t0.Add(time.Minute))
	first, ok := b.Quarantined(ctrl, oom, t0.Add(2*time.Minute))
	if !ok {
		t.Fatal("expected a quarantine after two failures")
	}
	b.Fail(ctrl, oom, t0.Add(2*time.Minute))
	second, ok := b.Quarantined(ctrl, oom, t0.Add(3*time.Minute))
	if !ok {
		t.Fatal("expected the quarantine to still be in force")
	}
	if !second.After(first) {
		t.Fatalf("a third failure must extend the quarantine: %s then %s", first, second)
	}
}

func TestOldFailuresStopCounting(t *testing.T) {
	// A workload that gave trouble last month must not be one slip away from a
	// quarantine today.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0)

	later := t0.Add(48 * time.Hour) // past FailureDecay
	b.Record(ctrl, oom, `{"a":2}`, "high", later)
	if _, quarantined := b.Fail(ctrl, oom, later.Add(time.Hour)); quarantined {
		t.Fatal("a failure older than the decay window must not count toward a quarantine")
	}
}

func TestRetireDropsUnconfirmedRemedies(t *testing.T) {
	// Nothing failed here. The remedy simply has not been proven in a long
	// time, and replaying an unproven fix is how podsmedic ends up treating a
	// cluster that no longer exists.
	b := retiring()
	b.Record("api/Deployment/stale", oom, `{"a":1}`, "high", t0)
	b.Record("api/Deployment/fresh", oom, `{"b":1}`, "high", t0.Add(6*24*time.Hour))

	retired := b.Retire(t0.Add(8 * 24 * time.Hour))
	if len(retired) != 1 || retired[0].Controller != "api/Deployment/stale" {
		t.Fatalf("expected only the stale remedy retired, got %+v", retired)
	}
	if _, ok := b.Lookup("api/Deployment/fresh", oom); !ok {
		t.Fatal("a recently confirmed remedy must survive")
	}
}

func TestRetirementIsNotAFailure(t *testing.T) {
	// Retiring must leave no scar: the remedy is re-learnable the moment it is
	// proven again.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Retire(t0.Add(8 * 24 * time.Hour))

	now := t0.Add(9 * 24 * time.Hour)
	if _, quarantined := b.Quarantined(ctrl, oom, now); quarantined {
		t.Fatal("retirement must not be held against the workload")
	}
	if !b.Record(ctrl, oom, `{"a":1}`, "high", now) {
		t.Fatal("a retired remedy must be immediately re-learnable")
	}
}

func TestRetireIsOffWhenMaxAgeIsZero(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	if got := b.Retire(t0.Add(5000 * time.Hour)); got != nil {
		t.Fatalf("a zero MaxAge means never retire, got %+v", got)
	}
}

func TestVerificationsAccumulate(t *testing.T) {
	// The track record is what /playbook reports, so a reader can tell a remedy
	// confirmed nine times from one confirmed once.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Record(ctrl, oom, `{"a":1}`, "high", t0.Add(time.Hour))
	b.Record(ctrl, oom, `{"a":1}`, "high", t0.Add(2*time.Hour))
	e, _ := b.Lookup(ctrl, oom)
	if e.Verifications != 3 {
		t.Fatalf("want 3 verifications, got %d", e.Verifications)
	}
}

func TestScarsSurviveRestore(t *testing.T) {
	// A flapping heal tends to cause the very restart that would otherwise
	// clear its quarantine, so the scars have to persist.
	b := retiring()
	b.Record(ctrl, oom, `{"a":1}`, "high", t0)
	b.Fail(ctrl, oom, t0)
	b.Fail(ctrl, oom, t0.Add(time.Minute))

	restored := retiring()
	restored.Restore(b.State())
	if _, ok := restored.Quarantined(ctrl, oom, t0.Add(2*time.Minute)); !ok {
		t.Fatal("the quarantine did not survive a restart")
	}
	if restored.Record(ctrl, oom, `{"a":9}`, "high", t0.Add(2*time.Minute)) {
		t.Fatal("a restored quarantine must still refuse to learn")
	}
}

func TestScarMapIsBounded(t *testing.T) {
	// Failures on a churning cluster must not grow the ConfigMap without limit.
	b := New(Options{MaxEntries: 5, MaxFailures: 99, FailureDecay: time.Hour})
	for i := 0; i < 50; i++ {
		b.Fail("ns/Deployment/w"+string(rune('a'+i%26))+string(rune('a'+i/26)), oom, t0.Add(time.Duration(i)*time.Second))
	}
	if n := len(b.Scars()); n > 5 {
		t.Fatalf("scar history grew to %d, past the %d cap", n, 5)
	}
}

func TestQuarantineCountReportsLiveQuarantinesOnly(t *testing.T) {
	b := retiring()
	b.Fail(ctrl, oom, t0)
	b.Fail(ctrl, oom, t0.Add(time.Minute))
	if n := b.QuarantineCount(t0.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("want 1 live quarantine, got %d", n)
	}
	if n := b.QuarantineCount(t0.Add(48 * time.Hour)); n != 0 {
		t.Fatalf("an expired quarantine must not be counted, got %d", n)
	}
}
