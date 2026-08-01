package incident

import (
	"testing"
	"time"

	"github.com/peceldev/podsmedic/internal/detect"
)

func prob(pod string, kind detect.Kind) detect.Problem {
	return detect.Problem{Namespace: "api", Pod: pod, Container: "web", Kind: kind}
}

func TestFirstProblemOpensIncident(t *testing.T) {
	s := NewStore(10 * time.Minute)
	inc, action := s.Observe(prob("web-1", detect.KindOOMKilled), time.Now())
	if action != New {
		t.Fatalf("first problem must open a New incident, got %v", action)
	}
	if inc.Trigger.Kind != detect.KindOOMKilled {
		t.Fatalf("trigger kind wrong: %+v", inc.Trigger)
	}
}

func TestSameSweepExtraKindIsFolded(t *testing.T) {
	s := NewStore(10 * time.Minute)
	now := time.Now()
	if _, a := s.Observe(prob("web-1", detect.KindOOMKilled), now); a != New {
		t.Fatalf("want New, got %v", a)
	}
	// Same pod, different kind, same sweep → folded into the New alert.
	inc, a := s.Observe(prob("web-1", detect.KindCrashLoopBackOff), now)
	if a != Suppress {
		t.Fatalf("a same-sweep extra kind must be suppressed (folded), got %v", a)
	}
	if got := inc.OtherKinds(); len(got) != 1 || got[0] != string(detect.KindCrashLoopBackOff) {
		t.Fatalf("OtherKinds should list the folded kind, got %v", got)
	}
}

func TestRepeatIsSuppressed(t *testing.T) {
	s := NewStore(10 * time.Minute)
	now := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), now)
	if _, a := s.Observe(prob("web-1", detect.KindOOMKilled), now.Add(time.Minute)); a != Suppress {
		t.Fatalf("a repeat of the same kind must be suppressed, got %v", a)
	}
}

func TestLaterSweepNewKindUpdates(t *testing.T) {
	s := NewStore(10 * time.Minute)
	t0 := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), t0)
	inc, a := s.Observe(prob("web-1", detect.KindRestartStorm), t0.Add(time.Minute))
	if a != Update {
		t.Fatalf("a new kind in a later sweep must Update, got %v", a)
	}
	if len(inc.Kinds) != 2 {
		t.Fatalf("incident should track both kinds, got %v", inc.Kinds)
	}
}

func TestReapResolvesQuietIncidents(t *testing.T) {
	s := NewStore(5 * time.Minute)
	t0 := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), t0)

	// Not yet past the window.
	if r := s.Reap(t0.Add(4 * time.Minute)); len(r) != 0 {
		t.Fatalf("nothing should resolve before the window, got %d", len(r))
	}
	// Past the window → resolved and removed.
	r := s.Reap(t0.Add(6 * time.Minute))
	if len(r) != 1 || r[0].Pod != "web-1" {
		t.Fatalf("the quiet incident should resolve, got %v", r)
	}
	if s.OpenCount() != 0 {
		t.Fatalf("resolved incident must be removed, open=%d", s.OpenCount())
	}
}

func TestSeenIncidentIsNotReaped(t *testing.T) {
	s := NewStore(5 * time.Minute)
	t0 := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), t0)
	// Seen again just before the window would expire.
	s.Observe(prob("web-1", detect.KindOOMKilled), t0.Add(4*time.Minute))
	if r := s.Reap(t0.Add(6 * time.Minute)); len(r) != 0 {
		t.Fatalf("a recently-seen incident must not resolve, got %d", len(r))
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	s := NewStore(10 * time.Minute)
	now := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), now)
	s.Observe(prob("web-1", detect.KindCrashLoopBackOff), now.Add(time.Minute)) // Update, second kind
	s.SetHealProposal("api/web-1/web", `{"kind":"patch_resources"}`, "high")

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 incident in snapshot, got %d", len(snap))
	}

	// Fresh store, restored from the snapshot.
	s2 := NewStore(10 * time.Minute)
	s2.Restore(snap)
	if s2.OpenCount() != 1 {
		t.Fatalf("restore should reopen the incident, open=%d", s2.OpenCount())
	}
	blob, conf, healed, ok := s2.HealProposal("api/web-1/web")
	if !ok || blob != `{"kind":"patch_resources"}` || conf != "high" || healed {
		t.Fatalf("heal proposal not restored: %q %q %v %v", blob, conf, healed, ok)
	}
	// The rebuilt kind set must recognise a known kind as a repeat (Suppress),
	// proving kindSet was reconstructed from the persisted Kinds slice.
	if _, a := s2.Observe(prob("web-1", detect.KindOOMKilled), now.Add(2*time.Minute)); a != Suppress {
		t.Fatalf("a known kind after restore must Suppress, got %v", a)
	}
}

func TestDirtyTracking(t *testing.T) {
	s := NewStore(10 * time.Minute)
	if s.Dirty() {
		t.Fatal("a fresh store is not dirty")
	}
	s.Observe(prob("web-1", detect.KindOOMKilled), time.Now())
	if !s.Dirty() {
		t.Fatal("an observation must mark the store dirty")
	}
	s.ClearDirty()
	if s.Dirty() {
		t.Fatal("ClearDirty must clear the flag")
	}
	// Restore must NOT set dirty: the restored state already matches storage.
	snap := s.Snapshot()
	s2 := NewStore(10 * time.Minute)
	s2.Restore(snap)
	if s2.Dirty() {
		t.Fatal("Restore must not mark the store dirty")
	}
}

func TestMarkHealedStopsAndPersists(t *testing.T) {
	s := NewStore(10 * time.Minute)
	s.Observe(prob("web-1", detect.KindOOMKilled), time.Now())
	s.ClearDirty()
	s.MarkHealed("api/web-1/web")
	if !s.Dirty() {
		t.Fatal("MarkHealed must mark the store dirty")
	}
	if _, _, healed, _ := s.HealProposal("api/web-1/web"); !healed {
		t.Fatal("MarkHealed must set healed")
	}
}

func TestReapMarksDirty(t *testing.T) {
	s := NewStore(5 * time.Minute)
	t0 := time.Now()
	s.Observe(prob("web-1", detect.KindOOMKilled), t0)
	s.ClearDirty()
	s.Reap(t0.Add(6 * time.Minute))
	if !s.Dirty() {
		t.Fatal("reaping an incident must mark the store dirty")
	}
}

func TestDifferentPodsAreDistinctIncidents(t *testing.T) {
	s := NewStore(10 * time.Minute)
	now := time.Now()
	if _, a := s.Observe(prob("web-1", detect.KindOOMKilled), now); a != New {
		t.Fatalf("want New for web-1, got %v", a)
	}
	if _, a := s.Observe(prob("web-2", detect.KindOOMKilled), now); a != New {
		t.Fatalf("a different pod is a separate incident, want New, got %v", a)
	}
	if s.OpenCount() != 2 {
		t.Fatalf("want 2 open incidents, got %d", s.OpenCount())
	}
}
