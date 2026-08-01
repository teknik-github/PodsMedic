package breaker

import "fmt"

// This file is the whole-cluster counterpart to the per-workload breaker in
// breaker.go. It exists because every other limit podsmedic has is scoped to a
// single workload — the heal cooldown, the circuit breaker, the playbook — so
// fifty distinct workloads failing at once pass all of them and would be patched
// in one sweep.
//
// It bounds two different things:
//
//   - Rate. Even when every heal is individually correct, changing twenty
//     controllers a minute is churn no operator can follow.
//   - Cause. When a large share of the cluster fails simultaneously, the cause
//     is almost never the workloads: a node went away, storage stalled, a
//     registry became unreachable. Raising memory limits in response is noise at
//     best, and at worst it rolls every affected Deployment while the cluster is
//     already struggling.

// ClusterOptions bounds cluster-wide healing for one sweep.
type ClusterOptions struct {
	// MaxPerSweep caps how many heals may execute in a single sweep. Zero means
	// unlimited, which is the pre-existing behaviour and not recommended.
	MaxPerSweep int
	// SurgeRatio is the share of workloads that may be failing before healing is
	// suspended as systemic. Zero disables the check.
	SurgeRatio float64
	// SurgeMinWorkloads is the smallest cluster the ratio is applied to. On a
	// four-workload cluster one failure is 25% and means nothing, so the ratio
	// needs a floor to be meaningful.
	SurgeMinWorkloads int
}

// Surge reports whether this sweep's failures look systemic rather than
// per-workload, in which case healing should stand down and a human should look
// at the infrastructure.
//
// Pure: the decision is arithmetic over two counts, so the policy is testable
// without a cluster.
func Surge(failing, total int, opts ClusterOptions) (bool, string) {
	if opts.SurgeRatio <= 0 || total <= 0 {
		return false, ""
	}
	if total < opts.SurgeMinWorkloads {
		// Too few workloads for a ratio to say anything: one failure out of three
		// is 33% and entirely normal.
		return false, ""
	}
	ratio := float64(failing) / float64(total)
	if ratio < opts.SurgeRatio {
		return false, ""
	}
	return true, fmt.Sprintf(
		"%d of %d workloads are failing (%d%%, threshold %d%%) — that pattern is infrastructure, not the workloads, so healing is suspended for this sweep",
		failing, total, int(ratio*100), int(opts.SurgeRatio*100))
}

// Budget is a per-sweep allowance of heals.
//
// It is deliberately not a rate limiter with a window: the sweep is already the
// unit of pacing, and "at most N changes per sweep" is something an operator can
// reason about, where "N per rolling minute" is not.
type Budget struct {
	max  int
	used int
}

// NewBudget builds an allowance. A non-positive max means unlimited.
func NewBudget(max int) *Budget { return &Budget{max: max} }

// Take consumes one heal from the allowance, reporting whether there was room.
func (b *Budget) Take() bool {
	if b == nil || b.max <= 0 {
		return true
	}
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

// Used is how much of the allowance this sweep has spent.
func (b *Budget) Used() int {
	if b == nil {
		return 0
	}
	return b.used
}

// Max is the allowance, for the message when it runs out.
func (b *Budget) Max() int {
	if b == nil {
		return 0
	}
	return b.max
}
