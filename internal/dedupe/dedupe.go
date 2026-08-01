// Package dedupe suppresses repeat alerts for a problem that is already known.
package dedupe

import (
	"sync"
	"time"
)

// Cache remembers when a fingerprint was last alerted on.
type Cache struct {
	mu       sync.Mutex
	cooldown time.Duration
	seen     map[string]time.Time
}

// New returns a cache that suppresses a fingerprint for the given cooldown.
func New(cooldown time.Duration) *Cache {
	return &Cache{cooldown: cooldown, seen: make(map[string]time.Time)}
}

// ShouldAlert reports whether the fingerprint is outside its cooldown window,
// and records the alert time when it returns true.
func (c *Cache) ShouldAlert(fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if last, ok := c.seen[fingerprint]; ok && now.Sub(last) < c.cooldown {
		return false
	}
	c.seen[fingerprint] = now
	return true
}

// Sweep drops entries older than twice the cooldown so the map does not grow
// without bound in long-lived clusters.
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := time.Now().Add(-2 * c.cooldown)
	for k, v := range c.seen {
		if v.Before(cutoff) {
			delete(c.seen, k)
		}
	}
}
