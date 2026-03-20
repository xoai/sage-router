package routing

import (
	"strings"
	"testing"
	"time"
)

func TestConversationStore_AddAndGet(t *testing.T) {
	s := NewConversationStore()

	s.AddTurn("hello", "user", "What is Go?", "")
	s.AddTurn("hello", "assistant", "Go is a programming language.", "claude-sonnet-4-6")

	h := s.GetHistory("hello")
	if h == nil {
		t.Fatal("expected history")
	}
	if len(h.Turns) != 2 {
		t.Errorf("turns = %d, want 2", len(h.Turns))
	}
	if h.LastModel != "claude-sonnet-4-6" {
		t.Errorf("last model = %q", h.LastModel)
	}

	// Decompress and verify
	text := DecompressTurn(h.Turns[1])
	if text != "Go is a programming language." {
		t.Errorf("decompressed = %q", text)
	}
}

func TestConversationStore_TTLExpiry(t *testing.T) {
	s := NewConversationStore()
	s.ttl = 10 * time.Millisecond

	s.AddTurn("hello", "user", "test", "")
	time.Sleep(20 * time.Millisecond)

	if s.GetHistory("hello") != nil {
		t.Error("expected nil after TTL")
	}
}

func TestConversationStore_SizeEviction(t *testing.T) {
	s := NewConversationStore()
	s.maxSize = 200 // very small budget

	// Use unique chars to make gzip less effective
	s.AddTurn("msg1", "user", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP"+strings.Repeat("x", 100), "")
	s.AddTurn("msg2", "user", "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210abcdefghijklmnop"+strings.Repeat("y", 100), "")
	s.AddTurn("msg3", "user", "1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop"+strings.Repeat("z", 100), "")

	// msg1 should be evicted (oldest, over budget)
	if s.GetHistory("msg1") != nil {
		t.Error("msg1 should be evicted")
	}
	if s.GetHistory("msg3") == nil {
		t.Error("msg3 should exist")
	}
}

func TestConversationStore_Compression(t *testing.T) {
	s := NewConversationStore()

	// Large repetitive content compresses well
	large := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)
	s.AddTurn("hello", "user", large, "")

	h := s.GetHistory("hello")
	if h == nil {
		t.Fatal("expected history")
	}

	// Compressed size should be much smaller than raw
	compressedSize := len(h.Turns[0].Content)
	rawSize := len(large)
	if compressedSize >= rawSize {
		t.Errorf("compressed (%d) should be < raw (%d)", compressedSize, rawSize)
	}

	// Decompress should match original
	got := DecompressTurn(h.Turns[0])
	if got != large {
		t.Error("decompressed content doesn't match original")
	}
}

func TestConversationStore_Miss(t *testing.T) {
	s := NewConversationStore()
	if s.GetHistory("nonexistent") != nil {
		t.Error("expected nil for miss")
	}
}
