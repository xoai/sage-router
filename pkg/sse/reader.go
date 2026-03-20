package sse

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Read parses an SSE stream from r and sends each complete event to the
// returned channel. The channel is closed when the stream ends (io.EOF),
// when a [DONE] event is encountered, or when the provided context (via
// the reader being closed) terminates.
//
// Callers should range over the returned channel:
//
//	for ev := range sse.Read(resp.Body) { ... }
func Read(r io.Reader) <-chan Event {
	ch := make(chan Event, 8)
	go func() {
		defer close(ch)
		readStream(r, ch)
	}()
	return ch
}

func readStream(r io.Reader, ch chan<- Event) {
	scanner := bufio.NewScanner(r)

	var (
		eventType string
		dataLines []string
		id        string
		retry     int
	)

	for scanner.Scan() {
		line := scanner.Text()

		// An empty line signals the end of an event block.
		if line == "" {
			if len(dataLines) > 0 {
				ev := Event{
					Type:  eventType,
					Data:  strings.Join(dataLines, "\n"),
					ID:    id,
					Retry: retry,
				}
				ch <- ev

				// If the event is a [DONE] sentinel, stop reading.
				if ev.IsDone() {
					return
				}
			}

			// Reset accumulators for next event.
			eventType = ""
			dataLines = nil
			id = ""
			retry = 0
			continue
		}

		// Lines starting with a colon are comments; ignore them.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Split on first colon.
		field, value := parseField(line)
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			eventType = value
		case "id":
			id = value
		case "retry":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				retry = n
			}
		}
	}

	// Flush any trailing event that was not followed by a blank line (common
	// when the upstream simply closes the connection).
	if len(dataLines) > 0 {
		ch <- Event{
			Type:  eventType,
			Data:  strings.Join(dataLines, "\n"),
			ID:    id,
			Retry: retry,
		}
	}
}

// parseField splits a line into the field name and its value. Per the SSE
// spec, if there is a colon the value starts after the first colon (and one
// optional leading space). If there is no colon the entire line is the field
// name with an empty value.
func parseField(line string) (field, value string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	field = line[:idx]
	value = line[idx+1:]
	// Remove at most one leading space from value.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
