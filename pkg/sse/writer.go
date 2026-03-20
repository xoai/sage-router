package sse

import (
	"fmt"
	"net/http"
	"strings"
)

// SetHeaders configures the response headers required for an SSE stream.
// Call this before writing any events.
func SetHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx buffering
}

// WriteEvent writes a full SSE event to w and flushes. Fields that are empty
// or zero-valued are omitted.
func WriteEvent(w http.ResponseWriter, ev Event) {
	if ev.ID != "" {
		fmt.Fprintf(w, "id: %s\n", ev.ID)
	}
	if ev.Type != "" {
		fmt.Fprintf(w, "event: %s\n", ev.Type)
	}
	if ev.Retry > 0 {
		fmt.Fprintf(w, "retry: %d\n", ev.Retry)
	}

	// Data may contain newlines; each line must be its own "data:" field.
	for _, line := range strings.Split(ev.Data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}

	// Blank line terminates the event.
	fmt.Fprint(w, "\n")

	flush(w)
}

// WriteChunk is a convenience helper that writes a single SSE event whose data
// is the provided byte slice. This is the common case for proxying
// OpenAI-compatible streaming chunks.
func WriteChunk(w http.ResponseWriter, data []byte) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	flush(w)
}

// WriteDone writes the OpenAI-style [DONE] sentinel and flushes.
func WriteDone(w http.ResponseWriter) {
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush(w)
}

// flush attempts to flush buffered data to the client. If the ResponseWriter
// implements http.Flusher (virtually all do), it calls Flush().
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
