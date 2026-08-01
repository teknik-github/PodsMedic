package audit

import (
	"context"
	"testing"
	"time"
)

// fakeCM is a minimal in-memory ConfigMapAPI.
type fakeCM struct {
	data map[string]string
}

func (f *fakeCM) GetConfigMap(_ context.Context, _, _ string) (map[string]string, error) {
	return f.data, nil
}
func (f *fakeCM) PutConfigMap(_ context.Context, _, _ string, data map[string]string) error {
	f.data = data
	return nil
}

func ev(n string) Event {
	return Event{Time: time.Now(), Controller: "Deployment/" + n, Action: "patch_resources", Outcome: "applied"}
}

func TestConfigMapLogRoundTrip(t *testing.T) {
	l := NewConfigMapLog(&fakeCM{}, "podsmedic", "podsmedic-audit", 10)
	ctx := context.Background()
	if err := l.Append(ctx, ev("a")); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, ev("b")); err != nil {
		t.Fatal(err)
	}
	got, err := l.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Controller != "Deployment/a" || got[1].Controller != "Deployment/b" {
		t.Fatalf("want [a b] in order, got %+v", got)
	}
}

func TestConfigMapLogEmpty(t *testing.T) {
	l := NewConfigMapLog(&fakeCM{}, "podsmedic", "podsmedic-audit", 10)
	got, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty log should list nothing, got %d", len(got))
	}
}

func TestCapDropsOldest(t *testing.T) {
	l := NewConfigMapLog(&fakeCM{}, "podsmedic", "podsmedic-audit", 3)
	ctx := context.Background()
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		if err := l.Append(ctx, ev(n)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.List(ctx)
	if len(got) != 3 {
		t.Fatalf("cap should hold 3, got %d", len(got))
	}
	// Oldest two ("1","2") dropped; newest kept in order.
	if got[0].Controller != "Deployment/3" || got[2].Controller != "Deployment/5" {
		t.Fatalf("want 3..5 retained in order, got %+v", got)
	}
}

func TestZeroMaxFallsBackToDefault(t *testing.T) {
	l := NewConfigMapLog(&fakeCM{}, "podsmedic", "podsmedic-audit", 0)
	if l.maxEvents != DefaultMaxEvents {
		t.Fatalf("zero max should fall back to %d, got %d", DefaultMaxEvents, l.maxEvents)
	}
}

func TestMemLogCap(t *testing.T) {
	m := NewMemLog(2)
	ctx := context.Background()
	m.Append(ctx, ev("1"))
	m.Append(ctx, ev("2"))
	m.Append(ctx, ev("3"))
	got, _ := m.List(ctx)
	if len(got) != 2 || got[0].Controller != "Deployment/2" {
		t.Fatalf("MemLog cap wrong, got %+v", got)
	}
}
