package heal

import (
	"time"

	"github.com/peceldev/podsmedic/internal/k8s"
)

// HealRecord is a persisted, applied patch-heal awaiting verification. It holds
// enough to (a) recognise the workload in a later sweep and (b) roll the change
// back to exactly the values that were in place before. Only real ApplyResources
// heals are recorded — a dry run changes nothing, and a restart has nothing to
// undo.
type HealRecord struct {
	// Controller identifies the patched workload, resolved at heal time so
	// verification never depends on the original (now-replaced) pod.
	ControllerKind string `json:"controllerKind"`
	ControllerName string `json:"controllerName"`
	Namespace      string `json:"namespace"`
	Container      string `json:"container"`
	Kind           string `json:"kind"` // the detected problem being fixed

	OldLimits   map[string]string `json:"oldLimits,omitempty"`
	OldRequests map[string]string `json:"oldRequests,omitempty"`
	NewLimits   map[string]string `json:"newLimits,omitempty"`
	NewRequests map[string]string `json:"newRequests,omitempty"`
	// OldImage/NewImage are set for an image heal, so a rollback can restore the
	// prior image reference.
	OldImage string `json:"oldImage,omitempty"`
	NewImage string `json:"newImage,omitempty"`
	// Probe fields are set for a probe heal. OldProbe holds the prior values of
	// exactly the fields that were changed, for rollback.
	ProbeType string           `json:"probeType,omitempty"`
	OldProbe  map[string]int32 `json:"oldProbe,omitempty"`
	NewProbe  map[string]int32 `json:"newProbe,omitempty"`
	// Replica fields are set for a scale heal, so a rollback can restore the
	// prior count.
	OldReplicas int32 `json:"oldReplicas,omitempty"`
	NewReplicas int32 `json:"newReplicas,omitempty"`

	AppliedAt   time.Time `json:"appliedAt"`
	VerifyAfter time.Time `json:"verifyAfter"`
	Summary     string    `json:"summary"`
	// ActionJSON is the raw validated action that produced this heal, carried so
	// that a heal which passes verification can be learned into the playbook and
	// replayed later without an LLM diagnosis. Opaque to verification itself.
	ActionJSON string `json:"actionJSON,omitempty"`
	// Confidence is the diagnosis confidence the heal was accepted at, kept so a
	// playbook replay can clear the same min-confidence gate.
	Confidence string `json:"confidence,omitempty"`
}

// Ref rebuilds the ControllerRef needed to patch the workload again (for a
// rollback).
func (r HealRecord) Ref() k8s.ControllerRef {
	return k8s.ControllerRef{Kind: r.ControllerKind, Name: r.ControllerName, Namespace: r.Namespace}
}

// ControllerKey is the verification join key: it identifies the workload
// independent of any single pod, so a heal recorded against one pod is matched
// to that pod's replacement in a later sweep.
func (r HealRecord) ControllerKey() string {
	return r.Namespace + "/" + r.ControllerKind + "/" + r.ControllerName
}

// ControllerKeyFor builds the same key from a resolved controller, so the agent
// can index the current sweep's problems by workload.
func ControllerKeyFor(ctrl k8s.ControllerRef) string {
	return ctrl.Namespace + "/" + ctrl.Kind + "/" + ctrl.Name
}

// Verdict is the outcome of checking a due heal against the cluster's current
// state.
type Verdict int

const (
	// VerdictPending means the verification window has not elapsed yet.
	VerdictPending Verdict = iota
	// VerdictHealthy means the workload recovered; the record can be retired.
	VerdictHealthy
	// VerdictRollback means the workload is still failing; undo the change.
	VerdictRollback
)

// VerifyVerdict is the pure decision at the heart of verification: given a
// record, the current time, and whether the workload still has an open problem,
// decide what to do. Keeping it pure is what makes the verify/rollback policy
// unit-testable without a cluster.
func VerifyVerdict(rec HealRecord, now time.Time, stillFailing bool) Verdict {
	if now.Before(rec.VerifyAfter) {
		return VerdictPending
	}
	if stillFailing {
		return VerdictRollback
	}
	return VerdictHealthy
}

// RecordFromPlan builds a verification record from an executed patch plan and
// the pre-change container state. before is the container as it was in the
// evidence bundle, so its Limits/Requests are the exact values to restore.
func RecordFromPlan(p *Plan, ctrl k8s.ControllerRef, before *k8s.ContainerSummary, now time.Time, verifyAfter time.Duration) HealRecord {
	rec := HealRecord{
		ControllerKind: ctrl.Kind,
		ControllerName: ctrl.Name,
		Namespace:      ctrl.Namespace,
		Container:      p.Container,
		NewLimits:      p.Limits,
		NewRequests:    p.Requests,
		AppliedAt:      now,
		VerifyAfter:    now.Add(verifyAfter),
		Summary:        p.Summary,
	}
	if before != nil {
		// Copy only the keys this plan actually changed, so a rollback restores
		// precisely those and leaves everything else untouched.
		rec.OldLimits = pick(before.Limits, p.Limits)
		rec.OldRequests = pick(before.Requests, p.Requests)
	}
	if p.Kind == ActionPatchImage {
		rec.NewImage = p.Image
		if before != nil {
			rec.OldImage = before.Image
		}
	}
	if p.Kind == ActionPatchProbe {
		rec.ProbeType = p.ProbeType
		rec.NewProbe = p.Probe
		if before != nil {
			rec.OldProbe = currentProbeFields(before.Probes[p.ProbeType], p.Probe)
		}
	}
	if p.Kind == ActionScaleReplicas {
		rec.NewReplicas = p.Replicas
		// OldReplicas is filled by the caller from the evidence bundle's replica
		// count, which RecordFromPlan does not carry.
	}
	return rec
}

// currentProbeFields reads the pre-heal values of exactly the fields a probe
// plan changes, so a rollback restores precisely those.
func currentProbeFields(cur *k8s.ProbeInfo, changed map[string]int32) map[string]int32 {
	if cur == nil || len(changed) == 0 {
		return nil
	}
	byName := map[string]int32{
		"initialDelaySeconds": cur.InitialDelaySeconds,
		"periodSeconds":       cur.PeriodSeconds,
		"timeoutSeconds":      cur.TimeoutSeconds,
		"failureThreshold":    cur.FailureThreshold,
	}
	out := map[string]int32{}
	for k := range changed {
		out[k] = byName[k]
	}
	return out
}

// pick returns the from-values for exactly the keys present in changed.
func pick(from, changed map[string]string) map[string]string {
	if len(changed) == 0 {
		return nil
	}
	out := make(map[string]string, len(changed))
	for k := range changed {
		if v, ok := from[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
