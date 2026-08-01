package breaker

import (
	"testing"
	"time"
)

func opts() Options {
	return Options{Window: time.Hour, MaxHeals: 3, MaxRollbacks: 2, OpenFor: time.Hour}
}

func TestClosedAllowsHealing(t *testing.T) {
	b := New(opts())
	if !b.Allowed("dep/a", time.Now()) {
		t.Fatal("a fresh breaker must allow healing")
	}
}

func TestRollbacksTrip(t *testing.T) {
	b := New(opts())
	now := time.Now()
	if b.OnRollback("dep/a", now) {
		t.Fatal("first rollback must not trip (MaxRollbacks=2)")
	}
	if !b.OnRollback("dep/a", now.Add(time.Minute)) {
		t.Fatal("second rollback must trip")
	}
	if b.Allowed("dep/a", now.Add(2*time.Minute)) {
		t.Fatal("a tripped breaker must block healing")
	}
}

func TestTripReportedOnce(t *testing.T) {
	b := New(opts())
	now := time.Now()
	b.OnRollback("dep/a", now)
	if !b.OnRollback("dep/a", now.Add(time.Minute)) {
		t.Fatal("second rollback should report the trip")
	}
	// A further rollback while open must not report a fresh trip.
	if b.OnRollback("dep/a", now.Add(2*time.Minute)) {
		t.Fatal("a rollback while already open must not re-report a trip")
	}
}

func TestHealFlappingTrips(t *testing.T) {
	b := New(opts())
	now := time.Now()
	if b.OnHeal("dep/a", now) || b.OnHeal("dep/a", now.Add(time.Minute)) {
		t.Fatal("heals below MaxHeals must not trip")
	}
	if !b.OnHeal("dep/a", now.Add(2*time.Minute)) {
		t.Fatal("the third heal in window must trip")
	}
}

func TestReopensAfterWindow(t *testing.T) {
	b := New(opts())
	now := time.Now()
	b.OnRollback("dep/a", now)
	b.OnRollback("dep/a", now.Add(time.Minute)) // trips, open for 1h
	if b.Allowed("dep/a", now.Add(30*time.Minute)) {
		t.Fatal("still open at 30m")
	}
	if !b.Allowed("dep/a", now.Add(61*time.Minute)) {
		t.Fatal("breaker must close after OpenFor elapses")
	}
	// History cleared: one rollback should not immediately re-trip.
	if b.OnRollback("dep/a", now.Add(62*time.Minute)) {
		t.Fatal("post-reset, a single rollback must not trip")
	}
}

func TestOldEventsPrunedFromWindow(t *testing.T) {
	b := New(opts())
	now := time.Now()
	b.OnRollback("dep/a", now)
	// Second rollback well outside the 1h window: the first has aged out, so
	// this is effectively the first-in-window and must not trip.
	if b.OnRollback("dep/a", now.Add(2*time.Hour)) {
		t.Fatal("a rollback outside the window must not count toward a trip")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	b := New(opts())
	now := time.Now()
	b.OnRollback("dep/a", now)
	b.OnRollback("dep/a", now.Add(time.Minute)) // a trips
	if !b.Allowed("dep/b", now.Add(time.Minute)) {
		t.Fatal("tripping one workload must not block another")
	}
	if b.OpenCount(now.Add(time.Minute)) != 1 {
		t.Fatalf("want 1 open workload, got %d", b.OpenCount(now.Add(time.Minute)))
	}
}

func TestDisabledSignalNeverTrips(t *testing.T) {
	b := New(Options{Window: time.Hour, MaxHeals: 0, MaxRollbacks: 0, OpenFor: time.Hour})
	now := time.Now()
	for i := 0; i < 10; i++ {
		if b.OnRollback("dep/a", now.Add(time.Duration(i)*time.Minute)) {
			t.Fatal("with thresholds at 0 the breaker must never trip")
		}
	}
}
