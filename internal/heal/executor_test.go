package heal

import (
	"context"
	"errors"
	"testing"

	"github.com/teknik-github/PodsMedic/internal/k8s"
)

// fakeCluster records executor calls and lets a test force a resolution error.
type fakeCluster struct {
	resolveErr   error
	patched      bool
	restarted    bool
	dryRun       bool
	gotLimits    map[string]string
	gotRequests  map[string]string
	gotContainer string
	gotImage     string
	gotProbeType string
	gotProbe     map[string]int32
	gotReplicas  int32
	managedBy    string // GitOps manager reported by WorkloadManagedBy
	createdPVC   *createdClaim
	createErr    error
}

// createdClaim records a CreatePVC call so a test can assert exactly what would
// be provisioned.
type createdClaim struct {
	namespace, name, size, class, accessMode string
	dryRun                                   bool
}

func (f *fakeCluster) CreatePVC(_ context.Context, namespace, name, size, class, accessMode string, dryRun bool) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.createdPVC = &createdClaim{namespace, name, size, class, accessMode, dryRun}
	return nil
}

func (f *fakeCluster) ScaleWorkload(_ context.Context, _ k8s.ControllerRef, replicas int32, dryRun bool) error {
	f.patched = true
	f.dryRun = dryRun
	f.gotReplicas = replicas
	return nil
}

func (f *fakeCluster) ResolveController(_ context.Context, ns, pod string) (k8s.ControllerRef, error) {
	if f.resolveErr != nil {
		return k8s.ControllerRef{}, f.resolveErr
	}
	return k8s.ControllerRef{Kind: "Deployment", Name: "web", Namespace: ns}, nil
}

func (f *fakeCluster) WorkloadManagedBy(_ context.Context, _ k8s.ControllerRef) (string, error) {
	return f.managedBy, nil
}

func (f *fakeCluster) PatchContainerResources(_ context.Context, _ k8s.ControllerRef, container string, limits, requests map[string]string, dryRun bool) error {
	f.patched = true
	f.dryRun = dryRun
	f.gotLimits = limits
	f.gotRequests = requests
	f.gotContainer = container
	return nil
}

func (f *fakeCluster) PatchContainerImage(_ context.Context, _ k8s.ControllerRef, container, image string, dryRun bool) error {
	f.patched = true
	f.dryRun = dryRun
	f.gotContainer = container
	f.gotImage = image
	return nil
}

func (f *fakeCluster) PatchContainerProbe(_ context.Context, _ k8s.ControllerRef, container, probeType string, fields map[string]int32, dryRun bool) error {
	f.patched = true
	f.dryRun = dryRun
	f.gotContainer = container
	f.gotProbeType = probeType
	f.gotProbe = fields
	return nil
}

func (f *fakeCluster) RestartWorkload(_ context.Context, _ k8s.ControllerRef, dryRun bool) error {
	f.restarted = true
	f.dryRun = dryRun
	return nil
}

func TestExecutorAppliesPatchWhenEnabled(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	plan := &Plan{Kind: ActionPatchResources, Namespace: "api", Pod: "web-1", Container: "web", Limits: map[string]string{"memory": "256Mi"}}
	out, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !fc.patched || fc.dryRun {
		t.Fatalf("expected a real patch, got patched=%v dryRun=%v", fc.patched, fc.dryRun)
	}
	if !out.Applied {
		t.Fatal("outcome should report Applied=true")
	}
}

func TestExecutorDryRunsWhenApplyDisabled(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, false, false)

	plan := &Plan{Kind: ActionPatchResources, Namespace: "api", Pod: "web-1", Container: "web", Limits: map[string]string{"memory": "256Mi"}}
	out, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !fc.patched || !fc.dryRun {
		t.Fatalf("expected a dry-run patch, got patched=%v dryRun=%v", fc.patched, fc.dryRun)
	}
	if out.Applied {
		t.Fatal("outcome should report Applied=false for a dry run")
	}
}

func TestExecutorRollbackRestoresOldValues(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	rec := HealRecord{
		ControllerKind: "Deployment", ControllerName: "web", Namespace: "api",
		Container: "web",
		OldLimits: map[string]string{"memory": "128Mi"},
	}
	if err := e.Rollback(context.Background(), rec); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !fc.patched || fc.dryRun {
		t.Fatalf("rollback should apply a real patch, got patched=%v dryRun=%v", fc.patched, fc.dryRun)
	}
	if fc.gotLimits["memory"] != "128Mi" || fc.gotContainer != "web" {
		t.Fatalf("rollback patched wrong values: container=%q limits=%v", fc.gotContainer, fc.gotLimits)
	}
}

func TestExecutorRollbackWithoutPriorValuesErrors(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	rec := HealRecord{ControllerKind: "Deployment", ControllerName: "web", Namespace: "api", Container: "web"}
	if err := e.Rollback(context.Background(), rec); err == nil {
		t.Fatal("rollback with no recorded prior values must error, not patch blindly")
	}
	if fc.patched {
		t.Fatal("nothing should be patched when there is nothing to restore")
	}
}

func TestExecutorAppliesImagePatch(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	plan := &Plan{Kind: ActionPatchImage, Namespace: "api", Pod: "web-1", Container: "web", Image: "ghcr.io/acme/web:v1.2.4"}
	out, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fc.gotImage != "ghcr.io/acme/web:v1.2.4" || fc.gotContainer != "web" {
		t.Fatalf("image patch wrong: container=%q image=%q", fc.gotContainer, fc.gotImage)
	}
	if !out.Applied {
		t.Fatal("outcome should report Applied=true")
	}
}

func TestExecutorRollbackRestoresOldImage(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	rec := HealRecord{
		ControllerKind: "Deployment", ControllerName: "web", Namespace: "api", Container: "web",
		OldImage: "ghcr.io/acme/web:v1.2.3", NewImage: "ghcr.io/acme/web:v1.2.4",
	}
	if err := e.Rollback(context.Background(), rec); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if fc.gotImage != "ghcr.io/acme/web:v1.2.3" {
		t.Fatalf("rollback should restore the old image, got %q", fc.gotImage)
	}
}

func TestExecutorAppliesProbePatch(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	plan := &Plan{Kind: ActionPatchProbe, Namespace: "api", Pod: "web-1", Container: "web", ProbeType: "liveness", Probe: map[string]int32{"initialDelaySeconds": 30}}
	if _, err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fc.gotProbeType != "liveness" || fc.gotProbe["initialDelaySeconds"] != 30 {
		t.Fatalf("probe patch wrong: type=%q fields=%v", fc.gotProbeType, fc.gotProbe)
	}
}

func TestExecutorRollbackRestoresOldProbe(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	rec := HealRecord{
		ControllerKind: "Deployment", ControllerName: "web", Namespace: "api", Container: "web",
		ProbeType: "liveness",
		OldProbe:  map[string]int32{"initialDelaySeconds": 0},
		NewProbe:  map[string]int32{"initialDelaySeconds": 30},
	}
	if err := e.Rollback(context.Background(), rec); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if fc.gotProbe["initialDelaySeconds"] != 0 {
		t.Fatalf("rollback should restore old probe, got %v", fc.gotProbe)
	}
}

func TestExecutorSkipsGitOpsManaged(t *testing.T) {
	fc := &fakeCluster{managedBy: "argocd"}
	e := NewExecutor(fc, true, false)

	plan := &Plan{Kind: ActionPatchResources, Namespace: "api", Pod: "web-1", Container: "web", Limits: map[string]string{"memory": "256Mi"}}
	_, err := e.Execute(context.Background(), plan)
	if !errors.Is(err, ErrGitOpsManaged) {
		t.Fatalf("a GitOps-managed workload must be refused with ErrGitOpsManaged, got %v", err)
	}
	if fc.patched {
		t.Fatal("must not patch a GitOps-managed workload")
	}
}

func TestExecutorAllowGitOpsOverride(t *testing.T) {
	fc := &fakeCluster{managedBy: "flux"}
	e := NewExecutor(fc, true, true) // allowGitOps

	plan := &Plan{Kind: ActionPatchResources, Namespace: "api", Pod: "web-1", Container: "web", Limits: map[string]string{"memory": "256Mi"}}
	if _, err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("with allowGitOps the patch should proceed, got %v", err)
	}
	if !fc.patched {
		t.Fatal("expected the patch to be applied when GitOps is allowed")
	}
}

func TestExecutorSurfacesResolveError(t *testing.T) {
	fc := &fakeCluster{resolveErr: errors.New("owner kind \"Job\" is not auto-healable")}
	e := NewExecutor(fc, true, false)

	plan := &Plan{Kind: ActionRestartWorkload, Namespace: "api", Pod: "web-1"}
	if _, err := e.Execute(context.Background(), plan); err == nil {
		t.Fatal("expected the resolve error to propagate")
	}
	if fc.patched || fc.restarted {
		t.Fatal("nothing should be mutated when the controller cannot be resolved")
	}
}

func TestExecuteCreatePVCPassesTheValidatedSpec(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	plan := &Plan{
		Kind: ActionCreatePVC, Namespace: "data", Pod: "db-0",
		Claim: &ClaimSpec{Namespace: "data", Name: "db-data", Size: "2Gi", StorageClass: "fast", AccessMode: "ReadWriteOnce"},
	}
	if _, err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := fc.createdPVC
	if got == nil {
		t.Fatal("no claim was created")
	}
	if got.namespace != "data" || got.name != "db-data" || got.size != "2Gi" || got.class != "fast" || got.accessMode != "ReadWriteOnce" {
		t.Fatalf("executor must pass the validated spec verbatim, got %+v", got)
	}
	if got.dryRun {
		t.Fatal("expected a real create with apply=true")
	}
}

func TestExecuteCreatePVCHonoursDryRun(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, false, false) // apply=false

	plan := &Plan{
		Kind: ActionCreatePVC, Namespace: "data", Pod: "db-0",
		Claim: &ClaimSpec{Namespace: "data", Name: "db-data", Size: "1Gi", AccessMode: "ReadWriteOnce"},
	}
	if _, err := e.Execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fc.createdPVC == nil || !fc.createdPVC.dryRun {
		t.Fatalf("a dry run must not create for real: %+v", fc.createdPVC)
	}
}

func TestExecuteCreatePVCRejectsPlanWithoutClaim(t *testing.T) {
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	if _, err := e.Execute(context.Background(), &Plan{Kind: ActionCreatePVC, Namespace: "data", Pod: "db-0"}); err == nil {
		t.Fatal("a create plan with no claim spec must error rather than create something invented")
	}
	if fc.createdPVC != nil {
		t.Fatal("nothing should have been created")
	}
}

func TestRollbackNeverDeletesACreatedClaim(t *testing.T) {
	// Undoing a create would mean deleting a claim that may by now be bound to a
	// volume holding data. There is no rollback path for it, and a record that
	// somehow reached here must fail loudly rather than improvise.
	fc := &fakeCluster{}
	e := NewExecutor(fc, true, false)

	err := e.Rollback(context.Background(), HealRecord{Namespace: "data", ControllerKind: "Deployment", ControllerName: "db"})
	if err == nil {
		t.Fatal("a record with nothing to restore must error, not guess")
	}
	if fc.patched || fc.restarted {
		t.Fatal("rollback must not have touched the workload")
	}
}
