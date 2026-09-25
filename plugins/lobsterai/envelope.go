package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// decodeJSON decodes JSON with number fidelity preserved (json.Number), so a
// large numeric id is never mangled into scientific notation.
func decodeJSON(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	return decoder.Decode(out)
}

// The LobsterAI business endpoints all wrap their payload in a
// `{code, msg, data}` envelope; code != 0 means failure. The chat endpoint is
// the single exception — it returns bare SSE with no envelope
// (lobsterai.ts:21-23).
//
// The envelope parser performs three checks (lobsterai.ts:149-181), and the
// third one is the load-bearing one: `data` must be a JSON *object*. Upstream
// reports an expired access token as `code: 0` with `data: null`, so accepting
// it as success would push the failure downstream into a nil dereference.
type envelopeResult struct {
	OK      bool
	Code    int
	Message string
	Data    map[string]any
}

// parseEnvelope decodes and validates an envelope from raw response bytes.
func parseEnvelope(body []byte) envelopeResult {
	if len(body) == 0 {
		return envelopeResult{Code: -1, Message: "响应体为空"}
	}
	var decoded any
	if errUnmarshal := decodeJSON(body, &decoded); errUnmarshal != nil {
		return envelopeResult{Code: -1, Message: "响应不是合法 JSON"}
	}
	return parseEnvelopeValue(decoded)
}

// parseEnvelopeValue validates an already-decoded envelope value.
func parseEnvelopeValue(body any) envelopeResult {
	record, ok := body.(map[string]any)
	if !ok {
		return envelopeResult{Code: -1, Message: "响应不是 JSON 对象"}
	}
	code := -1
	if parsed, found := readNumberField(record, "code"); found {
		code = int(parsed)
	}
	message := readStringField(record, "msg")
	if message == "" {
		message = readStringField(record, "message")
	}
	if code != 0 {
		if message == "" {
			message = "code=" + strconv.Itoa(code)
		}
		return envelopeResult{Code: code, Message: message}
	}
	data := asRecord(record["data"])
	if data == nil {
		// The server message wins when present; the structural explanation is
		// only a fallback (lobsterai.ts:172-179).
		if message == "" {
			message = "data 为空（accessToken 可能已失效）"
		}
		return envelopeResult{Code: code, Message: message}
	}
	return envelopeResult{OK: true, Code: code, Message: message, Data: data}
}

// asRecord returns v as a JSON object, or nil for arrays and scalars.
func asRecord(value any) map[string]any {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return record
}

// readStringField reads a string field, tolerating a numeric JSON value the way
// the TypeScript reader does (lobsterai.ts:134-139).
func readStringField(source map[string]any, key string) string {
	if source == nil {
		return ""
	}
	switch typed := source[key].(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

// readNumberField reads a numeric field, accepting the string form of a number
// (lobsterai.ts:142-147).
func readNumberField(source map[string]any, key string) (float64, bool) {
	if source == nil {
		return 0, false
	}
	switch typed := source[key].(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || !isNumericLiteral(trimmed) {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// readBoolField reports whether a field is the JSON boolean true.
func readBoolField(source map[string]any, key string) bool {
	if source == nil {
		return false
	}
	value, _ := source[key].(bool)
	return value
}

// readStringArray reads an array field, keeping only string entries.
func readStringArray(source map[string]any, key string) []string {
	if source == nil {
		return nil
	}
	raw, ok := source[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, isText := item.(string); isText {
			out = append(out, text)
		}
	}
	return out
}

// isNumericLiteral reports whether value is a plain decimal number literal,
// matching the TypeScript guard `/^-?\d+(\.\d+)?$/`.
func isNumericLiteral(value string) bool {
	body := strings.TrimPrefix(value, "-")
	if body == "" {
		return false
	}
	parts := strings.Split(body, ".")
	if len(parts) > 2 {
		return false
	}
	for index, part := range parts {
		if part == "" {
			return false
		}
		if index == 1 && len(parts) == 1 {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

// stringifyValue renders an arbitrary JSON value as text for error messages.
func stringifyValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		encoded, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return ""
		}
		return string(encoded)
	}
}
