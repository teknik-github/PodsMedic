// Package live carries what is happening in the cluster right now to anyone
// watching it.
//
// It exists for the visualisation, and its shape follows from that: a bounded
// ring of recent events plus a fan-out to subscribers. There is no history here
// and there is not meant to be — podsmedic stores almost nothing on purpose, and
// a live view wants the last few minutes, not the last few weeks.
//
// Two families of event flow through it, and keeping them distinguishable is the
// whole point: what the *cluster* did (a pod crashed, restarted, recovered) and
// what *podsmedic* did about it (diagnosed, healed, verified, rolled back).
// Watching a red event appear and a green one answer it is the story the display
// is there to tell.
package live

import (
	"sync"
	"time"
)

// Class is what kind of thing happened. The viewer colours by it, so the set is
// deliberately small — a legend nobody can hold in their head is no legend.
type Class string

const (
	// Cluster-side: something happened to a workload.
	ClassProblem  Class = "problem"  // a pod started failing
	ClassRestart  Class = "restart"  // a container restarted
	ClassRecovery Class = "recovery" // a pod became healthy again
	ClassGone     Class = "gone"     // a pod disappeared

	// podsmedic-side: something was done about it.
	ClassDiagnose Class = "diagnose" // evidence collected, model asked
	ClassHeal     Class = "heal"     // a change applied (or dry-run)
	ClassVerify   Class = "verify"   // a heal was confirmed to hold
	ClassRollback Class = "rollback" // a heal did not hold and was undone
	ClassDeclined Class = "declined" // a proposal was refused by the validator

	// ClassSweep is the agent's heartbeat, published once per sweep. It is not a
	// wire and never reaches the ring buffer — it exists so a healthy cluster
	// still shows signs of life. Without it the view is completely static when
	// nothing is wrong, and "everything is fine" looks identical to "the feed
	// died".
	ClassSweep Class = "sweep"
)

// FromPodsmedic reports whether this class describes podsmedic acting, rather
// than the cluster changing under it.
func (c Class) FromPodsmedic() bool {
	switch c {
	case ClassDiagnose, ClassHeal, ClassVerify, ClassRollback, ClassDeclined:
		return true
	}
	return false
}

// Event is one thing that happened, flattened for a browser to render.
type Event struct {
	At        time.Time `json:"at"`
	Class     Class     `json:"class"`
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod,omitempty"`
	Container string    `json:"container,omitempty"`
	// Workload is the owning controller ("Deployment/web") when known. The view
	// groups by it, so pods coming and going do not scatter the display.
	Workload string `json:"workload,omitempty"`
	Node     string `json:"node,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Key identifies the thing an event is about, for grouping in the view.
func (e Event) Key() string {
	if e.Workload != "" {
		return e.Namespace + "/" + e.Workload
	}
	return e.Namespace + "/" + e.Pod
}

// DefaultCapacity is how many recent events a stream keeps for a newly-attached
// viewer. Enough to show the last several minutes of a busy cluster without
// letting an idle process grow.
const DefaultCapacity = 200

// Stream is a bounded ring of recent events with fan-out to live subscribers.
//
// Publishing never blocks and never fails. That is the load-bearing property:
// this is called from the sweep and from an informer callback, and a browser tab
// left open on a laptop that went to sleep must not be able to stall either. A
// subscriber that cannot keep up loses events, which for a live view is the
// right trade — it wants what is happening now, not a backlog.
type Stream struct {
	mu       sync.Mutex
	ring     []Event
	capacity int
	subs     map[int]chan Event
	nextID   int
	dropped  uint64
}

// NewStream builds a stream retaining the given number of recent events.
func NewStream(capacity int) *Stream {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Stream{
		capacity: capacity,
		ring:     make([]Event, 0, capacity),
		subs:     map[int]chan Event{},
	}
}

// Publish records an event and delivers it to every attached subscriber.
func (s *Stream) Publish(e Event) {
	if s == nil {
		return // the UI is off; callers should not have to check
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.ring) == s.capacity {
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = e
	} else {
		s.ring = append(s.ring, e)
	}

	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
			s.dropped++ // slow subscriber; see the type comment
		}
	}
}

// PublishEphemeral fans an event out to live viewers without retaining it.
//
// The heartbeat uses this. Keeping one sweep per minute in the ring would evict
// the events worth looking at within a few hours, and replaying a backlog of
// heartbeats to a page that just loaded means nothing.
func (s *Stream) PublishEphemeral(e Event) {
	if s == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
			s.dropped++
		}
	}
}

// Subscribe attaches a viewer. The returned cancel func must be called when the
// viewer goes away, or the channel leaks.
func (s *Stream) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextID
	s.nextID++
	// Buffered so a brief render stall does not immediately cost events.
	ch := make(chan Event, 64)
	s.subs[id] = ch

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

// Recent returns the retained events, oldest first, so a viewer that just
// attached sees the last few minutes rather than a blank screen.
func (s *Stream) Recent() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.ring))
	copy(out, s.ring)
	return out
}

// Subscribers is the current viewer count, for metrics.
func (s *Stream) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// Dropped counts events a slow subscriber missed, for metrics.
func (s *Stream) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Workload is one controller as the view draws it: a single node in the ring
// around the globe, however many pods it currently has.
type Workload struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Pods      int    `json:"pods"`
	Ready     int    `json:"ready"`
	// Done counts pods that finished successfully. A Succeeded pod is never
	// Ready — that condition only applies while a pod is running — so counting
	// one as "not ready" paints a Job that completed weeks ago as a degraded
	// workload forever. Kept separate rather than folded into Ready because
	// "finished" and "serving" are different states and the detail panel says so.
	Done     int `json:"done,omitempty"`
	Problems int `json:"problems"`
}

// Snapshot is the whole picture a freshly-loaded page needs before the live
// feed starts making sense: what exists, and what just happened.
type Snapshot struct {
	At          time.Time `json:"at"`
	Nodes       int       `json:"nodes"`
	Pods        int       `json:"pods"`
	Problems    int       `json:"problems"`
	Incidents   int       `json:"incidents"`
	Healing     bool      `json:"healing"`
	SweepAgeSec int       `json:"sweepAgeSec"`
	// IntervalSec is the sweep period, so the view can show progress toward the
	// next one rather than only reacting once it lands.
	IntervalSec int        `json:"intervalSec"`
	Workloads   []Workload `json:"workloads"`
	Events      []Event    `json:"events"`
}

// Source supplies the snapshot. The agent implements it, since it holds the
// sweep results the view describes.
type Source interface {
	LiveSnapshot() Snapshot
}

// Suppressor drops an event that says the same thing as one it just saw.
//
// It exists because of the heal-retry loop: an incident that could not be healed
// is re-attempted every sweep, and re-declined every sweep, for the same reason.
// Left alone that is one event per workload per minute forever, which fills the
// ring and pushes out everything worth looking at. Watching a live cluster for a
// few minutes made that obvious in a way the unit tests could not.
//
// It is applied only where repetition is structural — a repeated *decline* is
// the retry loop echoing, whereas a repeated restart is a crash loop and is the
// most important thing on the screen.
type Suppressor struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]time.Time
}

// NewSuppressor builds a suppressor that lets an identical event through at most
// once per window.
func NewSuppressor(window time.Duration) *Suppressor {
	return &Suppressor{window: window, seen: map[string]time.Time{}}
}

// Allow reports whether this event is worth publishing, recording it if so.
func (s *Suppressor) Allow(e Event, now time.Time) bool {
	if s == nil || s.window <= 0 {
		return true
	}
	key := string(e.Class) + "|" + e.Key() + "|" + e.Reason + "|" + e.Detail

	s.mu.Lock()
	defer s.mu.Unlock()

	if last, ok := s.seen[key]; ok && now.Sub(last) < s.window {
		return false
	}
	// Expire while we are here, so a long-running agent does not accumulate a
	// key per workload that ever hiccuped.
	for k, t := range s.seen {
		if now.Sub(t) > s.window {
			delete(s.seen, k)
		}
	}
	s.seen[key] = now
	return true
}
