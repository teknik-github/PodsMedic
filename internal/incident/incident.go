// Package incident correlates the many symptoms of one failing workload into a
// single incident, so a pod that is simultaneously OOMKilled, CrashLoopBackOff,
// and a restart storm produces one alert and one diagnosis — not three.
package incident

import (
	"sync"
	"time"

	"github.com/teknik-github/PodsMedic/internal/detect"
)

// Action tells the caller what to do with an observed problem.
type Action int

const (
	// New means a fresh incident was opened; run the full diagnose+alert path.
	New Action = iota
	// Update means an already-open incident gained a new symptom kind in a
	// later sweep; send a lightweight follow-up, no re-diagnosis.
	Update
	// Suppress means the problem belongs to an open incident and needs no
	// notification (a repeat, or a same-sweep extra kind folded into New).
	Suppress
)

// Incident is one correlated failure of a container, spanning symptom kinds and
// in-place restarts. Keyed by namespace/pod/container: a crash-looping pod keeps
// its name across restarts, so this naturally spans them.
type Incident struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Trigger is the first problem seen, used to diagnose the incident.
	Trigger detect.Problem `json:"trigger"`
	// Kinds is every symptom kind seen, in first-seen order.
	Kinds     []detect.Kind `json:"kinds"`
	kindSet   map[detect.Kind]bool
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	Count     int       `json:"count"`

	// Heal-retry state. The agent stores its validated proposal here (as opaque
	// JSON, so this package stays free of the heal package) so a later sweep — or
	// a restart, once the incident is persisted — can re-attempt the heal without
	// a fresh diagnosis. Healed is set once a real patch lands, stopping retries.
	HealActionJSON string `json:"healAction,omitempty"`
	HealConfidence string `json:"healConfidence,omitempty"`
	Healed         bool   `json:"healed,omitempty"`
}

// OtherKinds returns the symptom kinds beyond the triggering one, for an alert
// that wants to say "OOMKilled (also: CrashLoopBackOff, RestartStorm)".
func (i *Incident) OtherKinds() []string {
	var out []string
	for _, k := range i.Kinds {
		if k != i.Trigger.Kind {
			out = append(out, string(k))
		}
	}
	return out
}

// Store holds open incidents in memory. It can be snapshotted to and restored
// from durable storage by the caller (see Snapshot/Restore/Dirty), so a restart
// need not re-alert every open incident nor forget a pending heal proposal.
type Store struct {
	mu           sync.Mutex
	open         map[string]*Incident
	resolveAfter time.Duration
	dirty        bool // set on any change the caller should persist
}

// NewStore builds a store. An incident is considered resolved once it has gone
// unseen for resolveAfter.
func NewStore(resolveAfter time.Duration) *Store {
	return &Store{open: map[string]*Incident{}, resolveAfter: resolveAfter}
}

func key(p detect.Problem) string {
	return p.Namespace + "/" + p.Pod + "/" + p.Container
}

// Observe records one detected problem against its incident and reports what to
// do. A brand-new kind arriving in the same sweep that opened the incident is
// folded into that New alert (returns Suppress), so the single alert can list
// every current symptom; a new kind in a later sweep returns Update.
func (s *Store) Observe(p detect.Problem, now time.Time) (*Incident, Action) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dirty = true // LastSeen/Count/kinds always move on an observation

	k := key(p)
	inc, ok := s.open[k]
	if !ok {
		inc = &Incident{
			Key:       k,
			Namespace: p.Namespace,
			Pod:       p.Pod,
			Container: p.Container,
			Trigger:   p,
			kindSet:   map[detect.Kind]bool{},
			FirstSeen: now,
			LastSeen:  now,
			Count:     1,
		}
		inc.addKind(p.Kind)
		s.open[k] = inc
		return inc, New
	}

	inc.LastSeen = now
	inc.Count++
	if inc.kindSet[p.Kind] {
		return inc, Suppress
	}
	inc.addKind(p.Kind)
	if inc.FirstSeen.Equal(now) {
		// Opened this very sweep — the New alert will list this kind too.
		return inc, Suppress
	}
	return inc, Update
}

func (i *Incident) addKind(k detect.Kind) {
	if !i.kindSet[k] {
		i.kindSet[k] = true
		i.Kinds = append(i.Kinds, k)
	}
}

// Reap removes and returns incidents unseen for longer than resolveAfter, so the
// caller can send a resolution notice.
func (s *Store) Reap(now time.Time) []*Incident {
	s.mu.Lock()
	defer s.mu.Unlock()

	var resolved []*Incident
	for k, inc := range s.open {
		if now.Sub(inc.LastSeen) > s.resolveAfter {
			resolved = append(resolved, inc)
			delete(s.open, k)
			s.dirty = true
		}
	}
	return resolved
}

// OpenCount is the number of currently open incidents, for metrics.
func (s *Store) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// SetHealProposal records the agent's validated heal proposal for an open
// incident, so a later sweep or a restart can retry it without re-diagnosing.
func (s *Store) SetHealProposal(key, actionJSON, confidence string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc := s.open[key]; inc != nil {
		inc.HealActionJSON = actionJSON
		inc.HealConfidence = confidence
		s.dirty = true
	}
}

// MarkHealed flags an incident's proposal as applied, stopping further retries.
func (s *Store) MarkHealed(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc := s.open[key]; inc != nil && !inc.Healed {
		inc.Healed = true
		s.dirty = true
	}
}

// HealProposal returns an incident's stored proposal. ok is false when the
// incident is gone; healed reports whether a patch already landed.
func (s *Store) HealProposal(key string) (actionJSON, confidence string, healed, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc := s.open[key]
	if inc == nil {
		return "", "", false, false
	}
	return inc.HealActionJSON, inc.HealConfidence, inc.Healed, true
}

// Dirty reports whether the store changed since the last ClearDirty, so the
// caller can skip a needless persist write.
func (s *Store) Dirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// ClearDirty marks the current state as persisted.
func (s *Store) ClearDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = false
}

// Snapshot returns a copy of every open incident, for persistence.
func (s *Store) Snapshot() []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Incident, 0, len(s.open))
	for _, inc := range s.open {
		out = append(out, *inc)
	}
	return out
}

// Restore loads incidents from a snapshot, replacing any current state. The
// derived kind set is rebuilt from the persisted Kinds slice. It does not mark
// the store dirty: the restored state already matches what is persisted.
func (s *Store) Restore(list []Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = make(map[string]*Incident, len(list))
	for i := range list {
		inc := list[i]
		inc.kindSet = make(map[detect.Kind]bool, len(inc.Kinds))
		for _, k := range inc.Kinds {
			inc.kindSet[k] = true
		}
		s.open[inc.Key] = &inc
	}
}
