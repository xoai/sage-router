package routing

import (
	"fmt"
	"testing"
	"time"
)

func TestSessionCache_GetSet(t *testing.T) {
	c := NewSessionCache()

	c.Set("hello world", "anthropic", "claude-sonnet-4-6")

	entry := c.Get("hello world")
	if entry == nil {
		t.Fatal("expected entry")
	}
	if entry.Provider != "anthropic" || entry.Model != "claude-sonnet-4-6" {
		t.Errorf("got %s/%s", entry.Provider, entry.Model)
	}
	if entry.TurnCount != 1 {
		t.Errorf("turn count = %d, want 1", entry.TurnCount)
	}
}

func TestSessionCache_Update(t *testing.T) {
	c := NewSessionCache()

	c.Set("hello", "openai", "gpt-4o")
	c.Set("hello", "openai", "gpt-4o")
	c.Set("hello", "openai", "gpt-4o")

	entry := c.Get("hello")
	if entry.TurnCount != 3 {
		t.Errorf("turn count = %d, want 3", entry.TurnCount)
	}
}

func TestSessionCache_Miss(t *testing.T) {
	c := NewSessionCache()
	if c.Get("nonexistent") != nil {
		t.Error("expected nil for miss")
	}
}

func TestSessionCache_TTLExpiry(t *testing.T) {
	c := NewSessionCache()
	c.ttl = 10 * time.Millisecond

	c.Set("hello", "anthropic", "claude-sonnet-4-6")
	time.Sleep(20 * time.Millisecond)

	if c.Get("hello") != nil {
		t.Error("expected nil after TTL expiry")
	}
}

func TestSessionCache_LRUEviction(t *testing.T) {
	c := NewSessionCache()
	c.max = 3

	c.Set("msg1", "a", "m1")
	c.Set("msg2", "a", "m2")
	c.Set("msg3", "a", "m3")
	c.Set("msg4", "a", "m4") // evicts msg1

	if c.Get("msg1") != nil {
		t.Error("msg1 should be evicted")
	}
	if c.Get("msg4") == nil {
		t.Error("msg4 should exist")
	}
	if c.Len() != 3 {
		t.Errorf("len = %d, want 3", c.Len())
	}
}

func TestSessionCache_HashStability(t *testing.T) {
	// Same message always produces same hash
	h1 := hashKey("hello world this is a test")
	h2 := hashKey("hello world this is a test")
	if h1 != h2 {
		t.Error("hash should be stable")
	}

	// Different messages produce different hashes
	h3 := hashKey("completely different message")
	if h1 == h3 {
		t.Error("different messages should hash differently")
	}
}

func TestSessionCache_LongMessageTruncation(t *testing.T) {
	// Messages longer than 200 bytes should hash the same prefix
	long1 := fmt.Sprintf("%-200s extra1", "same prefix")
	long2 := fmt.Sprintf("%-200s extra2", "same prefix")

	h1 := hashKey(long1)
	h2 := hashKey(long2)
	if h1 != h2 {
		t.Error("messages with same first 200 bytes should hash the same")
	}
}
