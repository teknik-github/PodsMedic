// Package playbook is podsmedic's memory of fixes that worked. Every heal that
// is applied and then passes verification is recorded here, keyed by workload
// controller and problem kind. When the same workload hits the same problem
// again, the agent can replay the remembered fix directly — no LLM diagnosis,
// no cost, no latency — while still running it through the pure validator, the
// circuit breaker, and verification.
//
// # Forgetting is half the job
//
// A remedy that is only ever learned becomes a liability: the workload's root
// cause changes, and podsmedic keeps confidently replaying the fix for a
// problem that no longer exists. So the book forgets in three distinct ways,
// and the distinction matters:
//
//   - Fail: the remedy was replayed and rolled back. The entry goes at once —
//     nothing that stopped working stays replayable. A scar is kept, and once a
//     pair collects MaxFailures scars inside FailureDecay it is quarantined:
//     the model handles that workload afresh and nothing is learned back until
//     the quarantine lifts. Without this, a flapping remedy is re-learned and
//     re-rolled-back forever, and each cycle is a real patch to a real cluster.
//   - Retire: nothing failed, the remedy simply has not been confirmed in a
//     long time. It is dropped with no scar, so the next occurrence is
//     diagnosed by the model and, if the fix still works, learned straight
//     back.
//   - Evict: the book is full, so the least recently useful entry makes room.
//
// Entries store the remedy as opaque JSON (the raw validated action), so this
// package stays free of the heal package. The book is pure and unit-testable;
// the agent owns its persistence.
package playbook

import (
	"sort"
	"sync"
	"time"
)

// Entry is one remembered remedy.
type Entry struct {
	// Controller is the workload key "namespace/Kind/Name"; Kind is the detected
	// problem it fixes. Together they key the entry.
	Controller string `json:"controller"`
	Kind       string `json:"kind"`
	// ActionJSON is the raw heal action to replay (re-validated on use).
	ActionJSON string `json:"actionJSON"`
	Confidence string `json:"confidence"`
	// Hits counts replays served from this entry, for the cost-savings metric.
	Hits         int       `json:"hits"`
	Recorded     time.Time `json:"recorded"`
	LastHit      time.Time `json:"lastHit,omitempty"`
	LastVerified time.Time `json:"lastVerified"`
	// Verifications counts the times this remedy has been confirmed to hold,
	// including the one that first created it. Read with Scar.Failures it is the
	// remedy's track record, which is what /playbook reports.
	Verifications int `json:"verifications,omitempty"`
}

// Scar remembers that a remedy for this workload and problem stopped holding.
// It outlives the entry deliberately: the entry is what gets replayed, the scar
// is what stops the same broken remedy being learned over and over.
type Scar struct {
	Controller  string    `json:"controller"`
	Kind        string    `json:"kind"`
	Failures    int       `json:"failures"`
	LastFailure time.Time `json:"lastFailure"`
	// QuarantinedUntil, when in the future, blocks learning this pair at all.
	QuarantinedUntil time.Time `json:"quarantinedUntil,omitempty"`
}

// Quarantined reports whether learning is currently blocked for this pair.
func (s Scar) Quarantined(now time.Time) bool {
	return now.Before(s.QuarantinedUntil)
}

// State is everything the book persists.
type State struct {
	Entries []Entry `json:"entries"`
	Scars   []Scar  `json:"scars,omitempty"`
}

func key(controller, kind string) string { return controller + "|" + kind }

// Options tunes remembering and forgetting.
type Options struct {
	// MaxEntries bounds the book so its ConfigMap stays small.
	MaxEntries int
	// MaxFailures is how many rollbacks a workload+kind may collect inside
	// FailureDecay before it is quarantined instead of simply re-learned.
	MaxFailures int
	// QuarantineFor is how long a quarantined pair stays unlearnable. It doubles
	// with each further failure, up to maxQuarantine.
	QuarantineFor time.Duration
	// FailureDecay is how long a rollback counts against a pair. A workload that
	// failed twice last month starts clean today.
	FailureDecay time.Duration
	// MaxAge retires a remedy this long after its last confirmation. Zero
	// disables retirement, which means trusting a fix indefinitely.
	MaxAge time.Duration
}

// Defaults for the book.
const (
	DefaultMaxEntries    = 500
	DefaultMaxFailures   = 2
	DefaultQuarantineFor = 24 * time.Hour
	DefaultFailureDecay  = 7 * 24 * time.Hour
	DefaultMaxAge        = 30 * 24 * time.Hour
	// maxQuarantine caps the doubling, so a pathological workload is retried
	// occasionally rather than being written off forever.
	maxQuarantine = 14 * 24 * time.Hour
)

func (o Options) withDefaults() Options {
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = DefaultMaxFailures
	}
	if o.QuarantineFor <= 0 {
		o.QuarantineFor = DefaultQuarantineFor
	}
	if o.FailureDecay <= 0 {
		o.FailureDecay = DefaultFailureDecay
	}
	return o
}

// Book holds remembered remedies. maxEntries bounds it, evicting the least
// recently useful (oldest LastVerified) when full.
type Book struct {
	mu      sync.Mutex
	entries map[string]*Entry
	scars   map[string]*Scar
	opts    Options
	dirty   bool
}

// New builds an empty book. A zero Options field falls back to its default;
// MaxAge is the exception, where zero genuinely means "never retire".
func New(opts Options) *Book {
	return &Book{
		entries: map[string]*Entry{},
		scars:   map[string]*Scar{},
		opts:    opts.withDefaults(),
	}
}

// Lookup returns a copy of the remedy for a workload+kind, if one is remembered.
func (b *Book) Lookup(controller, kind string) (Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key(controller, kind)]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Record stores (or refreshes) a verified remedy. Called when a heal passes
// verification, so the book learns from both LLM-diagnosed and replayed fixes.
//
// It reports whether the remedy was actually learned: a quarantined pair is
// refused, which is the point of the quarantine. The heal itself already
// happened and held — this only declines to promise it will work next time.
func (b *Book) Record(controller, kind, actionJSON, confidence string, now time.Time) bool {
	if actionJSON == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	k := key(controller, kind)
	if s, ok := b.scars[k]; ok && s.Quarantined(now) {
		return false
	}
	if e, ok := b.entries[k]; ok {
		e.ActionJSON = actionJSON
		e.Confidence = confidence
		e.LastVerified = now
		e.Verifications++
		b.dirty = true
		return true
	}
	if len(b.entries) >= b.opts.MaxEntries {
		b.evictOldest()
	}
	b.entries[k] = &Entry{
		Controller:    controller,
		Kind:          kind,
		ActionJSON:    actionJSON,
		Confidence:    confidence,
		Recorded:      now,
		LastVerified:  now,
		Verifications: 1,
	}
	b.dirty = true
	return true
}

// Fail records that a remedy was replayed and did not hold.
//
// The entry goes immediately — a fix that stopped working must never be
// replayable — and a scar is kept so repeat offenders can be told apart from
// one-offs. Once a pair reaches MaxFailures inside FailureDecay it is
// quarantined and nothing is learned for it until that lifts.
//
// Returns whether an entry was removed, and whether the pair is now quarantined.
func (b *Book) Fail(controller, kind string, now time.Time) (removed, quarantined bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	k := key(controller, kind)
	if _, ok := b.entries[k]; ok {
		delete(b.entries, k)
		removed = true
	}

	s := b.scars[k]
	switch {
	case s == nil:
		if len(b.scars) >= b.opts.MaxEntries {
			b.forgetStalestScar(now)
		}
		s = &Scar{Controller: controller, Kind: kind}
		b.scars[k] = s
	case now.Sub(s.LastFailure) > b.opts.FailureDecay:
		// The previous trouble is old news; this is a fresh streak rather than a
		// continuing one.
		s.Failures = 0
	}
	s.Failures++
	s.LastFailure = now

	if s.Failures >= b.opts.MaxFailures {
		// Each further failure doubles the wait, so a workload that cannot be
		// fixed automatically stops consuming attempts at a fixed rate.
		wait := b.opts.QuarantineFor << (s.Failures - b.opts.MaxFailures)
		if wait > maxQuarantine || wait <= 0 {
			wait = maxQuarantine
		}
		s.QuarantinedUntil = now.Add(wait)
		quarantined = true
	}
	b.dirty = true
	return removed, quarantined
}

// Retire drops remedies whose last confirmation is older than MaxAge and
// returns them, so the caller can say what it forgot and why. A zero MaxAge
// retires nothing.
//
// This is not a failure path: the remedies retired here never stopped working,
// they merely stopped being proven. Dropping them costs one LLM diagnosis the
// next time the problem appears, and buys the guarantee that podsmedic is never
// replaying a fix nobody has confirmed this month.
func (b *Book) Retire(now time.Time) []Entry {
	if b.opts.MaxAge <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var retired []Entry
	for k, e := range b.entries {
		if now.Sub(e.LastVerified) > b.opts.MaxAge {
			retired = append(retired, *e)
			delete(b.entries, k)
		}
	}
	if len(retired) == 0 {
		return nil
	}
	// Also drop scars nothing is holding open any more, so the map does not
	// accumulate history for workloads that have long since gone.
	for k, s := range b.scars {
		if !s.Quarantined(now) && now.Sub(s.LastFailure) > b.opts.MaxAge {
			delete(b.scars, k)
		}
	}
	sort.Slice(retired, func(i, j int) bool {
		return retired[i].LastVerified.Before(retired[j].LastVerified)
	})
	b.dirty = true
	return retired
}

// Quarantined reports whether this pair is currently barred from learning, and
// until when. The agent uses it to explain, in an alert, why a fix that just
// worked was not remembered.
func (b *Book) Quarantined(controller, kind string, now time.Time) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.scars[key(controller, kind)]
	if !ok || !s.Quarantined(now) {
		return time.Time{}, false
	}
	return s.QuarantinedUntil, true
}

// QuarantineCount is the number of pairs currently barred from learning, for
// metrics.
func (b *Book) QuarantineCount(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.scars {
		if s.Quarantined(now) {
			n++
		}
	}
	return n
}

// Scars returns a copy of the failure history, newest trouble first.
func (b *Book) Scars() []Scar {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Scar, 0, len(b.scars))
	for _, s := range b.scars {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastFailure.After(out[j].LastFailure) })
	return out
}

// forgetStalestScar makes room by dropping the least recently troubled pair
// that is not currently quarantined. Caller holds the lock.
func (b *Book) forgetStalestScar(now time.Time) {
	var stalestKey string
	var stalest time.Time
	for k, s := range b.scars {
		if s.Quarantined(now) {
			continue
		}
		if stalestKey == "" || s.LastFailure.Before(stalest) {
			stalestKey, stalest = k, s.LastFailure
		}
	}
	if stalestKey != "" {
		delete(b.scars, stalestKey)
	}
}

// MarkHit records that an entry served a replay.
func (b *Book) MarkHit(controller, kind string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[key(controller, kind)]; ok {
		e.Hits++
		e.LastHit = now
		b.dirty = true
	}
}

// Evict forgets a remedy without holding it against the workload. Used when the
// stored remedy is unusable for a reason that is our fault rather than the
// workload's — an action we can no longer decode — where a scar would punish the
// wrong party. A remedy that was tried and failed goes through Fail instead.
func (b *Book) Evict(controller, kind string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := key(controller, kind)
	if _, ok := b.entries[k]; ok {
		delete(b.entries, k)
		b.dirty = true
		return true
	}
	return false
}

// evictOldest drops the entry with the oldest LastVerified. Caller holds the lock.
func (b *Book) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for k, e := range b.entries {
		if oldestKey == "" || e.LastVerified.Before(oldest) {
			oldestKey, oldest = k, e.LastVerified
		}
	}
	if oldestKey != "" {
		delete(b.entries, oldestKey)
	}
}

// Count is the number of remembered remedies, for metrics.
func (b *Book) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Dirty reports whether the book changed since the last ClearDirty.
func (b *Book) Dirty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dirty
}

// ClearDirty marks the current state as persisted.
func (b *Book) ClearDirty() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = false
}

// Snapshot returns a copy of every entry, for readers (the chat command and the
// study report). Persistence uses State, which also carries the scars.
func (b *Book) Snapshot() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, len(b.entries))
	for _, e := range b.entries {
		out = append(out, *e)
	}
	return out
}

// State returns everything worth persisting, including the failure history.
// Scars have to survive a restart or the quarantine is trivially defeated by
// the restart a flapping heal tends to cause anyway.
func (b *Book) State() State {
	return State{Entries: b.Snapshot(), Scars: b.Scars()}
}

// Restore loads a persisted state, replacing current state. It does not mark
// the book dirty: the restored state already matches what is persisted.
func (b *Book) Restore(s State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = make(map[string]*Entry, len(s.Entries))
	for i := range s.Entries {
		e := s.Entries[i]
		b.entries[key(e.Controller, e.Kind)] = &e
	}
	b.scars = make(map[string]*Scar, len(s.Scars))
	for i := range s.Scars {
		sc := s.Scars[i]
		b.scars[key(sc.Controller, sc.Kind)] = &sc
	}
}
