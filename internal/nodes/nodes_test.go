package nodes

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func cond(t string, active bool, ago time.Duration) Condition {
	return Condition{Type: t, Active: active, Reason: "KubeletHasSomething", Since: now.Add(-ago)}
}

func healthy(name string) State {
	return State{
		Name: name, Pods: 12,
		Conditions: []Condition{
			cond("Ready", true, time.Hour),
			cond("DiskPressure", false, time.Hour),
			cond("MemoryPressure", false, time.Hour),
			cond("PIDPressure", false, time.Hour),
		},
	}
}

// withCond sets a condition, replacing any existing one of that type. The API
// only ever reports one condition per type, so appending a duplicate would test
// a state the cluster cannot produce.
func withCond(s State, c Condition) State {
	for i := range s.Conditions {
		if s.Conditions[i].Type == c.Type {
			s.Conditions[i] = c
			return s
		}
	}
	s.Conditions = append(s.Conditions, c)
	return s
}

func kinds(fs []Finding) []Kind {
	out := make([]Kind, len(fs))
	for i, f := range fs {
		out[i] = f.Kind
	}
	return out
}

func has(fs []Finding, k Kind) bool {
	for _, f := range fs {
		if f.Kind == k {
			return true
		}
	}
	return false
}

func TestHealthyNodeProducesNothing(t *testing.T) {
	if got := Check([]State{healthy("a")}, DefaultOptions(), now); len(got) != 0 {
		t.Fatalf("a healthy node must be silent, got %v", kinds(got))
	}
}

func TestNotReadyBeyondGraceIsCritical(t *testing.T) {
	s := healthy("a")
	s.Conditions[0] = cond("Ready", false, 10*time.Minute)

	got := Check([]State{s}, DefaultOptions(), now)
	if len(got) != 1 || got[0].Kind != KindNotReady {
		t.Fatalf("want one NotReady finding, got %v", kinds(got))
	}
	if got[0].Severity != SeverityCritical {
		t.Fatalf("a NotReady node is critical, got %q", got[0].Severity)
	}
	// The message goes straight to Telegram with no surrounding context, so it
	// has to carry the blast radius itself.
	if !strings.Contains(got[0].Summary, "12 pods") {
		t.Fatalf("the summary must say how much is at risk: %q", got[0].Summary)
	}
}

func TestFlappingConditionsAreGraced(t *testing.T) {
	// A kubelet restart drops Ready for a few seconds. Alerting on that would
	// make the feature noise, and noise is how a real alert gets ignored.
	s := healthy("a")
	s.Conditions[0] = cond("Ready", false, 10*time.Second)
	if got := Check([]State{s}, DefaultOptions(), now); len(got) != 0 {
		t.Fatalf("a condition inside the grace window must not report, got %v", kinds(got))
	}
}

func TestMissingReadyConditionCountsAsNotReady(t *testing.T) {
	// A node that will not say it is healthy is not healthy. Treating silence
	// as health is how a partitioned kubelet goes unnoticed.
	s := State{Name: "a", Pods: 3, Conditions: []Condition{cond("DiskPressure", false, time.Hour)}}
	got := Check([]State{s}, DefaultOptions(), now)
	if len(got) != 1 || got[0].Kind != KindNotReady {
		t.Fatalf("want NotReady for a node with no Ready condition, got %v", kinds(got))
	}
}

func TestPressureConditionsAreReportedWithTheirEffect(t *testing.T) {
	cases := []struct {
		condition string
		want      Kind
		severity  string
	}{
		{"DiskPressure", KindDiskPressure, SeverityCritical},
		{"MemoryPressure", KindMemoryPressure, SeverityCritical},
		{"PIDPressure", KindPIDPressure, SeverityWarning},
		{"NetworkUnavailable", KindNetworkUnavailable, SeverityWarning},
	}
	for _, c := range cases {
		s := withCond(healthy("a"), cond(c.condition, true, time.Hour))
		got := Check([]State{s}, DefaultOptions(), now)
		if len(got) != 1 || got[0].Kind != c.want {
			t.Fatalf("%s: want %s, got %v", c.condition, c.want, kinds(got))
		}
		if got[0].Severity != c.severity {
			t.Errorf("%s: want severity %s, got %s", c.condition, c.severity, got[0].Severity)
		}
	}
}

func TestInactivePressureIsSilent(t *testing.T) {
	// Every healthy node carries DiskPressure=False. Reporting the condition's
	// presence rather than its truth would alert on the entire cluster.
	s := withCond(healthy("a"), cond("DiskPressure", false, time.Hour))
	if got := Check([]State{s}, DefaultOptions(), now); len(got) != 0 {
		t.Fatalf("a False condition is the healthy case, got %v", kinds(got))
	}
}

func TestCordonIsOffByDefault(t *testing.T) {
	// During a planned drain this would alert on the operator's own work.
	s := healthy("a")
	s.Unschedulable = true
	if got := Check([]State{s}, DefaultOptions(), now); len(got) != 0 {
		t.Fatalf("cordon must be opt-in, got %v", kinds(got))
	}

	opts := DefaultOptions()
	opts.ReportCordoned = true
	got := Check([]State{s}, opts, now)
	if len(got) != 1 || got[0].Kind != KindCordoned {
		t.Fatalf("want a cordon finding when asked for, got %v", kinds(got))
	}
}

func TestOneNodeCanHaveSeveralFaults(t *testing.T) {
	s := withCond(healthy("a"), cond("Ready", false, time.Hour))
	s = withCond(s, cond("DiskPressure", true, time.Hour))

	got := Check([]State{s}, DefaultOptions(), now)
	if !has(got, KindNotReady) || !has(got, KindDiskPressure) {
		t.Fatalf("both faults should be reported, got %v", kinds(got))
	}
}

func TestCriticalFindingsSortFirst(t *testing.T) {
	warn := withCond(healthy("a"), cond("PIDPressure", true, time.Hour))
	crit := withCond(healthy("b"), cond("Ready", false, time.Hour))

	got := Check([]State{warn, crit}, DefaultOptions(), now)
	if len(got) != 2 {
		t.Fatalf("want two findings, got %v", kinds(got))
	}
	if got[0].Severity != SeverityCritical {
		t.Fatalf("the critical finding must come first, got %v", kinds(got))
	}
}

func TestKeyIsStableWhileTheFaultPersists(t *testing.T) {
	// Dedupe hangs off this. If the key moved with the age or the pod count, a
	// node NotReady for an hour would re-alert every sweep.
	a := Finding{Node: "n1", Kind: KindNotReady, Since: now, Pods: 3}
	b := Finding{Node: "n1", Kind: KindNotReady, Since: now.Add(time.Hour), Pods: 7}
	if a.Key() != b.Key() {
		t.Fatalf("the same ongoing fault must keep one key: %q vs %q", a.Key(), b.Key())
	}
	other := Finding{Node: "n1", Kind: KindDiskPressure}
	if a.Key() == other.Key() {
		t.Fatal("different faults on one node must be distinct")
	}
}

func TestUnknownTransitionTimeIsBelievedImmediately(t *testing.T) {
	// A missing timestamp is not evidence the fault is new, and grace-gating it
	// forever would hide it forever.
	s := healthy("a")
	s.Conditions[0] = Condition{Type: "Ready", Active: false}
	if got := Check([]State{s}, DefaultOptions(), now); len(got) != 1 {
		t.Fatalf("want the fault reported, got %v", kinds(got))
	}
}
