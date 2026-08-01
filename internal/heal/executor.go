package heal

import (
	"context"
	"errors"
	"fmt"

	"github.com/peceldev/podsmedic/internal/k8s"
)

// ErrGitOpsManaged means the target workload is reconciled from Git (ArgoCD,
// Flux, Helm) and must not be patched in place — the change would be reverted or
// start a fight with the GitOps controller. It is a skip, not a failure.
var ErrGitOpsManaged = errors.New("workload is GitOps-managed")

// Cluster is the subset of cluster operations the executor needs. k8s.Client
// implements it; a fake implements it in tests.
type Cluster interface {
	ResolveController(ctx context.Context, namespace, podName string) (k8s.ControllerRef, error)
	PatchContainerResources(ctx context.Context, ctrl k8s.ControllerRef, container string, limits, requests map[string]string, dryRun bool) error
	PatchContainerImage(ctx context.Context, ctrl k8s.ControllerRef, container, image string, dryRun bool) error
	PatchContainerProbe(ctx context.Context, ctrl k8s.ControllerRef, container, probeType string, fields map[string]int32, dryRun bool) error
	ScaleWorkload(ctx context.Context, ctrl k8s.ControllerRef, replicas int32, dryRun bool) error
	RestartWorkload(ctx context.Context, ctrl k8s.ControllerRef, dryRun bool) error
	CreatePVC(ctx context.Context, namespace, name, size, storageClass, accessMode string, dryRun bool) error
	// WorkloadManagedBy reports the GitOps manager owning the controller, or ""
	// if none.
	WorkloadManagedBy(ctx context.Context, ctrl k8s.ControllerRef) (string, error)
}

// Executor carries out a validated Plan against the cluster.
type Executor struct {
	cluster Cluster
	// apply is the master switch. When false, every change runs as a
	// server-side dry run: the API server validates it but nothing persists.
	// This makes enabling auto-heal a two-step commitment.
	apply bool
	// allowGitOps, when true, skips the GitOps-ownership guard — for operators
	// who deliberately let podsmedic patch reconciled workloads anyway.
	allowGitOps bool
}

// NewExecutor builds an executor. Pass apply=false to preview changes only, and
// allowGitOps=true to patch GitOps-managed workloads despite the guard.
func NewExecutor(cluster Cluster, apply, allowGitOps bool) *Executor {
	return &Executor{cluster: cluster, apply: apply, allowGitOps: allowGitOps}
}

// Outcome records what the executor did, for the alert and audit log.
type Outcome struct {
	Applied    bool              // false means it was a dry run
	Controller string            // the workload that was (or would be) changed
	Ref        k8s.ControllerRef // the resolved controller, for later verification
	Summary    string
}

// Execute resolves the plan's target controller and applies the change (or dry
// runs it). Resolving the controller is itself a safety gate: a pod owned by a
// kind podsmedic will not patch produces an error here rather than a mutation.
func (e *Executor) Execute(ctx context.Context, p *Plan) (*Outcome, error) {
	ctrl, err := e.cluster.ResolveController(ctx, p.Namespace, p.Pod)
	if err != nil {
		return nil, fmt.Errorf("resolve controller: %w", err)
	}

	// GitOps guard: a reconciled workload is owned by its repository, so patching
	// it here would be undone or contested. Refuse (as a skip) unless explicitly
	// allowed.
	if !e.allowGitOps {
		mgr, err := e.cluster.WorkloadManagedBy(ctx, ctrl)
		if err != nil {
			return nil, fmt.Errorf("check GitOps ownership of %s: %w", ctrl, err)
		}
		if mgr != "" {
			return nil, fmt.Errorf("%w by %s: fix it in the source repository, not in-cluster", ErrGitOpsManaged, mgr)
		}
	}

	dryRun := !e.apply

	switch p.Kind {
	case ActionPatchResources:
		if err := e.cluster.PatchContainerResources(ctx, ctrl, p.Container, p.Limits, p.Requests, dryRun); err != nil {
			return nil, err
		}
	case ActionPatchImage:
		if err := e.cluster.PatchContainerImage(ctx, ctrl, p.Container, p.Image, dryRun); err != nil {
			return nil, err
		}
	case ActionPatchProbe:
		if err := e.cluster.PatchContainerProbe(ctx, ctrl, p.Container, p.ProbeType, p.Probe, dryRun); err != nil {
			return nil, err
		}
	case ActionScaleReplicas:
		if err := e.cluster.ScaleWorkload(ctx, ctrl, p.Replicas, dryRun); err != nil {
			return nil, err
		}
	case ActionRestartWorkload:
		if err := e.cluster.RestartWorkload(ctx, ctrl, dryRun); err != nil {
			return nil, err
		}
	case ActionCreatePVC:
		if p.Claim == nil {
			return nil, fmt.Errorf("create_pvc plan carries no claim spec")
		}
		if err := e.cluster.CreatePVC(ctx, p.Claim.Namespace, p.Claim.Name, p.Claim.Size, p.Claim.StorageClass, p.Claim.AccessMode, dryRun); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("executor cannot run action kind %q", p.Kind)
	}

	return &Outcome{
		Applied:    e.apply,
		Controller: ctrl.String(),
		Ref:        ctrl,
		Summary:    p.Summary,
	}, nil
}

// Rollback restores a workload to the resource values recorded before a heal,
//
// Note what is absent: create_pvc has no rollback. Undoing it would mean
// deleting a claim, and by the time verification runs that claim may be bound to
// a volume holding data. An unwanted claim is left in place, labelled
// app.kubernetes.io/created-by=podsmedic so it is easy to find and remove by
// hand. The agent never records one for verification, so this is unreachable
// for that kind — the comment is here so it stays that way.
//
// used when verification finds the workload still failing. It patches only the
// keys the original heal changed (see RecordFromPlan), so unrelated fields are
// left alone. A value the heal *added* where none existed cannot be unset via a
// strategic-merge patch and is left in place — noted as a known limitation.
func (e *Executor) Rollback(ctx context.Context, rec HealRecord) error {
	// An image heal restores the prior image (which returns the workload to its
	// original failure, and the alert says so — better than leaving podsmedic's
	// wrong guess in place).
	if rec.NewImage != "" {
		if rec.OldImage == "" {
			return fmt.Errorf("nothing to roll back: no prior image was recorded")
		}
		return e.cluster.PatchContainerImage(ctx, rec.Ref(), rec.Container, rec.OldImage, !e.apply)
	}
	// A probe heal restores the prior probe timing.
	if rec.ProbeType != "" {
		if len(rec.OldProbe) == 0 {
			return fmt.Errorf("nothing to roll back: no prior probe values were recorded")
		}
		return e.cluster.PatchContainerProbe(ctx, rec.Ref(), rec.Container, rec.ProbeType, rec.OldProbe, !e.apply)
	}
	// A scale heal restores the prior replica count.
	if rec.NewReplicas > 0 {
		if rec.OldReplicas <= 0 {
			return fmt.Errorf("nothing to roll back: no prior replica count was recorded")
		}
		return e.cluster.ScaleWorkload(ctx, rec.Ref(), rec.OldReplicas, !e.apply)
	}
	if len(rec.OldLimits) == 0 && len(rec.OldRequests) == 0 {
		return fmt.Errorf("nothing to roll back: no prior resource values were recorded")
	}
	return e.cluster.PatchContainerResources(ctx, rec.Ref(), rec.Container, rec.OldLimits, rec.OldRequests, !e.apply)
}
