package usage

import (
	"log/slog"
	"sync"
	"time"

	"sage-router/internal/store"
)

// Entry represents a single usage record.
type Entry struct {
	RequestID    string
	Provider     string
	Model        string
	ConnectionID string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Cost         float64
	Latency      time.Duration
	Status       string
	CreatedAt    time.Time

	// Routing telemetry (populated for auto-routed requests)
	Strategy      string // "fast", "cheap", "best", "balanced", "manual"
	RoutingReason string // "session_affinity", "strategy_sort", "manual"
	AffinityHit   bool
}

// Tracker batches usage entries and periodically flushes to the store.
type Tracker struct {
	store  store.Store
	buffer chan *Entry
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewTracker creates a new usage tracker with background flushing.
func NewTracker(s store.Store) *Tracker {
	t := &Tracker{
		store:  s,
		buffer: make(chan *Entry, 1000),
		done:   make(chan struct{}),
	}
	t.wg.Add(1)
	go t.flushLoop()
	return t
}

// Record adds a usage entry to the buffer.
func (t *Tracker) Record(entry *Entry) {
	select {
	case t.buffer <- entry:
	default:
		// Buffer full — write synchronously
		slog.Warn("usage buffer full, writing synchronously")
		t.writeEntry(entry)
	}
}

// Flush drains all buffered entries synchronously.
func (t *Tracker) Flush() {
	for {
		select {
		case entry := <-t.buffer:
			t.writeEntry(entry)
		default:
			return
		}
	}
}

// Close stops the tracker and flushes remaining entries.
func (t *Tracker) Close() {
	close(t.done)
	t.wg.Wait()
}

func (t *Tracker) flushLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	batch := make([]*Entry, 0, 50)

	for {
		select {
		case entry := <-t.buffer:
			batch = append(batch, entry)
			if len(batch) >= 50 {
				t.flushBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				t.flushBatch(batch)
				batch = batch[:0]
			}
		case <-t.done:
			// Drain remaining
			close(t.buffer)
			for entry := range t.buffer {
				batch = append(batch, entry)
			}
			if len(batch) > 0 {
				t.flushBatch(batch)
			}
			return
		}
	}
}

func (t *Tracker) flushBatch(batch []*Entry) {
	for _, entry := range batch {
		t.writeEntry(entry)
	}
}

func (t *Tracker) writeEntry(entry *Entry) {
	storeEntry := &store.UsageEntry{
		ID:           entry.RequestID,
		RequestID:    entry.RequestID,
		Provider:     entry.Provider,
		Model:        entry.Model,
		ConnectionID: entry.ConnectionID,
		InputTokens:  entry.InputTokens,
		OutputTokens: entry.OutputTokens,
		TotalTokens:  entry.TotalTokens,
		Cost:         entry.Cost,
		Latency:      entry.Latency,
		Status:       entry.Status,
		CreatedAt:    entry.CreatedAt,
	}
	if err := t.store.RecordUsage(storeEntry); err != nil {
		slog.Error("failed to record usage", "error", err)
	}
}
