// Package credjson normalises credential timestamps at the JSON boundary.
//
// The host re-serialises the credential it stores, and a string whose contents
// look like a number is written back as a JSON **number**. A credential struct
// that declares such a field as `string` therefore fails to decode on the model
// and execution paths — and the failure is easy to miss, because the provider
// simply publishes no models:
//
//	cannot unmarshal number into Go struct field Credential.expires_at of type string
//
// The inverse happens too: a field declared as an integer is written by hand as
// a quoted number (`expire_time: "1768000000"`) and fails the same way.
//
// CoerceToStrings and CoerceToNumbers rewrite only the named top-level fields of
// a JSON object, so a credential type can keep plain `string` / `int64` fields
// and still accept both spellings. Callers wire them into an UnmarshalJSON on
// the credential type; every other use of the field stays untouched.
package credjson

import (
	"encoding/json"
	"strconv"
	"strings"
)

// CoerceToStrings rewrites the named top-level fields to JSON strings.
//
// A field already holding a string, an absent field, and null are left as they
// are. A field holding a number or a boolean becomes its literal text. When
// nothing changes the input slice is returned unchanged, so the common path does
// not re-encode the document.
func CoerceToStrings(raw []byte, fields ...string) ([]byte, error) {
	return coerce(raw, fields, func(literal string) json.RawMessage {
		return json.RawMessage(strconv.Quote(literal))
	}, func(text string) bool {
		return len(text) > 0 && text[0] == '"'
	})
}

// CoerceToNumbers rewrites the named top-level fields to JSON numbers when they
// hold a numeric string. Non-numeric strings are left alone so the caller's own
// decode error still names the offending value.
func CoerceToNumbers(raw []byte, fields ...string) ([]byte, error) {
	return coerce(raw, fields, func(literal string) json.RawMessage {
		return json.RawMessage(literal)
	}, func(text string) bool {
		return !isQuoted(text)
	})
}

// coerce applies rewrite to each named field whose current encoding does not
// already satisfy skip.
func coerce(raw []byte, fields []string, rewrite func(string) json.RawMessage, skip func(string) bool) ([]byte, error) {
	if len(raw) == 0 || len(fields) == 0 {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	changed := false
	for _, field := range fields {
		value, present := object[field]
		if !present {
			continue
		}
		text := strings.TrimSpace(string(value))
		if text == "" || text == "null" || skip(text) {
			continue
		}
		if numeric := numericLiteral(text); numeric != "" {
			object[field] = rewrite(numeric)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(object)
}

// isQuoted reports whether the literal is a JSON string.
func isQuoted(text string) bool {
	return len(text) > 0 && text[0] == '"'
}

// numericLiteral returns the bare numeric text of a literal, accepting either a
// JSON number or a JSON string that contains a number. It returns "" for
// anything else, including a non-numeric string.
func numericLiteral(text string) string {
	if isQuoted(text) {
		decoded, errString := strconv.Unquote(text)
		if errString != nil {
			return ""
		}
		decoded = strings.TrimSpace(decoded)
		if !looksNumeric(decoded) {
			return ""
		}
		return decoded
	}
	if !looksNumeric(text) {
		return ""
	}
	return text
}

// looksNumeric reports whether the text is a JSON number literal.
func looksNumeric(text string) bool {
	if text == "" {
		return false
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&number); errDecode != nil {
		return false
	}
	// Reject anything with trailing content, and reject JSON literals that are
	// valid but not numbers (true/false/null were filtered earlier).
	if _, errFloat := number.Float64(); errFloat != nil {
		return false
	}
	return !decoder.More()
}
