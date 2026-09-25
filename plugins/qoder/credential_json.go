package main

import (
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
)

// UnmarshalJSON decodes a Qoder credential as the host may hand it back.
//
// The expiry fields are epoch integers, so the host does not rewrite them; but a
// credential written by hand (or produced by another tool) can carry them as
// quoted numbers, which would otherwise fail the decode and leave the provider
// silently publishing no models. CoerceToNumbers accepts both spellings.
func (c *Credential) UnmarshalJSON(data []byte) error {
	coerced, errCoerce := credjson.CoerceToNumbers(data, "expire_time", "refresh_token_expire_time")
	if errCoerce != nil {
		return errCoerce
	}
	// The alias drops this method, so the decode below cannot recurse.
	type plain Credential
	return json.Unmarshal(coerced, (*plain)(c))
}
