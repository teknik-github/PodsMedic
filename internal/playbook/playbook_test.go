package playbook

import (
	"testing"
	"time"
)

const (
	ctrl = "api/Deployment/web"
	oom  = "OOMKilled"
)

func TestLookupMissThenRecordHit(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	if _, ok := b.Lookup(ctrl, oom); ok {
		t.Fatal("empty book must miss")
	}
	now := time.Now()
	b.Record(ctrl, oom, `{"kind":"patch_resources"}`, "high", now)
	e, ok := b.Lookup(ctrl, oom)
	if !ok || e.ActionJSON != `{"kind":"patch_resources"}` || e.Confidence != "high" {
		t.Fatalf("recorded remedy not found: %+v ok=%v", e, ok)
	}
	if e.Hits != 0 {
		t.Fatalf("a fresh entry has no hits, got %d", e.Hits)
	}
}

func TestRecordIgnoresEmptyAction(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	b.Record(ctrl, oom, "", "high", time.Now())
	if b.Count() != 0 {
		t.Fatal("an empty action must not be recorded")
	}
}

func TestMarkHitCounts(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	now := time.Now()
	b.Record(ctrl, oom, `{"a":1}`, "high", now)
	b.MarkHit(ctrl, oom, now.Add(time.Minute))
	b.MarkHit(ctrl, oom, now.Add(2*time.Minute))
	e, _ := b.Lookup(ctrl, oom)
	if e.Hits != 2 {
		t.Fatalf("want 2 hits, got %d", e.Hits)
	}
}

func TestKindsAreDistinct(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	now := time.Now()
	b.Record(ctrl, oom, `{"a":1}`, "high", now)
	b.Record(ctrl, "CrashLoopBackOff", `{"b":2}`, "high", now)
	if b.Count() != 2 {
		t.Fatalf("distinct kinds are distinct entries, got %d", b.Count())
	}
}

func TestRecordRefreshesExisting(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	now := time.Now()
	b.Record(ctrl, oom, `{"v":1}`, "medium", now)
	b.Record(ctrl, oom, `{"v":2}`, "high", now.Add(time.Hour))
	if b.Count() != 1 {
		t.Fatalf("re-record must refresh, not duplicate, got %d", b.Count())
	}
	e, _ := b.Lookup(ctrl, oom)
	if e.ActionJSON != `{"v":2}` || e.Confidence != "high" {
		t.Fatalf("refresh should overwrite action/confidence, got %+v", e)
	}
}

func TestEvict(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	b.Record(ctrl, oom, `{"a":1}`, "high", time.Now())
	if !b.Evict(ctrl, oom) {
		t.Fatal("evict of an existing entry returns true")
	}
	if _, ok := b.Lookup(ctrl, oom); ok {
		t.Fatal("evicted entry must be gone")
	}
	if b.Evict(ctrl, oom) {
		t.Fatal("evict of a missing entry returns false")
	}
}

func TestCapEvictsOldestVerified(t *testing.T) {
	b := New(Options{MaxEntries: 2})
	t0 := time.Now()
	b.Record("api/Deployment/a", oom, `{"a":1}`, "high", t0) // oldest
	b.Record("api/Deployment/b", oom, `{"b":1}`, "high", t0.Add(1*time.Minute))
	b.Record("api/Deployment/c", oom, `{"c":1}`, "high", t0.Add(2*time.Minute)) // triggers evict
	if b.Count() != 2 {
		t.Fatalf("cap must hold 2, got %d", b.Count())
	}
	if _, ok := b.Lookup("api/Deployment/a", oom); ok {
		t.Fatal("the oldest-verified entry should have been evicted")
	}
	if _, ok := b.Lookup("api/Deployment/c", oom); !ok {
		t.Fatal("the newest entry must be present")
	}
}

func TestDirtyAndSnapshotRestore(t *testing.T) {
	b := New(Options{MaxEntries: 10})
	if b.Dirty() {
		t.Fatal("fresh book not dirty")
	}
	now := time.Now()
	b.Record(ctrl, oom, `{"a":1}`, "high", now)
	if !b.Dirty() {
		t.Fatal("record must mark dirty")
	}
	snap := b.Snapshot()
	b.ClearDirty()

	b2 := New(Options{MaxEntries: 10})
	b2.Restore(State{Entries: snap})
	if b2.Dirty() {
		t.Fatal("restore must not mark dirty")
	}
	if _, ok := b2.Lookup(ctrl, oom); !ok {
		t.Fatal("restored book must find the remedy")
	}
}
