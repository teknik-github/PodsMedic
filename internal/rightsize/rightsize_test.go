package rightsize

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

const (
	mi = 1 << 20
	gi = 1 << 30
)

// observe feeds n samples spread over the given window, so a test can state
// "watched for two days" rather than build a loop each time.
func observe(t *Tracker, s Sample, n int, window time.Duration) {
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * window / time.Duration(n-1))
		t.Observe([]Sample{s}, at)
	}
}

func sample() Sample {
	return Sample{
		Namespace: "api", Workload: "Deployment/web", Container: "app",
		CPUMilli: 20, MemBytes: 100 * mi,
		RequestCPUMilli: 500, RequestMemBytes: 1 * gi,
	}
}

func only(t *testing.T, fs []Finding, r Resource) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Resource == r {
			return f
		}
	}
	t.Fatalf("no %s finding in %d results", r, len(fs))
	return Finding{}
}

func TestNothingIsSaidWithoutEnoughEvidence(t *testing.T) {
	// Every workload has a quiet ten minutes. Judging one on that basis is how
	// a report tells someone to shrink a nightly batch job.
	tr := New(0)
	observe(tr, sample(), 200, 30*time.Minute) // plenty of samples, no window
	if got := tr.Findings(DefaultOptions(), t0); len(got) != 0 {
		t.Fatalf("a short window must produce nothing, got %d findings", len(got))
	}

	tr2 := New(0)
	observe(tr2, sample(), 5, 72*time.Hour) // long window, barely any samples
	if got := tr2.Findings(DefaultOptions(), t0); len(got) != 0 {
		t.Fatalf("too few samples must produce nothing, got %d findings", len(got))
	}
}

func TestOversizedIsReportedWithARecommendation(t *testing.T) {
	tr := New(0)
	observe(tr, sample(), 100, 48*time.Hour)

	fs := tr.Findings(DefaultOptions(), t0)
	cpu := only(t, fs, CPU)
	if cpu.Kind != KindOversized {
		t.Fatalf("want Oversized, got %s", cpu.Kind)
	}
	// Peak 20m, headroom 1.5 → 30m.
	if cpu.Recommended != 30 {
		t.Fatalf("want a 30m recommendation from a 20m peak, got %dm", cpu.Recommended)
	}
	if cpu.Delta != 30-500 {
		t.Fatalf("delta should be the reduction, got %d", cpu.Delta)
	}

	mem := only(t, fs, Memory)
	// Peak 100Mi × 1.5 = 150Mi, rounded up to the 16Mi step = 160Mi.
	if mem.Recommended != 160*mi {
		t.Fatalf("want 160Mi, got %s", FormatMem(mem.Recommended))
	}
}

func TestRecommendationAlwaysClearsTheObservedPeak(t *testing.T) {
	// The one thing a rightsizing report must never do is recommend a value the
	// container has already been seen to exceed.
	tr := New(0)
	s := sample()
	s.CPUMilli, s.MemBytes = 137, 331*mi
	observe(tr, s, 100, 48*time.Hour)

	for _, f := range tr.Findings(DefaultOptions(), t0) {
		if f.Recommended <= f.Peak {
			t.Fatalf("%s recommendation %d does not clear the measured peak %d", f.Resource, f.Recommended, f.Peak)
		}
	}
}

func TestBurstyWorkloadIsJudgedOnItsPeakNotItsMean(t *testing.T) {
	// A container that idles at 10m and spikes to 900m is correctly sized at
	// 1000m. Recommending from the mean would throttle it.
	tr := New(0)
	s := sample()
	s.RequestCPUMilli = 1000
	for i := 0; i < 100; i++ {
		s.CPUMilli = 10
		if i == 50 {
			s.CPUMilli = 900
		}
		tr.Observe([]Sample{s}, t0.Add(time.Duration(i)*time.Hour))
	}
	for _, f := range tr.Findings(DefaultOptions(), t0) {
		if f.Resource == CPU && f.Kind == KindOversized {
			t.Fatalf("a workload that peaked at 900m against a 1000m request is not oversized: %+v", f)
		}
	}
}

func TestUndersizedIsReportedAsAnIncrease(t *testing.T) {
	tr := New(0)
	s := sample()
	s.CPUMilli, s.RequestCPUMilli = 800, 500
	s.MemBytes, s.RequestMemBytes = 100*mi, 1*gi
	observe(tr, s, 100, 48*time.Hour)

	cpu := only(t, tr.Findings(DefaultOptions(), t0), CPU)
	if cpu.Kind != KindUndersized {
		t.Fatalf("peak above request is Undersized, got %s", cpu.Kind)
	}
	if cpu.Delta <= 0 {
		t.Fatalf("an undersized finding raises the request, got delta %d", cpu.Delta)
	}
	if !strings.Contains(cpu.Summary, "did not reserve") {
		t.Fatalf("the summary should explain the risk, got %q", cpu.Summary)
	}
}

func TestMissingRequestsAreReportedEvenWhenUsageIsTiny(t *testing.T) {
	// The harm is not the size. It is that the scheduler, the eviction ranking,
	// and podsmedic's own capacity gate are all working from a zero that is
	// not true.
	tr := New(0)
	s := sample()
	s.CPUMilli, s.MemBytes = 1, 2*mi
	s.RequestCPUMilli, s.RequestMemBytes = 0, 0
	observe(tr, s, 100, 48*time.Hour)

	fs := tr.Findings(DefaultOptions(), t0)
	if len(fs) != 2 {
		t.Fatalf("want a finding for each resource, got %d", len(fs))
	}
	for _, f := range fs {
		if f.Kind != KindNoRequests {
			t.Errorf("%s: want NoRequests, got %s", f.Resource, f.Kind)
		}
		if !strings.Contains(f.Summary, "best-effort") {
			t.Errorf("%s: the summary should name the consequence, got %q", f.Resource, f.Summary)
		}
	}
}

func TestTinySavingsAreSuppressed(t *testing.T) {
	// A report full of 5m savings is a report nobody reads.
	tr := New(0)
	s := sample()
	s.CPUMilli, s.RequestCPUMilli = 10, 60
	s.MemBytes, s.RequestMemBytes = 30*mi, 80*mi // peak×1.5 → 48Mi, a 32Mi saving
	observe(tr, s, 100, 48*time.Hour)

	if got := tr.Findings(DefaultOptions(), t0); len(got) != 0 {
		t.Fatalf("savings below the threshold must be dropped, got %+v", got)
	}
}

func TestFindingsAreSortedBiggestReductionFirst(t *testing.T) {
	tr := New(0)
	small := sample()
	small.Workload, small.RequestCPUMilli, small.CPUMilli = "Deployment/small", 800, 20
	big := sample()
	big.Workload, big.RequestCPUMilli, big.CPUMilli = "Deployment/big", 4000, 20
	observe(tr, small, 100, 48*time.Hour)
	observe(tr, big, 100, 48*time.Hour)

	fs := tr.Findings(DefaultOptions(), t0)
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	if fs[0].Workload != "Deployment/big" {
		t.Fatalf("the largest reduction must lead the report, got %s", fs[0].Workload)
	}
}

func TestTotalsCountReductionsOnly(t *testing.T) {
	// Netting an increase against a saving would let one hide the other, and
	// "we can free 2 cores" would quietly mean "after also adding 2".
	fs := []Finding{
		{Resource: CPU, Delta: -500},
		{Resource: CPU, Delta: 300},
		{Resource: Memory, Delta: -1 * gi},
	}
	cpu, mem := Totals(fs)
	if cpu != 500 {
		t.Fatalf("want 500m freed, got %d", cpu)
	}
	if mem != gi {
		t.Fatalf("want 1Gi freed, got %d", mem)
	}
}

func TestHistorySurvivesASpecChange(t *testing.T) {
	// Someone editing a manifest must not erase a week of measurement, or the
	// report is permanently unavailable on a maintained cluster.
	tr := New(0)
	s := sample()
	observe(tr, s, 50, 24*time.Hour)
	s.RequestCPUMilli = 2000
	observe(tr, s, 50, 24*time.Hour)

	obs := tr.Snapshot()
	if len(obs) != 1 || obs[0].Samples != 100 {
		t.Fatalf("want one observation with 100 samples, got %+v", obs)
	}
	// The judgement must use the current spec, not the old one.
	if obs[0].RequestCPUMilli != 2000 {
		t.Fatalf("want the latest request, got %d", obs[0].RequestCPUMilli)
	}
}

func TestUnownedPodsAreNotTracked(t *testing.T) {
	// A bare pod has no stable identity to accumulate against: its name changes
	// and the history would be one sample deep forever.
	tr := New(0)
	s := sample()
	s.Workload = ""
	tr.Observe([]Sample{s}, t0)
	if tr.Tracking() != 0 {
		t.Fatal("a container with no workload must not be tracked")
	}
}

func TestTrackingIsBounded(t *testing.T) {
	tr := New(5)
	for i := 0; i < 50; i++ {
		s := sample()
		s.Workload = "Deployment/w" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		tr.Observe([]Sample{s}, t0.Add(time.Duration(i)*time.Minute))
	}
	if n := tr.Tracking(); n > 5 {
		t.Fatalf("history grew to %d, past the cap of 5", n)
	}
}

func TestForgetDropsVanishedWorkloads(t *testing.T) {
	tr := New(0)
	tr.Observe([]Sample{sample()}, t0)
	if n := tr.Forget(t0.Add(time.Hour)); n != 1 {
		t.Fatalf("want 1 forgotten, got %d", n)
	}
	if tr.Tracking() != 0 {
		t.Fatal("the vanished workload should be gone")
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	tr := New(0)
	observe(tr, sample(), 100, 48*time.Hour)
	before := tr.Findings(DefaultOptions(), t0)

	restored := New(0)
	restored.Restore(tr.Snapshot())
	if restored.Dirty() {
		t.Fatal("restore must not mark the tracker dirty")
	}
	if got := restored.Findings(DefaultOptions(), t0); len(got) != len(before) {
		t.Fatalf("restored history gave %d findings, want %d", len(got), len(before))
	}
}

func TestFormatting(t *testing.T) {
	// The numbers get pasted into manifests, so they have to look like manifest
	// values rather than raw bytes.
	cases := []struct{ got, want string }{
		{FormatCPU(1500), "1500m"},
		{FormatCPU(2000), "2"},
		{FormatMem(160 * mi), "160Mi"},
		{FormatMem(2 * gi), "2Gi"},
		{FormatMem(1536 * mi), "1536Mi"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
