package heal

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/teknik-github/PodsMedic/internal/capacity"
	"github.com/teknik-github/PodsMedic/internal/detect"
	"github.com/teknik-github/PodsMedic/internal/k8s"
)

// ErrNoSafeAction means the proposal did not clear the safety checks and the
// pod should be alerted on but not auto-healed.
var ErrNoSafeAction = errors.New("no safe automated action")

// Options bounds what Validate will permit. Zero values are unlimited/none, so
// callers should build these from config with real caps.
type Options struct {
	// MaxMemory / MaxCPU are absolute ceilings a limit may be raised to.
	MaxMemory resource.Quantity
	MaxCPU    resource.Quantity
	// MaxMultiplier caps how many times the current value a new value may be
	// (e.g. 4 means a limit can at most quadruple in one step). Ignored when
	// the current value is zero/unset.
	MaxMultiplier float64
	// MinConfidence gates healing on the diagnosis confidence ("high" is
	// strongest). Only a diagnosis at or above this confidence is acted on.
	MinConfidence string
	// AllowRequests permits patching resource *requests*. Off by default:
	// raising a request can push a pod past node capacity and leave it stuck
	// Pending — a worse state than the crash being fixed. Raising limits is
	// always allowed because it never affects scheduling.
	AllowRequests bool
	// Probe loosening caps. A proposed probe value may only increase, and never
	// past these ceilings — so a probe can be relaxed but not effectively
	// disabled. Zero means no cap for that field.
	MaxProbeInitialDelaySeconds int32
	MaxProbePeriodSeconds       int32
	MaxProbeTimeoutSeconds      int32
	MaxProbeFailureThreshold    int32
	// MaxReplicas is an optional absolute ceiling a scale_replicas heal may
	// raise a workload to — a hand-set backstop over the derived target. Zero
	// means no explicit backstop; scaling is then bounded by measured capacity
	// and MaxMultiplier alone. Scaling is disabled entirely by clearing
	// AutoReplicas *and* leaving MaxReplicas at zero.
	MaxReplicas int32
	// AutoReplicas derives the replica target from the workload's measured CPU
	// utilisation and the cluster's free capacity, instead of trusting a number
	// from the model. Off means the model's proposal is used as the target
	// (still capacity-checked and still capped).
	AutoReplicas bool
	// TargetCPURatio is the utilisation a derived scale-up aims to bring the
	// workload back down to (0.7 = 70% of its CPU limit). Lower spreads load
	// more aggressively and costs more pods.
	TargetCPURatio float64
	// PVC creation. Off by default, and the only creating action podsmedic has.
	// PVCAutoCreate is the master switch; the size and class are taken from here
	// rather than from the model, and PVCMaxSize is the hard ceiling a
	// misconfigured default is capped to.
	PVCAutoCreate   bool
	PVCDefaultSize  string
	PVCStorageClass string
	PVCMaxSize      resource.Quantity
	// AllowedKinds is the set of detected problem kinds eligible for healing.
	AllowedKinds map[detect.Kind]bool
	// DenyNamespaces are never touched (control-plane namespaces by default).
	DenyNamespaces map[string]bool
	// AllowNamespaces, when non-empty, restricts healing to these namespaces.
	AllowNamespaces map[string]bool
}

// Validate turns an untrusted proposal into a bounded, executable Plan, or
// returns ErrNoSafeAction. It is pure — no cluster calls — so it is fully unit
// tested, which is where the safety guarantees live.
func Validate(b *k8s.Bundle, confidence string, a Action, opts Options) (*Plan, error) {
	if a.Kind == ActionNone || a.Kind == "" {
		return nil, ErrNoSafeAction
	}

	// Gate: a storage fault admits only two recoveries, and nothing else.
	//
	// Everything a storage failure might tempt you into — resizing a claim,
	// recreating it, forcing a detach, deleting the bound volume — is either
	// irreversible or destroys data, so none of it is available here whatever the
	// configuration says. What is left is genuinely safe:
	//
	//   restart_workload — only once every claim is Bound again. The storage is
	//     healthy and the pod is merely stuck on a stale mount or an attachment
	//     that has since been released. Touches no storage object at all.
	//   create_pvc      — only for a claim that does not exist. Creating is
	//     additive; the worst case is an unused volume, not lost data.
	//
	// Any other action kind on a storage problem is refused above the allowlist,
	// so putting PVCPending in PODSMEDIC_HEAL_KINDS cannot widen this.
	if b.Problem.Kind.Storage() && a.Kind != ActionRestartWorkload && a.Kind != ActionCreatePVC {
		return nil, fmt.Errorf("%w: %s is a storage fault; only restart_workload (once claims are bound) or create_pvc (for a claim that does not exist) are ever permitted, not %s",
			ErrNoSafeAction, b.Problem.Kind, a.Kind)
	}
	// The mirror of the rule above: creating a claim is only ever a response to a
	// storage fault, never to an OOM or a crash loop.
	if a.Kind == ActionCreatePVC && !b.Problem.Kind.Storage() {
		return nil, fmt.Errorf("%w: create_pvc is only valid for a storage fault, not %s", ErrNoSafeAction, b.Problem.Kind)
	}

	// Gate: only heal problems we have opted into.
	if len(opts.AllowedKinds) > 0 && !opts.AllowedKinds[b.Problem.Kind] {
		return nil, fmt.Errorf("%w: problem kind %s is not in the heal allowlist", ErrNoSafeAction, b.Problem.Kind)
	}

	// Gate: confidence. A low-confidence guess must not mutate the cluster.
	if !confidenceAtLeast(confidence, opts.MinConfidence) {
		return nil, fmt.Errorf("%w: diagnosis confidence %q is below the %q threshold", ErrNoSafeAction, confidence, opts.MinConfidence)
	}

	// Gate: namespace. Control-plane namespaces are off-limits; an allowlist,
	// if set, further narrows the scope.
	ns := b.Problem.Namespace
	if opts.DenyNamespaces[ns] {
		return nil, fmt.Errorf("%w: namespace %q is denied for healing", ErrNoSafeAction, ns)
	}
	if len(opts.AllowNamespaces) > 0 && !opts.AllowNamespaces[ns] {
		return nil, fmt.Errorf("%w: namespace %q is not in the heal allowlist", ErrNoSafeAction, ns)
	}

	switch a.Kind {
	case ActionPatchResources:
		return validatePatch(b, a, opts)
	case ActionRestartWorkload:
		return validateRestart(b, a)
	case ActionPatchImage:
		return validatePatchImage(b, a)
	case ActionPatchProbe:
		return validatePatchProbe(b, a, opts)
	case ActionScaleReplicas:
		return validateScaleReplicas(b, a, opts)
	case ActionCreatePVC:
		return validateCreatePVC(b, a, opts)
	default:
		return nil, fmt.Errorf("%w: unknown action kind %q", ErrNoSafeAction, a.Kind)
	}
}

// validateCreatePVC creates a claim a pod references but that was never
// created. It is podsmedic's only creating action, so every bound is explicit.
//
// The name and namespace come from the pod's own spec; the size, class, and
// access mode from configuration. The model contributes nothing but the choice
// of action — a provisioned volume costs money and cannot be shrunk once bound,
// which makes it a poor thing to size from untrusted input.
func validateCreatePVC(b *k8s.Bundle, a Action, opts Options) (*Plan, error) {
	if !opts.PVCAutoCreate {
		return nil, fmt.Errorf("%w: creating claims is disabled (set PODSMEDIC_PVC_AUTOCREATE=true)", ErrNoSafeAction)
	}
	// Stricter than every other action: an empty namespace allowlist means "all
	// namespaces" elsewhere, but provisioning storage cluster-wide by default is
	// not a reasonable reading of any configuration. An explicit list is required.
	if len(opts.AllowNamespaces) == 0 {
		return nil, fmt.Errorf("%w: creating claims requires an explicit namespace allowlist (PODSMEDIC_HEAL_NAMESPACES)", ErrNoSafeAction)
	}

	missing := missingClaims(b)
	switch {
	case len(missing) == 0:
		return nil, fmt.Errorf("%w: every claim this pod mounts already exists; a claim is never modified, only created", ErrNoSafeAction)
	case len(missing) > 1:
		// Which one is the real omission, and what size each should be, is not
		// something to guess at.
		return nil, fmt.Errorf("%w: %d claims are missing (%s) — too ambiguous to create automatically", ErrNoSafeAction, len(missing), strings.Join(missing, ", "))
	}

	// A single claim shared by several replicas needs ReadWriteMany, which many
	// storage classes do not offer. Guessing wrong produces a volume that binds
	// and then blocks every replica but one, which is worse than the Pending pod.
	if b.Replicas > 1 {
		return nil, fmt.Errorf("%w: the workload has %d replicas sharing one claim, so the required access mode is ambiguous — create it by hand", ErrNoSafeAction, b.Replicas)
	}

	size, err := boundedClaimSize(opts)
	if err != nil {
		return nil, err
	}

	claim := &ClaimSpec{
		Namespace:    b.Problem.Namespace,
		Name:         missing[0],
		Size:         size,
		StorageClass: opts.PVCStorageClass,
		AccessMode:   "ReadWriteOnce",
	}
	where := "the cluster default storage class"
	if claim.StorageClass != "" {
		where = "storage class " + claim.StorageClass
	}
	return &Plan{
		Kind:      ActionCreatePVC,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Claim:     claim,
		Reason:    a.Reason,
		Summary:   fmt.Sprintf("create PersistentVolumeClaim %q in %s: %s, ReadWriteOnce, %s", claim.Name, claim.Namespace, claim.Size, where),
	}, nil
}

// missingClaims names the claims the pod mounts that do not exist. It reads the
// collected evidence rather than the cluster, so validation stays pure.
func missingClaims(b *k8s.Bundle) []string {
	var out []string
	for _, c := range b.Claims {
		if c.Missing {
			out = append(out, c.ClaimName)
		}
	}
	return out
}

// boundedClaimSize resolves the configured size against the hard ceiling. A
// default above the ceiling is a misconfiguration, and it is capped rather than
// honoured.
func boundedClaimSize(opts Options) (string, error) {
	want, err := resource.ParseQuantity(opts.PVCDefaultSize)
	if err != nil || want.Sign() <= 0 {
		return "", fmt.Errorf("%w: PODSMEDIC_PVC_DEFAULT_SIZE %q is not a positive quantity", ErrNoSafeAction, opts.PVCDefaultSize)
	}
	if !opts.PVCMaxSize.IsZero() && want.Cmp(opts.PVCMaxSize) > 0 {
		return opts.PVCMaxSize.String(), nil
	}
	return want.String(), nil
}

// allClaimsBound reports whether every claim the pod mounts is Bound — the
// precondition for recovering a stuck pod with a restart. A pod with no claims
// at all trivially satisfies it.
func allClaimsBound(b *k8s.Bundle) bool {
	for _, c := range b.Claims {
		if c.Phase != "Bound" {
			return false
		}
	}
	return true
}

func validateRestart(b *k8s.Bundle, a Action) (*Plan, error) {
	// On a storage fault, a restart is only recovery once the storage itself is
	// healthy again: the volume has been provisioned, or the old attachment has
	// been released, and all that remains is a pod holding a stale mount.
	// Restarting while a claim is still unbound just churns a workload that
	// cannot possibly start.
	if b.Problem.Kind.Storage() {
		if !allClaimsBound(b) {
			return nil, fmt.Errorf("%w: a claim this pod mounts is still not Bound, so a restart would only recreate a pod that cannot start — fix the volume first", ErrNoSafeAction)
		}
		if len(b.Claims) == 0 {
			return nil, fmt.Errorf("%w: no claim evidence available, so there is no way to confirm the storage recovered", ErrNoSafeAction)
		}
	}

	// A restart is only meaningful for a controller-owned pod; a bare pod would
	// just be gone. Owner resolution happens at execution time, but a pod with
	// no owner reference is rejected up front.
	if b.Pod.OwnerKind == "" {
		return nil, fmt.Errorf("%w: pod has no controller to restart", ErrNoSafeAction)
	}
	return &Plan{
		Kind:      ActionRestartWorkload,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Reason:    a.Reason,
		Summary:   fmt.Sprintf("rollout restart of the controller owning %s/%s", b.Problem.Namespace, b.Problem.Pod),
	}, nil
}

func validatePatch(b *k8s.Bundle, a Action, opts Options) (*Plan, error) {
	container := a.Container
	if container == "" {
		container = b.Problem.Container
	}
	if container == "" {
		return nil, fmt.Errorf("%w: patch_resources needs a target container", ErrNoSafeAction)
	}

	cur := findContainer(b, container)
	if cur == nil {
		return nil, fmt.Errorf("%w: container %q not found in pod", ErrNoSafeAction, container)
	}

	limits := map[string]string{}
	requests := map[string]string{}
	var changes []string

	// Each resource is validated independently. A single bad value rejects the
	// whole plan rather than applying a partial, possibly inconsistent change.
	if a.MemoryLimit != "" {
		v, desc, err := boundedIncrease("memory", "limit", cur.Limits["memory"], a.MemoryLimit, opts.MaxMemory, opts.MaxMultiplier)
		if err != nil {
			return nil, err
		}
		limits["memory"] = v
		changes = append(changes, desc)
	}
	if a.CPULimit != "" {
		v, desc, err := boundedIncrease("cpu", "limit", cur.Limits["cpu"], a.CPULimit, opts.MaxCPU, opts.MaxMultiplier)
		if err != nil {
			return nil, err
		}
		limits["cpu"] = v
		changes = append(changes, desc)
	}
	// Requests move the scheduling floor, so they are gated behind a separate
	// opt-in and additionally checked against node capacity below.
	if (a.MemoryRequest != "" || a.CPURequest != "") && !opts.AllowRequests {
		return nil, fmt.Errorf("%w: proposal changes resource requests, but request patching is disabled (set PODSMEDIC_HEAL_PATCH_REQUESTS=true) — requests affect scheduling and can strand a pod as Pending", ErrNoSafeAction)
	}
	if a.MemoryRequest != "" {
		v, desc, err := boundedIncrease("memory", "request", cur.Requests["memory"], a.MemoryRequest, opts.MaxMemory, opts.MaxMultiplier)
		if err != nil {
			return nil, err
		}
		requests["memory"] = v
		changes = append(changes, desc)
	}
	if a.CPURequest != "" {
		v, desc, err := boundedIncrease("cpu", "request", cur.Requests["cpu"], a.CPURequest, opts.MaxCPU, opts.MaxMultiplier)
		if err != nil {
			return nil, err
		}
		requests["cpu"] = v
		changes = append(changes, desc)
	}
	// Capacity is checked once, on the pod's *projected total*, not per resource:
	// two individually-affordable raises can still add up to a pod that fits
	// nowhere.
	if len(requests) > 0 {
		if err := schedulable(b, cur, requests); err != nil {
			return nil, err
		}
	}

	if len(limits) == 0 && len(requests) == 0 {
		return nil, fmt.Errorf("%w: patch_resources proposed no resource changes", ErrNoSafeAction)
	}

	return &Plan{
		Kind:      ActionPatchResources,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Container: container,
		Limits:    nilIfEmpty(limits),
		Requests:  nilIfEmpty(requests),
		Reason:    a.Reason,
		Summary:   fmt.Sprintf("patch container %q: %s", container, strings.Join(changes, ", ")),
	}, nil
}

// validatePatchImage accepts a corrected image only when it is unmistakably the
// same artifact source as the current one — same registry and repository, with
// only the tag or digest changed. This is the security crux: pod logs are
// untrusted, so a proposal that would point the workload at any *other* image
// (a different repo or registry) is refused outright, closing the obvious
// supply-chain attack via prompt injection.
func validatePatchImage(b *k8s.Bundle, a Action) (*Plan, error) {
	container := a.Container
	if container == "" {
		container = b.Problem.Container
	}
	if container == "" {
		return nil, fmt.Errorf("%w: patch_image needs a target container", ErrNoSafeAction)
	}
	cur := findContainer(b, container)
	if cur == nil {
		return nil, fmt.Errorf("%w: container %q not found in pod", ErrNoSafeAction, container)
	}

	newImage, desc, err := boundedImage(cur.Image, a.Image)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Kind:      ActionPatchImage,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Container: container,
		Image:     newImage,
		Reason:    a.Reason,
		Summary:   fmt.Sprintf("patch container %q image: %s", container, desc),
	}, nil
}

// validatePatchProbe accepts only a loosening of a probe's timing. Every field
// may increase but never decrease, and never past its cap — so a probe can be
// relaxed to survive a slow start, but never tightened (which would kill faster)
// or effectively disabled. The probe target (path/port/command) is deliberately
// never changed here; that class of fix is alert-only.
func validatePatchProbe(b *k8s.Bundle, a Action, opts Options) (*Plan, error) {
	container := a.Container
	if container == "" {
		container = b.Problem.Container
	}
	if container == "" {
		return nil, fmt.Errorf("%w: patch_probe needs a target container", ErrNoSafeAction)
	}
	cur := findContainer(b, container)
	if cur == nil {
		return nil, fmt.Errorf("%w: container %q not found in pod", ErrNoSafeAction, container)
	}

	pt := strings.ToLower(a.ProbeType)
	if pt != "liveness" && pt != "readiness" {
		return nil, fmt.Errorf("%w: probe type %q must be liveness or readiness", ErrNoSafeAction, a.ProbeType)
	}
	curProbe := cur.Probes[pt]
	if curProbe == nil {
		return nil, fmt.Errorf("%w: container %q has no %s probe to adjust", ErrNoSafeAction, container, pt)
	}

	fields := map[string]int32{}
	var changes []string
	// Each field: propose>0 means "set to this"; accepted only if it is an
	// increase over the current value and within the cap.
	add := func(name string, proposed, current, cap int32) error {
		if proposed == 0 {
			return nil // unchanged
		}
		if proposed < current {
			return fmt.Errorf("%w: proposed %s %s %d is below the current %d (refusing to tighten a probe)", ErrNoSafeAction, pt, name, proposed, current)
		}
		if proposed == current {
			return nil // no-op
		}
		if cap > 0 && proposed > cap {
			return fmt.Errorf("%w: proposed %s %s %d exceeds the cap %d", ErrNoSafeAction, pt, name, proposed, cap)
		}
		fields[name] = proposed
		changes = append(changes, fmt.Sprintf("%s %d→%d", name, current, proposed))
		return nil
	}

	if err := add("initialDelaySeconds", a.ProbeInitialDelaySeconds, curProbe.InitialDelaySeconds, opts.MaxProbeInitialDelaySeconds); err != nil {
		return nil, err
	}
	if err := add("periodSeconds", a.ProbePeriodSeconds, curProbe.PeriodSeconds, opts.MaxProbePeriodSeconds); err != nil {
		return nil, err
	}
	if err := add("timeoutSeconds", a.ProbeTimeoutSeconds, curProbe.TimeoutSeconds, opts.MaxProbeTimeoutSeconds); err != nil {
		return nil, err
	}
	if err := add("failureThreshold", a.ProbeFailureThreshold, curProbe.FailureThreshold, opts.MaxProbeFailureThreshold); err != nil {
		return nil, err
	}

	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: patch_probe proposed no loosening change", ErrNoSafeAction)
	}

	return &Plan{
		Kind:      ActionPatchProbe,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Container: container,
		ProbeType: pt,
		Probe:     fields,
		Reason:    a.Reason,
		Summary:   fmt.Sprintf("loosen %s probe on %q: %s", pt, container, strings.Join(changes, ", ")),
	}, nil
}

// validateScaleReplicas accepts only a bounded scale-up, and only into space
// the cluster demonstrably has.
//
// The replica count comes from one of two places. With AutoReplicas (the
// default) it is *derived*: the workload's measured CPU utilisation across its
// replicas, run through the standard utilisation formula, so the number is
// arithmetic over live data rather than a figure the model produced from
// untrusted logs. The model's own proposal can then only ever lower it. Without
// AutoReplicas the model's number is the target, and every bound is a hard
// refusal — an out-of-range proposal from an untrusted source is a signal, not
// something to quietly round down.
//
// Either way the target is gated on real headroom. b.Capacity is the cluster's
// remaining schedulable space with a reserve already held back; a nil snapshot
// means capacity could not be read, and this refuses rather than adds pods
// blind. That is the whole point of the gate: an unbounded scale-up is how a
// cluster ends up with more pods than its nodes and control plane can serve.
//
// Workload-level: no container.
func validateScaleReplicas(b *k8s.Bundle, a Action, opts Options) (*Plan, error) {
	if !opts.AutoReplicas && opts.MaxReplicas <= 0 {
		return nil, fmt.Errorf("%w: scaling is disabled (PODSMEDIC_HEAL_MAX_REPLICAS=0)", ErrNoSafeAction)
	}
	// Something else already owns spec.replicas. Writing it too would produce two
	// controllers overwriting each other every reconcile — the same reason a
	// GitOps-managed workload is left alone. The HPA is also better informed:
	// it is built for this and runs continuously, where podsmedic sees the
	// workload once a sweep.
	if b.Autoscaler != nil {
		return nil, fmt.Errorf("%w: HorizontalPodAutoscaler %s already manages this workload's replica count — scaling it here would fight the autoscaler; raise its maxReplicas instead",
			ErrNoSafeAction, b.Autoscaler)
	}

	current := b.Replicas
	if current <= 0 {
		return nil, fmt.Errorf("%w: workload replica count is unknown or not scalable", ErrNoSafeAction)
	}

	target, why, err := scaleTarget(b, a, opts, current)
	if err != nil {
		return nil, err
	}
	if target <= current {
		return nil, fmt.Errorf("%w: target %d replicas is not an increase over the current %d (refusing to scale down)", ErrNoSafeAction, target, current)
	}

	// Capacity gate. Fail closed: no snapshot, no scaling.
	if b.Capacity == nil {
		return nil, fmt.Errorf("%w: cluster capacity is unreadable (node or pod list denied) — refusing to add replicas without knowing whether they fit", ErrNoSafeAction)
	}
	fit := b.Capacity.FitAdditional(b.PodRequests)
	if fit <= 0 {
		return nil, fmt.Errorf("%w: the cluster has no room for another replica of this pod (%s); %s — scaling now would only add Pending pods",
			ErrNoSafeAction, b.PodRequests, b.Capacity.Summary().Describe())
	}
	if headroomCap := addClamped(current, fit); target > headroomCap {
		target = headroomCap
		why += fmt.Sprintf("; trimmed to %d by cluster headroom (room for %d more)", target, fit)
	}

	// Policy bounds. A derived target is our own arithmetic, so clamping it to
	// policy still delivers partial relief and the next sweep re-evaluates. A
	// model-proposed target that breaches a bound is refused outright.
	if opts.MaxMultiplier > 0 {
		if ceiling := int32(float64(current) * opts.MaxMultiplier); target > ceiling {
			if !opts.AutoReplicas {
				return nil, fmt.Errorf("%w: proposed %d replicas is more than %.1fx the current %d", ErrNoSafeAction, target, opts.MaxMultiplier, current)
			}
			target = ceiling
			why += fmt.Sprintf("; capped at %d by the %.1fx step limit", target, opts.MaxMultiplier)
		}
	}
	if opts.MaxReplicas > 0 && target > opts.MaxReplicas {
		if !opts.AutoReplicas {
			return nil, fmt.Errorf("%w: proposed %d replicas exceeds the cap %d", ErrNoSafeAction, target, opts.MaxReplicas)
		}
		target = opts.MaxReplicas
		why += fmt.Sprintf("; capped at the configured backstop of %d", target)
	}

	if target <= current {
		return nil, fmt.Errorf("%w: every bound trims the target back to the current %d replicas", ErrNoSafeAction, current)
	}

	summary := fmt.Sprintf("scale controller owning %s/%s: %d→%d replicas", b.Problem.Namespace, b.Problem.Pod, current, target)
	if why != "" {
		summary += " (" + why + ")"
	}
	return &Plan{
		Kind:      ActionScaleReplicas,
		Namespace: b.Problem.Namespace,
		Pod:       b.Problem.Pod,
		Replicas:  target,
		Reason:    a.Reason,
		Summary:   summary,
	}, nil
}

// scaleTarget produces the replica count to aim for, before capacity and policy
// bounds are applied, along with a human explanation of where it came from.
func scaleTarget(b *k8s.Bundle, a Action, opts Options, current int32) (int32, string, error) {
	if !opts.AutoReplicas {
		return a.Replicas, "proposed by the diagnosis", nil
	}

	if b.Load == nil {
		return 0, "", fmt.Errorf("%w: no live CPU measurements for this workload, so a replica count cannot be derived (metrics-server missing?)", ErrNoSafeAction)
	}
	target, why, err := capacity.TargetReplicas(*b.Load, opts.TargetCPURatio)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %s", ErrNoSafeAction, err)
	}

	// The model's number is untrusted, so it may only ever make the derived
	// target smaller — never argue it upward.
	if a.Replicas > current && a.Replicas < target {
		why += fmt.Sprintf("; held to the diagnosis's more conservative %d", a.Replicas)
		target = a.Replicas
	}
	return target, why, nil
}

// addClamped adds a capacity fit (an int64 that can be very large on a big
// cluster) to a replica count without overflowing int32.
func addClamped(current int32, fit int64) int32 {
	const maxInt32 = int64(^uint32(0) >> 1)
	if sum := int64(current) + fit; sum < maxInt32 {
		return int32(sum)
	}
	return int32(maxInt32)
}

// boundedImage enforces the same-repository rule and rejects mutable/ambiguous
// references. It returns the accepted image and a human description.
func boundedImage(current, proposed string) (string, string, error) {
	if proposed == "" {
		return "", "", fmt.Errorf("%w: patch_image proposed no image", ErrNoSafeAction)
	}
	if current == "" {
		// Without a current image there is no repository to pin against, so we
		// cannot bound the change safely.
		return "", "", fmt.Errorf("%w: current image is unknown, cannot bound an image change", ErrNoSafeAction)
	}

	curRepo, curTag, curDigest := parseImageRef(current)
	newRepo, newTag, newDigest := parseImageRef(proposed)

	if newRepo != curRepo {
		return "", "", fmt.Errorf("%w: proposed image repository %q differs from the current %q (only the tag/digest may change)", ErrNoSafeAction, newRepo, curRepo)
	}
	if newTag == "" && newDigest == "" {
		return "", "", fmt.Errorf("%w: proposed image must pin a tag or digest", ErrNoSafeAction)
	}
	if newTag == "latest" {
		return "", "", fmt.Errorf("%w: refusing the mutable %q tag", ErrNoSafeAction, "latest")
	}
	if newTag == curTag && newDigest == curDigest {
		return "", "", fmt.Errorf("%w: proposed image is identical to the current one", ErrNoSafeAction)
	}

	return proposed, fmt.Sprintf("%s→%s", current, proposed), nil
}

// parseImageRef splits an image reference into repository (registry + path),
// tag, and digest. The tag separator is the last colon that follows the last
// slash, so a registry port (host:5000/...) is not mistaken for a tag.
func parseImageRef(ref string) (repo, tag, digest string) {
	rest := ref
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		digest = rest[at+1:]
		rest = rest[:at]
	}
	slash := strings.LastIndex(rest, "/")
	if colon := strings.LastIndex(rest, ":"); colon > slash {
		tag = rest[colon+1:]
		rest = rest[:colon]
	}
	repo = rest
	return repo, tag, digest
}

// boundedIncrease enforces the core safety rule for resource edits: a value may
// only rise, may not exceed the absolute cap, and may not exceed the current
// value times the multiplier. It returns the accepted value and a human
// description, or an error that wraps ErrNoSafeAction.
func boundedIncrease(res, field, current, proposed string, cap resource.Quantity, multiplier float64) (string, string, error) {
	want, err := resource.ParseQuantity(proposed)
	if err != nil {
		return "", "", fmt.Errorf("%w: proposed %s %s %q is not a valid quantity", ErrNoSafeAction, res, field, proposed)
	}
	if want.Sign() <= 0 {
		return "", "", fmt.Errorf("%w: proposed %s %s must be positive", ErrNoSafeAction, res, field)
	}

	// Absolute ceiling.
	if !cap.IsZero() && want.Cmp(cap) > 0 {
		return "", "", fmt.Errorf("%w: proposed %s %s %s exceeds the cap %s", ErrNoSafeAction, res, field, want.String(), cap.String())
	}

	curDesc := "unset"
	if current != "" {
		have, err := resource.ParseQuantity(current)
		if err == nil {
			curDesc = have.String()
			// Never shrink: lowering a limit under load risks a worse failure
			// than the one being fixed.
			if want.Cmp(have) < 0 {
				return "", "", fmt.Errorf("%w: proposed %s %s %s is below the current %s (refusing to shrink)", ErrNoSafeAction, res, field, want.String(), have.String())
			}
			// Bounded growth relative to the current value.
			if multiplier > 0 && !have.IsZero() {
				ceiling := scaleQuantity(have, multiplier)
				if want.Cmp(ceiling) > 0 {
					return "", "", fmt.Errorf("%w: proposed %s %s %s is more than %.1fx the current %s", ErrNoSafeAction, res, field, want.String(), multiplier, have.String())
				}
			}
		}
	}

	return want.String(), fmt.Sprintf("%s %s %s→%s", res, field, curDesc, want.String()), nil
}

// scaleQuantity multiplies a quantity, working in milli-units so CPU quantities
// like "250m" scale correctly.
func scaleQuantity(q resource.Quantity, factor float64) resource.Quantity {
	scaled := int64(float64(q.MilliValue()) * factor)
	return *resource.NewMilliQuantity(scaled, q.Format)
}

// schedulable rejects a request raise that would leave the pod unable to be
// placed.
//
// With a cluster capacity snapshot this is a real headroom check: the pod's
// *projected total* request — every container, plus the raise — must fit in the
// remaining space on some schedulable node, after the reserve. Raising a
// request is the one resource edit that consumes cluster capacity, so it is
// bounded by the same arithmetic as a scale-up.
//
// Without a snapshot it falls back to the weaker single-node allocatable check:
// catches the outright-impossible case (a request larger than any whole node)
// and leaves the rest to the scheduler. The fallback is deliberate rather than
// a refusal — unlike scaling, request patching is pre-existing behaviour, and a
// cluster whose node reads are denied should keep the guard it had rather than
// silently lose the feature.
func schedulable(b *k8s.Bundle, cur *k8s.ContainerSummary, requests map[string]string) error {
	if b.Capacity != nil {
		projected := projectedPodRequests(b, cur, requests)
		if err := b.Capacity.Fits(projected); err != nil {
			return fmt.Errorf("%w: raising requests would take the pod to %s, and %s", ErrNoSafeAction, projected, err)
		}
		return nil
	}
	return fitsOnNode(b, requests)
}

// projectedPodRequests is what the pod would reserve after this container's
// request change: the pod's current total, plus the delta on each changed
// resource.
func projectedPodRequests(b *k8s.Bundle, cur *k8s.ContainerSummary, requests map[string]string) capacity.Requests {
	out := b.PodRequests
	if v, ok := requests["cpu"]; ok {
		out.CPUMilli += milliValue(v) - milliValue(cur.Requests["cpu"])
	}
	if v, ok := requests["memory"]; ok {
		out.MemBytes += byteValue(v) - byteValue(cur.Requests["memory"])
	}
	return out
}

func milliValue(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func byteValue(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

// fitsOnNode is the pre-capacity-snapshot check, kept as the fallback when node
// reads are unavailable: a single container request may not exceed the whole
// node's allocatable. When no node evidence is present it does not block, since
// impossibility cannot be proven.
func fitsOnNode(b *k8s.Bundle, requests map[string]string) error {
	if b.Node == nil {
		return nil
	}
	for _, res := range []string{"memory", "cpu"} {
		value, ok := requests[res]
		if !ok {
			continue
		}
		allocStr, ok := b.Node.Allocatable[res]
		if !ok {
			continue
		}
		alloc, err := resource.ParseQuantity(allocStr)
		if err != nil {
			continue
		}
		want, err := resource.ParseQuantity(value) // already validated upstream
		if err != nil {
			continue
		}
		if want.Cmp(alloc) > 0 {
			return fmt.Errorf("%w: proposed %s request %s exceeds node %s allocatable %s — the pod would be unschedulable",
				ErrNoSafeAction, res, want.String(), b.Node.Name, alloc.String())
		}
	}
	return nil
}

func findContainer(b *k8s.Bundle, name string) *k8s.ContainerSummary {
	for i := range b.Pod.Containers {
		if b.Pod.Containers[i].Name == name {
			return &b.Pod.Containers[i]
		}
	}
	return nil
}

// confidenceAtLeast reports whether have meets the want threshold on the
// low<medium<high scale. An unset threshold permits any confidence.
func confidenceAtLeast(have, want string) bool {
	rank := map[string]int{"low": 1, "medium": 2, "high": 3}
	if want == "" {
		return true
	}
	return rank[strings.ToLower(have)] >= rank[strings.ToLower(want)]
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
