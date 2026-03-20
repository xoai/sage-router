package routing

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"
	"time"
)

const (
	defaultMaxStoreSize = 50 * 1024 * 1024 // 50MB
	defaultStoreTTL     = 2 * time.Hour
)

// Turn is a single message in a conversation.
type Turn struct {
	Role       string // "user" or "assistant"
	Content    []byte // gzip-compressed text (images stripped)
	TokenCount int    // estimated token count
	Model      string // which model generated this (assistant only)
}

// History is the conversation history for one session.
type History struct {
	Turns     []Turn
	LastModel string
	UpdatedAt time.Time
}

// ConversationStore is an in-memory ring buffer of conversation histories
// keyed by session hash. Used for telemetry, retry detection, and (v2)
// context bridge generation.
type ConversationStore struct {
	mu      sync.RWMutex
	convos  map[uint64]*History
	order   []uint64 // LRU
	maxSize int64
	curSize int64
	ttl     time.Duration
}

// NewConversationStore creates a store with default settings.
func NewConversationStore() *ConversationStore {
	return &ConversationStore{
		convos:  make(map[uint64]*History),
		maxSize: defaultMaxStoreSize,
		ttl:     defaultStoreTTL,
	}
}

// AddTurn appends a turn to the conversation identified by firstMsg.
func (s *ConversationStore) AddTurn(firstMsg, role, content, model string) {
	key := hashKey(firstMsg)
	compressed := compress([]byte(content))
	tokenCount := (len(content) + 3) / 4

	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.convos[key]
	if !ok {
		// Evict if over budget
		for s.curSize+int64(len(compressed)) > s.maxSize && len(s.order) > 0 {
			s.evictOldest()
		}

		h = &History{}
		s.convos[key] = h
		s.order = append(s.order, key)
	}

	turn := Turn{
		Role:       role,
		Content:    compressed,
		TokenCount: tokenCount,
		Model:      model,
	}

	h.Turns = append(h.Turns, turn)
	h.UpdatedAt = time.Now()
	if model != "" {
		h.LastModel = model
	}
	s.curSize += int64(len(compressed))

	// Touch LRU
	s.touchLRU(key)
}

// GetHistory returns the conversation history for a session, or nil.
func (s *ConversationStore) GetHistory(firstMsg string) *History {
	key := hashKey(firstMsg)

	s.mu.RLock()
	h, ok := s.convos[key]
	s.mu.RUnlock()

	if !ok {
		return nil
	}
	if time.Since(h.UpdatedAt) > s.ttl {
		s.mu.Lock()
		s.removeKey(key)
		s.mu.Unlock()
		return nil
	}
	return h
}

// Len returns the number of stored conversations.
func (s *ConversationStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.convos)
}

// Size returns the current memory usage in bytes.
func (s *ConversationStore) Size() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.curSize
}

func (s *ConversationStore) touchLRU(key uint64) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, key)
}

func (s *ConversationStore) evictOldest() {
	if len(s.order) == 0 {
		return
	}
	oldest := s.order[0]
	s.order = s.order[1:]
	s.removeKey(oldest)
}

func (s *ConversationStore) removeKey(key uint64) {
	if h, ok := s.convos[key]; ok {
		for _, t := range h.Turns {
			s.curSize -= int64(len(t.Content))
		}
		delete(s.convos, key)
	}
}

// DecompressTurn decompresses a turn's content.
func DecompressTurn(t Turn) string {
	return string(decompress(t.Content))
}

func compress(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func decompress(data []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data // return raw if not gzipped
	}
	defer r.Close()
	out, _ := io.ReadAll(r)
	return out
}
