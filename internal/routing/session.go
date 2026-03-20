package routing

import (
	"hash/fnv"
	"sync"
	"time"
)

const (
	defaultSessionTTL = 2 * time.Hour
	defaultMaxEntries = 10000
	hashPrefixLen     = 200 // only hash first 200 bytes of first message
)

// SessionEntry records which model was used for a conversation.
type SessionEntry struct {
	Provider   string
	Model      string
	LastUsedAt time.Time
	TurnCount  int

	// Bridge lifecycle
	BridgeActive    bool
	BridgeTurnsLeft int
}

// SessionCache provides O(1) session affinity lookups.
// Key is FNV-1a hash of the first user message (first 200 bytes).
type SessionCache struct {
	mu    sync.RWMutex
	items map[uint64]*SessionEntry
	order []uint64 // LRU: most recent at end
	ttl   time.Duration
	max   int
}

// NewSessionCache creates a session cache with default settings.
func NewSessionCache() *SessionCache {
	return &SessionCache{
		items: make(map[uint64]*SessionEntry),
		ttl:   defaultSessionTTL,
		max:   defaultMaxEntries,
	}
}

// hashKey computes FNV-1a of the first N bytes of the first user message.
func hashKey(firstMsg string) uint64 {
	s := firstMsg
	if len(s) > hashPrefixLen {
		s = s[:hashPrefixLen]
	}
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// Get returns the session entry for a conversation, or nil if not found/expired.
func (c *SessionCache) Get(firstMsg string) *SessionEntry {
	key := hashKey(firstMsg)

	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()

	if !ok {
		return nil
	}

	if time.Since(entry.LastUsedAt) > c.ttl {
		// Expired — remove lazily
		c.mu.Lock()
		delete(c.items, key)
		c.removeLRU(key)
		c.mu.Unlock()
		return nil
	}

	return entry
}

// Set creates or updates a session entry. Also handles LRU eviction.
func (c *SessionCache) Set(firstMsg, provider, model string) {
	key := hashKey(firstMsg)

	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.items[key]; ok {
		entry.Provider = provider
		entry.Model = model
		entry.LastUsedAt = time.Now()
		entry.TurnCount++
		c.touchLRU(key)
		return
	}

	// Evict if at capacity
	for len(c.items) >= c.max {
		c.evictOldest()
	}

	c.items[key] = &SessionEntry{
		Provider:   provider,
		Model:      model,
		LastUsedAt: time.Now(),
		TurnCount:  1,
	}
	c.order = append(c.order, key)
}

// Len returns the number of entries.
func (c *SessionCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

func (c *SessionCache) touchLRU(key uint64) {
	c.removeLRU(key)
	c.order = append(c.order, key)
}

func (c *SessionCache) removeLRU(key uint64) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *SessionCache) evictOldest() {
	if len(c.order) == 0 {
		return
	}
	oldest := c.order[0]
	c.order = c.order[1:]
	delete(c.items, oldest)
}
