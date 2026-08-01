package heal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Store persists heal records between sweeps so verification survives a restart
// of the podsmedic pod. Without persistence a crash-restart would forget its
// pending heals and could re-heal a still-settling workload.
type Store interface {
	Save(ctx context.Context, rec HealRecord) error
	List(ctx context.Context) ([]HealRecord, error)
	// Delete removes the record for a controller key once it is verified or
	// rolled back.
	Delete(ctx context.Context, controllerKey string) error
}

// ConfigMapAPI is the minimal ConfigMap access the store needs. k8s.Client
// implements it; the direction keeps k8s free of any heal import.
type ConfigMapAPI interface {
	// GetConfigMap returns the data map, or (nil, nil) when the ConfigMap does
	// not exist yet.
	GetConfigMap(ctx context.Context, namespace, name string) (map[string]string, error)
	PutConfigMap(ctx context.Context, namespace, name string, data map[string]string) error
}

// configMapKey is the single data key under which the record set is stored as a
// JSON array. One key keeps the whole set consistent with one read-modify-write
// and sidesteps ConfigMap key-charset limits on workload names.
const configMapKey = "records.json"

// ConfigMapStore persists records in a ConfigMap in podsmedic's own namespace.
// A single replica serialises access, so plain read-modify-write is safe.
type ConfigMapStore struct {
	api       ConfigMapAPI
	namespace string
	name      string
}

// NewConfigMapStore builds a store backed by the named ConfigMap.
func NewConfigMapStore(api ConfigMapAPI, namespace, name string) *ConfigMapStore {
	return &ConfigMapStore{api: api, namespace: namespace, name: name}
}

func (s *ConfigMapStore) load(ctx context.Context) ([]HealRecord, error) {
	data, err := s.api.GetConfigMap(ctx, s.namespace, s.name)
	if err != nil {
		return nil, err
	}
	blob, ok := data[configMapKey]
	if !ok || blob == "" {
		return nil, nil
	}
	var recs []HealRecord
	if err := json.Unmarshal([]byte(blob), &recs); err != nil {
		return nil, fmt.Errorf("decode heal state: %w", err)
	}
	return recs, nil
}

func (s *ConfigMapStore) store(ctx context.Context, recs []HealRecord) error {
	blob, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("encode heal state: %w", err)
	}
	return s.api.PutConfigMap(ctx, s.namespace, s.name, map[string]string{configMapKey: string(blob)})
}

func (s *ConfigMapStore) Save(ctx context.Context, rec HealRecord) error {
	recs, err := s.load(ctx)
	if err != nil {
		return err
	}
	recs = upsert(recs, rec)
	return s.store(ctx, recs)
}

func (s *ConfigMapStore) List(ctx context.Context) ([]HealRecord, error) {
	return s.load(ctx)
}

func (s *ConfigMapStore) Delete(ctx context.Context, controllerKey string) error {
	recs, err := s.load(ctx)
	if err != nil {
		return err
	}
	recs = remove(recs, controllerKey)
	return s.store(ctx, recs)
}

// upsert replaces any existing record for the same controller key, so a
// re-heal of a workload supersedes the prior pending record rather than
// stacking a second one.
func upsert(recs []HealRecord, rec HealRecord) []HealRecord {
	out := remove(recs, rec.ControllerKey())
	return append(out, rec)
}

func remove(recs []HealRecord, controllerKey string) []HealRecord {
	out := recs[:0]
	for _, r := range recs {
		if r.ControllerKey() != controllerKey {
			out = append(out, r)
		}
	}
	return out
}

// MemStore is an in-memory Store for tests and for a run without persistence.
type MemStore struct {
	mu   sync.Mutex
	recs map[string]HealRecord
}

// NewMemStore builds an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{recs: map[string]HealRecord{}}
}

func (m *MemStore) Save(_ context.Context, rec HealRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.ControllerKey()] = rec
	return nil
}

func (m *MemStore) List(_ context.Context) ([]HealRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HealRecord, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemStore) Delete(_ context.Context, controllerKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, controllerKey)
	return nil
}
