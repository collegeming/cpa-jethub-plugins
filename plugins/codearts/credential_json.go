package main

import (
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
)

// UnmarshalJSON decodes a CodeArts credential exactly as the host hands it back.
//
// The host re-serialises the credential it stores, and a timestamp whose text
// looks like a number is written back as a JSON number. Without this coercion
// the decode into the string field fails on the model and execution paths, and
// the failure is silent: the provider simply publishes no models.
func (c *Credential) UnmarshalJSON(data []byte) error {
	coerced, errCoerce := credjson.CoerceToStrings(data, "expires_at")
	if errCoerce != nil {
		return errCoerce
	}
	// The alias drops this method, so the decode below cannot recurse.
	type plain Credential
	return json.Unmarshal(coerced, (*plain)(c))
}
