package sse

// Event represents a single Server-Sent Event.
type Event struct {
	// Type is the event type (from the "event:" field). Empty means the default
	// message type.
	Type string

	// Data is the event payload. Multiple "data:" lines are joined with newlines.
	Data string

	// ID is the last-event-id (from the "id:" field).
	ID string

	// Retry is the reconnection time in milliseconds (from the "retry:" field).
	// Zero means the field was not present.
	Retry int
}

// IsDone reports whether this event signals the end of the stream.
// OpenAI-compatible APIs send data: [DONE] as the final event.
func (e Event) IsDone() bool {
	return e.Data == "[DONE]"
}
