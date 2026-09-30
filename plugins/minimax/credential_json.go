package main

import (
	"encoding/json"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
)

// UnmarshalJSON decodes a MiniMax credential as the host may hand it back.
//
// `expires_at` is a STRING millisecond timestamp in the credential contract
// (`minimax-oauth.ts:parseMinimaxTokenGrant`), but the host re-serialises
// credentials it stores and writes a numeric-looking string back as a JSON
// number. Decoding that into the plain `string` field fails, and the failure is
// easy to miss: the provider then simply publishes no models and every executor
// call reports a broken credential.
//
// ⚠️ `ExpiresAtMS` accepts ONLY a pure digit string, so a value that arrives as
// `1.790645562328e+12` (a float that survived a round trip) cannot be rescued
// afterwards — the coercion below is what keeps it usable, by rewriting the
// number into the plain integer spelling before the field is decoded.
func (c *Credential) UnmarshalJSON(data []byte) error {
	coerced, errCoerce := credjson.CoerceToStrings(data, "expires_at")
	if errCoerce != nil {
		return errCoerce
	}
	// A non-string, non-null `expires_at` that survived coercion is unusable;
	// drop it rather than lose the whole credential over one timestamp. The
	// JWT fallback still resolves an expiry for any token that is a JWT, and a
	// credential with no expiry is left to the server's judgement rather than
	// being declared dead.
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
