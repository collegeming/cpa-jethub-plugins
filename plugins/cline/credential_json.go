package main

import (
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
)

// UnmarshalJSON decodes a Cline credential as the host may hand it back.
//
// The host re-serialises the credential it stores, and a string whose contents
// look like a number comes back as a JSON **number**. Two directions are at risk
// here and both are covered:
//
//   - `expire_time` is an epoch integer, so a hand-written or third-party auth
//     file may carry it as a quoted number and fail a plain int64 decode;
//   - the text fields are opposite: an opaque token that happens to be all
//     digits would come back as a number and fail a plain string decode. The
//     access token carries the `workos:` prefix so it is safe today, but the
//     refresh token is opaque (`tmgEeM…`) and could be anything.
//
// CoerceToStrings runs first and CoerceToNumbers second; both are no-ops on the
// encoding they expect, so the pair is order-independent and idempotent.
func (c *Credential) UnmarshalJSON(data []byte) error {
	textual, errText := credjson.CoerceToStrings(data, "access_token", "refresh_token", "account_id", "email", "nickname")
	if errText != nil {
		return errText
	}
	numeric, errNumber := credjson.CoerceToNumbers(textual, "expire_time")
	if errNumber != nil {
		return errNumber
	}
	// The alias drops this method, so the decode below cannot recurse.
	type plain Credential
	return json.Unmarshal(numeric, (*plain)(c))
}
