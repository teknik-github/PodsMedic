package predict

import (
	"testing"
	"time"

	"github.com/peceldev/podsmedic/internal/detect"
)

const mib = 1 << 20

func sample(pod string, usageMi, limitMi int64) Sample {
	return Sample{Namespace: "api", Pod: pod, Container: "web", UsageBytes: usageMi * mib, LimitBytes: limitMi * mib}
}

func opts() Options { return Options{HighRatio: 0.90, MinConsecutive: 3} }

func TestFlagsAfterSustainedHighUsage(t *testing.T) {
	p := New(opts())
	now := time.Now()
	// 95% of limit, three consecutive sweeps.
	if got := p.Observe([]Sample{sample("web-1", 486, 512)}, now); len(got) != 0 {
		t.Fatalf("no flag on the first high sweep, got %v", got)
	}
	if got := p.Observe([]Sample{sample("web-1", 486, 512)}, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("no flag on the second, got %v", got)
	}
	got := p.Observe([]Sample{sample("web-1", 486, 512)}, now.Add(2*time.Minute))
	if len(got) != 1 || got[0].Kind != detect.KindMemoryPressure {
		t.Fatalf("third consecutive high sweep must flag MemoryPressure, got %v", got)
	}
	if got[0].Pod != "web-1" || got[0].Container != "web" {
		t.Fatalf("problem identity wrong: %+v", got[0])
	}
}

func TestBelowRatioNeverFlags(t *testing.T) {
	p := New(opts())
	now := time.Now()
	for i := 0; i < 5; i++ {
		if got := p.Observe([]Sample{sample("web-1", 400, 512)}, now.Add(time.Duration(i)*time.Minute)); len(got) != 0 {
			t.Fatal("usage under HighRatio must never flag")
		}
	}
}

func TestStreakResetsOnDrop(t *testing.T) {
	p := New(opts())
	now := time.Now()
	p.Observe([]Sample{sample("web-1", 500, 512)}, now)                    // high 1
	p.Observe([]Sample{sample("web-1", 500, 512)}, now.Add(1*time.Minute)) // high 2
	p.Observe([]Sample{sample("web-1", 300, 512)}, now.Add(2*time.Minute)) // drop → reset
	got := p.Observe([]Sample{sample("web-1", 500, 512)}, now.Add(3*time.Minute))
	if len(got) != 0 {
		t.Fatalf("a drop must reset the streak, got %v", got)
	}
}

func TestNoLimitIgnored(t *testing.T) {
	p := New(opts())
	now := time.Now()
	s := Sample{Namespace: "api", Pod: "web-1", Container: "web", UsageBytes: 900 * mib, LimitBytes: 0}
	for i := 0; i < 5; i++ {
		if got := p.Observe([]Sample{s}, now.Add(time.Duration(i)*time.Minute)); len(got) != 0 {
			t.Fatal("a container with no memory limit must never flag")
		}
	}
}

func TestVanishedContainerPruned(t *testing.T) {
	p := New(opts())
	now := time.Now()
	p.Observe([]Sample{sample("web-1", 500, 512)}, now)
	p.Observe([]Sample{sample("web-1", 500, 512)}, now.Add(time.Minute))
	if p.Tracking() != 1 {
		t.Fatalf("want 1 tracked, got %d", p.Tracking())
	}
	// Next sweep the container is gone (no samples).
	p.Observe(nil, now.Add(2*time.Minute))
	if p.Tracking() != 0 {
		t.Fatalf("a vanished container must be pruned, tracking=%d", p.Tracking())
	}
}

func TestKeepsFlaggingWhilePressureContinues(t *testing.T) {
	p := New(opts())
	now := time.Now()
	for i := 0; i < 3; i++ {
		p.Observe([]Sample{sample("web-1", 500, 512)}, now.Add(time.Duration(i)*time.Minute))
	}
	// Still high on the 4th and 5th — must keep flagging (incident correlation
	// dedupes; the predictor should not go silent).
	if got := p.Observe([]Sample{sample("web-1", 500, 512)}, now.Add(3*time.Minute)); len(got) != 1 {
		t.Fatalf("sustained pressure must keep flagging, got %v", got)
	}
}

func cpuSample(pod string, usageM, limitM int64) Sample {
	return Sample{Namespace: "api", Pod: pod, Container: "web", CPUMilli: usageM, CPULimit: limitM}
}

func TestFlagsCPUPressure(t *testing.T) {
	p := New(opts())
	now := time.Now()
	// 95% of a 500m limit, three consecutive sweeps.
	p.Observe([]Sample{cpuSample("web-1", 475, 500)}, now)
	p.Observe([]Sample{cpuSample("web-1", 475, 500)}, now.Add(time.Minute))
	got := p.Observe([]Sample{cpuSample("web-1", 475, 500)}, now.Add(2*time.Minute))
	if len(got) != 1 || got[0].Kind != detect.KindCPUPressure {
		t.Fatalf("sustained high CPU must flag CPUPressure, got %v", got)
	}
}

func TestMemAndCPUIndependent(t *testing.T) {
	p := New(opts())
	now := time.Now()
	// Both memory and CPU high for the same container, three sweeps → two problems.
	s := Sample{Namespace: "api", Pod: "web-1", Container: "web",
		UsageBytes: 486 * mib, LimitBytes: 512 * mib, CPUMilli: 475, CPULimit: 500}
	p.Observe([]Sample{s}, now)
	p.Observe([]Sample{s}, now.Add(time.Minute))
	got := p.Observe([]Sample{s}, now.Add(2*time.Minute))
	if len(got) != 2 {
		t.Fatalf("want both MemoryPressure and CPUPressure, got %d: %v", len(got), got)
	}
}
