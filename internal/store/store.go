// Package store holds the rendered iCalendar feed in memory, guarded for
// concurrent access by the poller (writer) and HTTP handlers (readers).
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Snapshot is an immutable view of the current feed.
type Snapshot struct {
	Body         []byte
	ETag         string
	LastModified time.Time
	OK           bool // false until the first successful fetch
}

// Store is a concurrency-safe holder for the latest rendered feed.
type Store struct {
	mu   sync.RWMutex
	snap Snapshot
}

// New returns an empty store. Until Set is called, Get reports OK == false.
func New() *Store {
	return &Store{}
}

// Set replaces the current snapshot with the given body, computing a strong
// validator (ETag) and stamping the modification time.
func (s *Store) Set(body []byte, modTime time.Time) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = Snapshot{
		Body:         body,
		ETag:         etag,
		LastModified: modTime,
		OK:           true,
	}
}

// Get returns the current snapshot. The returned Body must not be mutated.
func (s *Store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}
