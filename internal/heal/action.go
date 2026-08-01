// Package heal validates and plans automated remediation for a diagnosed pod
// failure.
//
// The security model is: the LLM *proposes* an Action (untrusted — pod logs can
// carry prompt injection), and this package's Validate is the trust boundary
// that decides what, if anything, is safe to do. Validate never emits a
// destructive change: it only raises resource limits within hard caps, or
// requests a rollout restart. Everything else degrades to "alert only".
package heal

import "strings"

// ActionKind enumerates the remediations the LLM may propose. The set is
// deliberately small and non-destructive.
type ActionKind string

const (
	// ActionNone means no safe automated fix exists — alert a human.
	ActionNone ActionKind = "none"
	// ActionPatchResources raises a container's memory/CPU requests or limits.
	ActionPatchResources ActionKind = "patch_resources"
	// ActionRestartWorkload triggers a rollout restart of the owning controller.
	ActionRestartWorkload ActionKind = "restart_workload"
	// ActionPatchImage corrects a container image reference (e.g. a typo'd tag
	// causing ImagePullBackOff). Constrained to the same repository — only the
	// tag or digest may change.
	ActionPatchImage ActionKind = "patch_image"
	// ActionPatchProbe loosens a liveness/readiness probe (e.g. an
	// initialDelay too short to survive startup). Only ever relaxes timing —
	// never tightens or disables a probe.
	ActionPatchProbe ActionKind = "patch_probe"
	// ActionScaleReplicas raises a workload's replica count to spread load (e.g.
	// sustained CPU pressure on every replica). Only ever scales *up*, within a
	// hard cap — never down, which would cut availability.
	ActionScaleReplicas ActionKind = "scale_replicas"
	// ActionCreatePVC creates a PersistentVolumeClaim a pod references but that
	// was never created — the "forgot to apply the PVC manifest" case.
	//
	// It is the only action that creates anything, and the only one that touches
	// storage. It is bounded to creation alone: podsmedic never modifies, resizes,
	// or deletes a claim, so the worst case is an unused volume rather than lost
	// data. Off by default.
	ActionCreatePVC ActionKind = "create_pvc"
)

// Action is the remediation the LLM proposes. It is untrusted input: every
// field is re-checked by Validate before anything touches the cluster. Empty
// resource fields mean "leave unchanged".
type Action struct {
	Kind          ActionKind `json:"kind"`
	Container     string     `json:"container"`
	MemoryLimit   string     `json:"memory_limit"`
	CPULimit      string     `json:"cpu_limit"`
	MemoryRequest string     `json:"memory_request"`
	CPURequest    string     `json:"cpu_request"`
	// Image is the corrected image reference for patch_image. It must be the
	// same repository as the current image — only the tag/digest may differ.
	Image string `json:"image"`
	// Probe fields for patch_probe. ProbeType is "liveness" or "readiness"; the
	// numeric fields are the proposed new values (0 = leave unchanged). The
	// validator only ever accepts increases.
	ProbeType                string `json:"probe_type"`
	ProbeInitialDelaySeconds int32  `json:"probe_initial_delay_seconds"`
	ProbePeriodSeconds       int32  `json:"probe_period_seconds"`
	ProbeTimeoutSeconds      int32  `json:"probe_timeout_seconds"`
	ProbeFailureThreshold    int32  `json:"probe_failure_threshold"`
	// Replicas is the proposed new replica count for scale_replicas. The
	// validator only ever accepts an increase, within a hard cap.
	Replicas int32  `json:"replicas"`
	Reason   string `json:"reason"`
}

// Describe renders an action for a human, for the chat and playbook listings.
//
// It describes the *proposal*, not an applied change, so it deliberately omits
// scale_replicas' replica count: that number is re-derived from live load when
// the action runs, and printing the stale one would misrepresent what a replay
// would do.
func (a Action) Describe() string {
	var parts []string
	if a.Container != "" {
		parts = append(parts, "container "+a.Container)
	}
	for _, f := range []struct{ label, value string }{
		{"memory limit", a.MemoryLimit},
		{"cpu limit", a.CPULimit},
		{"memory request", a.MemoryRequest},
		{"cpu request", a.CPURequest},
		{"image", a.Image},
	} {
		if f.value != "" {
			parts = append(parts, f.label+" → "+f.value)
		}
	}
	if a.ProbeType != "" {
		parts = append(parts, "loosen "+a.ProbeType+" probe")
	}
	if a.Kind == ActionScaleReplicas {
		parts = append(parts, "replica count re-derived from load at replay")
	}
	if a.Kind == ActionCreatePVC {
		parts = append(parts, "size and class from configuration at replay")
	}

	if len(parts) == 0 {
		return string(a.Kind)
	}
	return string(a.Kind) + ": " + strings.Join(parts, ", ")
}

// Plan is a validated, bounded remediation ready to execute. It is produced
// only by Validate, so its presence means the change already passed every
// safety check.
type Plan struct {
	Kind      ActionKind
	Namespace string
	Pod       string
	Container string
	// Limits and Requests hold the final resource values to set, keyed by
	// "memory"/"cpu". Only present for ActionPatchResources.
	Limits   map[string]string
	Requests map[string]string
	// Image is the validated replacement image. Only present for ActionPatchImage.
	Image string
	// ProbeType and Probe hold the validated probe loosening. Only present for
	// ActionPatchProbe. Probe is keyed by field name
	// ("initialDelaySeconds"/"periodSeconds"/"timeoutSeconds"/"failureThreshold").
	ProbeType string
	Probe     map[string]int32
	// Replicas is the validated new replica count. Only present for
	// ActionScaleReplicas.
	Replicas int32
	// Claim describes the PersistentVolumeClaim to create. Only present for
	// ActionCreatePVC. Every field is derived from configuration and the pod's
	// own spec — never from the model.
	Claim  *ClaimSpec
	Reason string
	// Summary is a human-readable description of exactly what will change,
	// for the alert and the audit log.
	Summary string
}

// ClaimSpec is the PersistentVolumeClaim ActionCreatePVC will create.
//
// Notably absent is any field the model can influence. Size and class come from
// configuration, the name and namespace from the pod's own spec, and the access
// mode is fixed. A provisioned volume costs money and, once bound, is awkward to
// resize and impossible to shrink — so unlike a memory limit, this is not a
// number worth taking from untrusted input at all.
type ClaimSpec struct {
	Namespace string
	Name      string
	Size      string
	// StorageClass empty means "use the cluster default", which is what omitting
	// storageClassName does.
	StorageClass string
	AccessMode   string
}

// WorkloadKey identifies the target for cooldown bookkeeping, so the same
// workload is not healed on every poll.
func (p Plan) WorkloadKey() string {
	return string(p.Kind) + ":" + p.Namespace + "/" + p.Pod + "/" + p.Container
}
