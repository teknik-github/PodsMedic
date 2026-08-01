package digest

import (
	"strings"
	"testing"
	"time"
)

var jakarta = time.FixedZone("WIB", 7*3600)

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 8, day, hour, minute, 0, 0, jakarta)
}

func nine() Schedule { return Schedule{Hour: 9, Loc: jakarta} }

func TestParseSchedule(t *testing.T) {
	s, on, err := ParseSchedule("09:30", jakarta)
	if err != nil || !on {
		t.Fatalf("09:30 should parse, got on=%v err=%v", on, err)
	}
	if s.Hour != 9 || s.Minute != 30 {
		t.Fatalf("parsed wrong: %+v", s)
	}

	// Empty means off, and off is a configuration rather than a mistake.
	if _, on, err := ParseSchedule("", jakarta); on || err != nil {
		t.Fatalf("empty should be off with no error, got on=%v err=%v", on, err)
	}
	for _, bad := range []string{"25:00", "09:70", "morning", "9"} {
		if _, _, err := ParseSchedule(bad, jakarta); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestNotDueBeforeTheSlot(t *testing.T) {
	sent := at(1, 9, 0)
	if nine().Due(at(1, 8, 59), sent) {
		t.Fatal("must not fire before the scheduled time")
	}
}

func TestDueOncePerDay(t *testing.T) {
	s := nine()
	sent := at(1, 9, 0)
	if s.Due(at(1, 23, 0), sent) {
		t.Fatal("must not fire twice on the same day")
	}
	if !s.Due(at(2, 9, 1), sent) {
		t.Fatal("must fire on the next day's slot")
	}
}

func TestAMissedWindowIsCaughtLate(t *testing.T) {
	// The process was down at 09:00 and came back at 11:00. The digest should
	// arrive late, not vanish for a day: a summary that silently skips is worse
	// than one that is two hours old.
	s := nine()
	sent := at(1, 9, 0)
	if !s.Due(at(2, 11, 0), sent) {
		t.Fatal("a missed slot must still be owed")
	}
}

func TestAfterMidnightBelongsToYesterdaysSlot(t *testing.T) {
	// A late-evening schedule read just after midnight must not conclude that
	// today's slot is still in the future and skip last night's send.
	s := Schedule{Hour: 23, Loc: jakarta}
	sent := at(1, 12, 0)
	if !s.Due(at(2, 0, 30), sent) {
		t.Fatal("the 23:00 slot from the previous day was missed")
	}
	if got := s.LastSlot(at(2, 0, 30)); got.Day() != 1 || got.Hour() != 23 {
		t.Fatalf("last slot should be yesterday 23:00, got %s", got)
	}
}

func TestScheduleIsReadInItsOwnZone(t *testing.T) {
	// 09:00 WIB is 02:00 UTC. Reading the clock in the wrong zone would send
	// the daily summary in the middle of the night.
	s := nine()
	utcMorning := time.Date(2026, 8, 2, 2, 1, 0, 0, time.UTC) // 09:01 WIB
	if !s.Due(utcMorning, at(1, 9, 0)) {
		t.Fatal("the slot should have passed in the configured zone")
	}
	utcEarlier := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC) // 08:00 WIB
	if s.Due(utcEarlier, at(1, 9, 0)) {
		t.Fatal("the slot has not passed yet in the configured zone")
	}
}

func TestTallyTakeResetsAtomically(t *testing.T) {
	tally := NewTally(at(1, 9, 0))
	tally.Sweep()
	tally.Sweep()
	tally.Heal("applied")
	tally.Heal("skipped")
	tally.Verification("rolledback")
	tally.LLM(0.25)

	c, span := tally.Take(at(1, 10, 0))
	if c.Sweeps != 2 || c.HealsApplied != 1 || c.HealsDeclined != 1 || c.RolledBack != 1 {
		t.Fatalf("counters lost: %+v", c)
	}
	if c.LLMCalls != 1 || c.LLMCostUSD != 0.25 {
		t.Fatalf("cost accounting lost: %+v", c)
	}
	if span != time.Hour {
		t.Fatalf("want a one-hour span, got %s", span)
	}

	// Taking again must start clean, or a day's numbers appear in the next
	// digest as well.
	again, _ := tally.Take(at(1, 11, 0))
	if again != (Counters{}) {
		t.Fatalf("counters were not reset: %+v", again)
	}
}

func TestPeekDoesNotDisturbTheDailyAccounting(t *testing.T) {
	tally := NewTally(at(1, 9, 0))
	tally.Sweep()
	if c, _ := tally.Peek(at(1, 10, 0)); c.Sweeps != 1 {
		t.Fatalf("peek should see the count, got %+v", c)
	}
	if c, _ := tally.Take(at(1, 10, 0)); c.Sweeps != 1 {
		t.Fatal("peek must not have consumed the counters")
	}
}

func TestQuietPeriodStillProducesADigest(t *testing.T) {
	// The whole reason the feature exists: if it only sent when something broke,
	// silence would still be ambiguous.
	out := Build(Input{
		Counters: Counters{Sweeps: 1440}, GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour,
		Pods: 32, Nodes: 1, AutoHeal: true, Applying: true,
	})
	if !strings.Contains(out, "quiet period") {
		t.Fatalf("a quiet day should say so plainly: %q", out)
	}
	if !strings.Contains(out, "1440 sweep") {
		t.Fatalf("a quiet digest must still prove the agent was working: %q", out)
	}
}

func TestAQuietPeriodWithStandingProblemsDoesNotClaimNothingFailed(t *testing.T) {
	// Caught by reading a real digest: "nothing failed" printed directly under
	// "5 problem(s)" reads as a contradiction, and a summary a reader stops
	// trusting is worse than none. Nothing *new* failing is a different claim
	// from nothing being wrong.
	out := Build(Input{
		Counters: Counters{Sweeps: 4}, GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour,
		Pods: 34, Problems: 5, IncidentsOpen: 4,
	})
	if strings.Contains(out, "nothing failed and nothing needed changing") {
		t.Fatalf("standing problems must not be described as nothing failing: %q", out)
	}
	if !strings.Contains(out, "still open") {
		t.Fatalf("the digest must say the open problems are not being fixed: %q", out)
	}
}

func TestDryRunIsNeverPresentedAsRealChange(t *testing.T) {
	out := Build(Input{
		Counters:    Counters{Sweeps: 10, HealsDryRun: 3},
		GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour, AutoHeal: true, Applying: false,
	})
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("a dry run must be labelled as one: %q", out)
	}
	if strings.Contains(out, "3 applied") {
		t.Fatalf("dry runs must not read as applied changes: %q", out)
	}
}

func TestRollbacksAreCalledOut(t *testing.T) {
	out := Build(Input{
		Counters:    Counters{Sweeps: 10, HealsApplied: 2, Verified: 1, RolledBack: 1},
		GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour, AutoHeal: true, Applying: true,
	})
	if !strings.Contains(out, "rolled back") || !strings.Contains(out, "worth a look") {
		t.Fatalf("a rollback needs a human's attention drawn to it: %q", out)
	}
}

func TestDeclinesAloneAreExplainedNotAlarming(t *testing.T) {
	// A validator declining is the normal case. Presented bare, "12 declined"
	// reads as a fault.
	out := Build(Input{
		Counters:    Counters{Sweeps: 10, HealsDeclined: 12},
		GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour, AutoHeal: true, Applying: true,
	})
	if !strings.Contains(out, "expected outcome") {
		t.Fatalf("declines should be explained: %q", out)
	}
}

func TestStandingBrakesAreSurfaced(t *testing.T) {
	// A workload whose breaker tripped last week is invisible everywhere else.
	out := Build(Input{
		Counters:    Counters{Sweeps: 100},
		GeneratedAt: at(2, 9, 0), Span: 24 * time.Hour,
		BreakersOpen: 2, Quarantined: 1, AutoHeal: true, Applying: true,
	})
	if !strings.Contains(out, "STANDING BRAKES") {
		t.Fatalf("suspended healing must be reported: %q", out)
	}
	if !strings.Contains(out, "circuit breaker") || !strings.Contains(out, "quarantined") {
		t.Fatalf("both brakes should be named: %q", out)
	}
}

func TestNoBrakesSectionWhenThereAreNone(t *testing.T) {
	out := Build(Input{Counters: Counters{Sweeps: 1}, GeneratedAt: at(2, 9, 0), Span: time.Hour})
	if strings.Contains(out, "STANDING BRAKES") {
		t.Fatalf("an empty section is noise: %q", out)
	}
}

func TestShortSpanIsStatedNotAssumed(t *testing.T) {
	// After a restart the digest covers less than a day. Half a day of numbers
	// reading as a full one would understate every rate.
	out := Build(Input{Counters: Counters{Sweeps: 30}, GeneratedAt: at(2, 9, 0), Span: 3 * time.Hour})
	if !strings.Contains(out, "3 hours") {
		t.Fatalf("the covered span must be stated: %q", out)
	}
}

func TestAutoHealOffIsStated(t *testing.T) {
	out := Build(Input{Counters: Counters{Sweeps: 5}, GeneratedAt: at(2, 9, 0), Span: time.Hour})
	if !strings.Contains(out, "Auto-heal is off") {
		t.Fatalf("a reader must know nothing is being changed: %q", out)
	}
}
