package heal

import (
	"context"
	"testing"
	"time"
)

// fakeConfigMap is an in-memory ConfigMapAPI: one namespace/name → data.
type fakeConfigMap struct {
	data    map[string]string
	present bool
	puts    int
}

func (f *fakeConfigMap) GetConfigMap(_ context.Context, _, _ string) (map[string]string, error) {
	if !f.present {
		return nil, nil // not created yet
	}
	return f.data, nil
}

func (f *fakeConfigMap) PutConfigMap(_ context.Context, _, _ string, data map[string]string) error {
	f.data = data
	f.present = true
	f.puts++
	return nil
}

func rec(ns, name string) HealRecord {
	return HealRecord{
		ControllerKind: "Deployment", ControllerName: name, Namespace: ns,
		Container: "web", VerifyAfter: time.Unix(100, 0),
	}
}

func TestConfigMapStoreRoundTrip(t *testing.T) {
	api := &fakeConfigMap{}
	s := NewConfigMapStore(api, "podsmedic", "podsmedic-heal-state")
	ctx := context.Background()

	// Empty store reads as no records, not an error.
	got, err := s.List(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: got %v err %v", got, err)
	}

	if err := s.Save(ctx, rec("api", "web")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.Save(ctx, rec("api", "cache")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ = s.List(ctx)
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d", len(got))
	}

	if err := s.Delete(ctx, "api/Deployment/web"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = s.List(ctx)
	if len(got) != 1 || got[0].ControllerName != "cache" {
		t.Fatalf("delete left wrong set: %+v", got)
	}
}

func TestConfigMapStoreUpsertReplaces(t *testing.T) {
	api := &fakeConfigMap{}
	s := NewConfigMapStore(api, "podsmedic", "state")
	ctx := context.Background()

	r := rec("api", "web")
	r.Summary = "first"
	_ = s.Save(ctx, r)
	r.Summary = "second" // same controller key
	_ = s.Save(ctx, r)

	got, _ := s.List(ctx)
	if len(got) != 1 {
		t.Fatalf("re-heal of one workload must not stack records, got %d", len(got))
	}
	if got[0].Summary != "second" {
		t.Fatalf("upsert should replace, got %q", got[0].Summary)
	}
}
