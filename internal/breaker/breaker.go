// Package breaker is a per-workload circuit breaker for auto-heal. A workload
// that keeps failing its heals — repeated rollbacks, or the same fix applied
// over and over — is one podsmedic cannot actually fix; continuing to patch it
// just churns the cluster and buries the real problem. After too many such
// events in a window the breaker trips "open": healing for that one workload is
// suspended for a cooldown and a human is told to look. Everything is keyed by
// the controller, so it survives pod churn, and the logic is pure so the policy
// is unit-testable without a cluster.
package breaker

import (
	"sync"
	"time"
)

// Options configures the trip thresholds. A non-positive Max disables that
// signal; a non-positive Window or OpenFor is treated as "never prune" / "never
// reopen" respectively by the caller supplying sane values.
type Options struct {
	// Window is the sliding span over which heals and rollbacks are counted.
	Window time.Duration
	// MaxHeals trips the breaker once this many heals land within Window. Guards
	// against a workload that recovers, relapses, and is re-healed endlessly.
	MaxHeals int
	// MaxRollbacks trips once this many heals are rolled back within Window — the
	// strongest signal a fix does not hold.
	MaxRollbacks int
	// OpenFor is how long healing stays suspended after a trip.
	OpenFor time.Duration
}

// Breaker tracks heal/rollback history per controller key and decides whether
// healing is currently permitted.
type Breaker struct {
	mu   sync.Mutex
	opts Options
	st   map[string]*state
}

type state struct {
	heals     []time.Time
	rollbacks []time.Time
	openUntil time.Time // zero = closed
}

// New builds a breaker with the given thresholds.
func New(o Options) *Breaker {
	return &Breaker{opts: o, st: map[string]*state{}}
}

// Allowed reports whether a heal may proceed for the workload. A breaker whose
// open window has elapsed closes and forgets its history, giving the workload a
// clean slate.
func (b *Breaker) Allowed(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.st[key]
	if s == nil || s.openUntil.IsZero() {
		return true
	}
	if now.Before(s.openUntil) {
		return false
	}
	// Open window elapsed: reset and allow again.
	delete(b.st, key)
	return true
}

// OnHeal records an applied heal and reports whether that tripped the breaker
// (a closed→open transition), so the caller can alert exactly once.
func (b *Breaker) OnHeal(key string, now time.Time) bool {
	return b.record(key, now, false)
}

// OnRollback records a rollback and reports whether that tripped the breaker.
func (b *Breaker) OnRollback(key string, now time.Time) bool {
	return b.record(key, now, true)
}

func (b *Breaker) record(key string, now time.Time, rollback bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := b.st[key]
	if s == nil {
		s = &state{}
		b.st[key] = s
	}
	switch {
	case s.openUntil.IsZero():
		// closed: carry on
	case now.Before(s.openUntil):
		// already open — record nothing new, it is already tripped
		return false
	default:
		// open window elapsed: start fresh before recording this event
		*s = state{}
	}

	if rollback {
		s.rollbacks = append(s.rollbacks, now)
	} else {
		s.heals = append(s.heals, now)
	}
	prune(s, now.Add(-b.opts.Window))

	tripped := (b.opts.MaxRollbacks > 0 && len(s.rollbacks) >= b.opts.MaxRollbacks) ||
		(b.opts.MaxHeals > 0 && len(s.heals) >= b.opts.MaxHeals)
	if tripped {
		s.openUntil = now.Add(b.opts.OpenFor)
		return true
	}
	return false
}

// prune drops events at or before cutoff.
func prune(s *state, cutoff time.Time) {
	s.heals = after(s.heals, cutoff)
	s.rollbacks = after(s.rollbacks, cutoff)
}

func after(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// IsOpen reports whether the breaker is currently tripped for the workload.
func (b *Breaker) IsOpen(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.st[key]
	return s != nil && !s.openUntil.IsZero() && now.Before(s.openUntil)
}

// OpenCount is the number of currently tripped workloads, for metrics.
func (b *Breaker) OpenCount(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.st {
		if !s.openUntil.IsZero() && now.Before(s.openUntil) {
			n++
		}
	}
	return n
}
