// Package sse implements the Server-Sent Events framing used by the CodeArts
// Snap-Access chat endpoint and by CPA when streaming Chat Completions back to
// a client.
package sse

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Done is the terminal payload of an OpenAI-style SSE stream.
const Done = "[DONE]"

// Scanner incrementally extracts `data:` payloads from a byte stream that may
// be split at arbitrary boundaries.
type Scanner struct {
	buf []byte
}

// Feed appends upstream bytes and returns every complete `data:` payload.
// Payloads are returned verbatim (without the `data:` prefix or trailing
// newline) so callers can compare against Done or unmarshal JSON.
func (s *Scanner) Feed(chunk []byte) []string {
	if len(chunk) > 0 {
		s.buf = append(s.buf, chunk...)
	}
	var events []string
	for {
		idx := bytes.IndexByte(s.buf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(s.buf[:idx]), "\r")
		s.buf = s.buf[idx+1:]
		if payload, ok := parseDataLine(line); ok {
			events = append(events, payload)
		}
	}
	return events
}

// Buffered reports whether an incomplete line is still pending.
func (s *Scanner) Buffered() bool { return len(bytes.TrimSpace(s.buf)) > 0 }

// parseDataLine recognises an SSE data field, tolerating the optional single
// space after the colon that the spec allows.
func parseDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "), true
}

// Encode frames a payload as one SSE event.
func Encode(payload string) []byte {
	var b strings.Builder
	b.Grow(len(payload) + 8)
	b.WriteString("data: ")
	b.WriteString(payload)
	b.WriteString("\n\n")
	return []byte(b.String())
}

// EncodeJSON marshals v and frames it as one SSE event.
func EncodeJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Encode(string(raw)), nil
}

// DoneEvent is the terminating `data: [DONE]` frame.
func DoneEvent() []byte { return Encode(Done) }
