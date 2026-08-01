// Package audit keeps a durable, append-only trail of every heal lifecycle
// event — applied, dry-run, verified, rolled back — so a change made to the
// cluster can be reviewed after the fact: who/what/when, and the exact
// before/after values. The trail survives a restart of the podsmedic pod and is
// bounded in size, dropping the oldest events once a cap is reached.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Event is one entry in the audit trail. Old/New carry the changed values,
// keyed by a short field name ("limit.memory", "image", "initialDelaySeconds"),
// so a reviewer sees precisely what moved. They are empty for a restart, which
// changes no values.
type Event struct {
	Time       time.Time         `json:"time"`
	Namespace  string            `json:"namespace"`
	Controller string            `json:"controller"` // "Kind/Name"
	Container  string            `json:"container"`
	Action     string            `json:"action"`  // patch_resources|patch_image|patch_probe|restart_workload
	Outcome    string            `json:"outcome"` // applied|dryrun|verified|rolledback|rollback_failed
	Old        map[string]string `json:"old,omitempty"`
	New        map[string]string `json:"new,omitempty"`
	Summary    string            `json:"summary,omitempty"`
}

// Log is a durable append-only trail. Append never blocks a heal: a caller
// treats its error as non-fatal.
type Log interface {
	Append(ctx context.Context, e Event) error
	List(ctx context.Context) ([]Event, error)
}

// ConfigMapAPI is the minimal ConfigMap access the log needs. k8s.Client
// implements it; keeping the dependency this way leaves k8s free of any audit
// import.
type ConfigMapAPI interface {
	// GetConfigMap returns the data map, or (nil, nil) when the ConfigMap does
	// not exist yet.
	GetConfigMap(ctx context.Context, namespace, name string) (map[string]string, error)
	PutConfigMap(ctx context.Context, namespace, name string, data map[string]string) error
}

// configMapKey is the single data key under which the whole event list is stored
// as a JSON array — one read-modify-write keeps it consistent, and one key
// sidesteps ConfigMap key-charset limits.
const configMapKey = "events.json"

// DefaultMaxEvents bounds the trail so it stays well under the ConfigMap 1 MiB
// limit (an event is a few hundred bytes).
const DefaultMaxEvents = 500

// ConfigMapLog persists the trail in a ConfigMap in podsmedic's own namespace. A
// single replica serialises access, so plain read-modify-write is safe. When the
// list exceeds maxEvents the oldest entries are dropped.
type ConfigMapLog struct {
	api       ConfigMapAPI
	namespace string
	name      string
	maxEvents int
}

// NewConfigMapLog builds a log backed by the named ConfigMap. A non-positive
// maxEvents falls back to DefaultMaxEvents.
func NewConfigMapLog(api ConfigMapAPI, namespace, name string, maxEvents int) *ConfigMapLog {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	return &ConfigMapLog{api: api, namespace: namespace, name: name, maxEvents: maxEvents}
}

func (l *ConfigMapLog) load(ctx context.Context) ([]Event, error) {
	data, err := l.api.GetConfigMap(ctx, l.namespace, l.name)
	if err != nil {
		return nil, err
	}
	blob, ok := data[configMapKey]
	if !ok || blob == "" {
		return nil, nil
	}
	var events []Event
	if err := json.Unmarshal([]byte(blob), &events); err != nil {
		return nil, fmt.Errorf("decode audit trail: %w", err)
	}
	return events, nil
}

func (l *ConfigMapLog) Append(ctx context.Context, e Event) error {
	events, err := l.load(ctx)
	if err != nil {
		return err
	}
	events = append(events, e)
	if len(events) > l.maxEvents {
		// Drop the oldest, keeping the most recent maxEvents.
		events = events[len(events)-l.maxEvents:]
	}
	blob, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("encode audit trail: %w", err)
	}
	return l.api.PutConfigMap(ctx, l.namespace, l.name, map[string]string{configMapKey: string(blob)})
}

func (l *ConfigMapLog) List(ctx context.Context) ([]Event, error) {
	return l.load(ctx)
}

// NopLog is a Log that discards everything, used when the audit trail is
// disabled so callers need no nil checks.
type NopLog struct{}

func (NopLog) Append(context.Context, Event) error   { return nil }
func (NopLog) List(context.Context) ([]Event, error) { return nil, nil }

// MemLog is an in-memory Log for tests. It applies the same cap as ConfigMapLog.
type MemLog struct {
	mu        sync.Mutex
	events    []Event
	maxEvents int
}

// NewMemLog builds an empty in-memory log.
func NewMemLog(maxEvents int) *MemLog {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	return &MemLog{maxEvents: maxEvents}
}

func (m *MemLog) Append(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	if len(m.events) > m.maxEvents {
		m.events = m.events[len(m.events)-m.maxEvents:]
	}
	return nil
}

func (m *MemLog) List(_ context.Context) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out, nil
}
