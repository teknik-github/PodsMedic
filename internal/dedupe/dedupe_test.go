package dedupe

import (
	"testing"
	"time"
)

func TestSuppressesWithinCooldown(t *testing.T) {
	c := New(time.Hour)

	if !c.ShouldAlert("api/web/app/OOMKilled") {
		t.Fatal("first sighting must alert")
	}
	if c.ShouldAlert("api/web/app/OOMKilled") {
		t.Fatal("second sighting within cooldown must be suppressed")
	}
	if !c.ShouldAlert("api/web/app/CrashLoopBackOff") {
		t.Fatal("a different fingerprint must alert independently")
	}
}

func TestAlertsAgainAfterCooldown(t *testing.T) {
	c := New(10 * time.Millisecond)

	if !c.ShouldAlert("fp") {
		t.Fatal("first sighting must alert")
	}
	time.Sleep(20 * time.Millisecond)
	if !c.ShouldAlert("fp") {
		t.Fatal("expected a re-alert once the cooldown elapsed")
	}
}

func TestSweepDropsStaleEntries(t *testing.T) {
	c := New(5 * time.Millisecond)
	c.ShouldAlert("fp")

	time.Sleep(20 * time.Millisecond) // older than 2x cooldown
	c.Sweep()

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) != 0 {
		t.Fatalf("expected stale entries to be swept, %d remain", len(c.seen))
	}
}
