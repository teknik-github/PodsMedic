package heal

import (
	"testing"
	"time"

	"github.com/peceldev/podsmedic/internal/k8s"
)

func TestVerifyVerdict(t *testing.T) {
	now := time.Now()
	future := HealRecord{VerifyAfter: now.Add(time.Minute)}
	if got := VerifyVerdict(future, now, true); got != VerdictPending {
		t.Fatalf("before the window a heal is pending, got %v", got)
	}

	due := HealRecord{VerifyAfter: now.Add(-time.Minute)}
	if got := VerifyVerdict(due, now, false); got != VerdictHealthy {
		t.Fatalf("a recovered workload is healthy, got %v", got)
	}
	if got := VerifyVerdict(due, now, true); got != VerdictRollback {
		t.Fatalf("a still-failing workload must roll back, got %v", got)
	}
}

func TestRecordFromPlanCopiesOnlyChangedKeys(t *testing.T) {
	before := &k8s.ContainerSummary{
		Name:   "web",
		Limits: map[string]string{"memory": "128Mi", "cpu": "250m"},
	}
	plan := &Plan{
		Kind:      ActionPatchResources,
		Namespace: "api",
		Container: "web",
		Limits:    map[string]string{"memory": "256Mi"}, // only memory changed
		Summary:   "patch container \"web\": memory limit 128Mi→256Mi",
	}
	ctrl := k8s.ControllerRef{Kind: "Deployment", Name: "web", Namespace: "api"}

	rec := RecordFromPlan(plan, ctrl, before, time.Unix(0, 0), 10*time.Minute)

	if rec.OldLimits["memory"] != "128Mi" {
		t.Fatalf("old memory limit not captured: %+v", rec.OldLimits)
	}
	if _, ok := rec.OldLimits["cpu"]; ok {
		t.Fatalf("cpu was not changed and must not be recorded: %+v", rec.OldLimits)
	}
	if rec.NewLimits["memory"] != "256Mi" {
		t.Fatalf("new memory limit not captured: %+v", rec.NewLimits)
	}
	if rec.ControllerKey() != "api/Deployment/web" {
		t.Fatalf("unexpected controller key %q", rec.ControllerKey())
	}
	if !rec.VerifyAfter.Equal(time.Unix(0, 0).Add(10 * time.Minute)) {
		t.Fatalf("verify window not set: %v", rec.VerifyAfter)
	}
}

func TestRecordFromPlanUnsetLimitLeavesNilOld(t *testing.T) {
	before := &k8s.ContainerSummary{Name: "web"} // no prior limits
	plan := &Plan{Container: "web", Limits: map[string]string{"memory": "256Mi"}}

	rec := RecordFromPlan(plan, k8s.ControllerRef{Kind: "Deployment", Name: "web", Namespace: "api"}, before, time.Now(), time.Minute)
	if rec.OldLimits != nil {
		t.Fatalf("no prior value should record nil old limits, got %+v", rec.OldLimits)
	}
}
