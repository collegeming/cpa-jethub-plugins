package main

import (
	"encoding/json"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
)

// UnmarshalJSON decodes a Raccoon credential as the host may hand it back.
//
// `expires_at` is a STRING millisecond timestamp in the credential contract
// (`raccoon-oauth.ts:245`), but the host re-serialises credentials it stores and
// writes a numeric-looking string back as a JSON number. Decoding that into the
// plain `string` field fails, and the failure is easy to miss: the provider then
// simply publishes no models and every executor call reports a broken
// credential. CoerceToStrings rewrites only that named field.
func (c *Credential) UnmarshalJSON(data []byte) error {
	coerced, errCoerce := credjson.CoerceToStrings(data, "expires_at")
	if errCoerce != nil {
		return errCoerce
	}
	sanitized, errSanitize := dropNonStringField(coerced, "expires_at")
	if errSanitize != nil {
		return errSanitize
	}
	// The alias drops this method, so the decode below cannot recurse.
	type plain Credential
	return json.Unmarshal(sanitized, (*plain)(c))
}

// dropNonStringField removes a top-level field whose value is neither a string
// nor null.
//
// The source treats a non-string `expires_at` as ABSENT rather than as a broken
// credential (`raccoon.ts:142-150`), and losing a whole credential over one
// unusable timestamp would be far worse than losing the timestamp — the JWT
// fallback still resolves the expiry.
func dropNonStringField(raw []byte, field string) ([]byte, error) {
	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	value, present := object[field]
	if !present {
		return raw, nil
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, `"`) {
		return raw, nil
	}
	delete(object, field)
	return json.Marshal(object)
}
