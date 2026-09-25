package credjson

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCoerceToStringsTurnsNumbersIntoStrings(t *testing.T) {
	// This is the shape the host produces: a stored epoch that used to be a
	// string comes back as a JSON number.
	raw := []byte(`{"access_token":"AT","expires_at":4102444800000,"nickname":"p"}`)
	coerced, errCoerce := CoerceToStrings(raw, "expires_at")
	if errCoerce != nil {
		t.Fatalf("CoerceToStrings: %v", errCoerce)
	}

	var decoded struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   string `json:"expires_at"`
		Nickname    string `json:"nickname"`
	}
	if errUnmarshal := json.Unmarshal(coerced, &decoded); errUnmarshal != nil {
		t.Fatalf("decoding into string fields must succeed: %v", errUnmarshal)
	}
	if decoded.ExpiresAt != "4102444800000" {
		t.Fatalf("expires_at = %q, want the decimal text", decoded.ExpiresAt)
	}
	if decoded.AccessToken != "AT" || decoded.Nickname != "p" {
		t.Fatalf("untouched fields changed: %#v", decoded)
	}
}

// TestCoerceToStringsIsIdentityWhenNothingChanges guards the common path: a
// credential that already stores strings must not be re-encoded.
func TestCoerceToStringsIsIdentityWhenNothingChanges(t *testing.T) {
	raw := []byte(`{"expires_at":"4102444800000","refresh_expires_at":"4134000000000"}`)
	coerced, errCoerce := CoerceToStrings(raw, "expires_at", "refresh_expires_at")
	if errCoerce != nil {
		t.Fatalf("CoerceToStrings: %v", errCoerce)
	}
	if !bytes.Equal(coerced, raw) {
		t.Fatalf("document was rewritten despite needing no change:\n got %s\nwant %s", coerced, raw)
	}
}

func TestCoerceToStringsLeavesOtherShapesAlone(t *testing.T) {
	raw := []byte(`{"a":"text","b":null,"c":true,"missing":1}`)
	coerced, errCoerce := CoerceToStrings(raw, "a", "b", "c", "d")
	if errCoerce != nil {
		t.Fatalf("CoerceToStrings: %v", errCoerce)
	}
	// "a" is a non-numeric string and must survive verbatim; b/c/d must not
	// become strings because null and booleans are not timestamps.
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(coerced, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["a"] != "text" {
		t.Errorf("a = %#v, want \"text\"", decoded["a"])
	}
	if decoded["b"] != nil {
		t.Errorf("b = %#v, want null", decoded["b"])
	}
	if decoded["c"] != true {
		t.Errorf("c = %#v, want true", decoded["c"])
	}
	if _, present := decoded["d"]; present {
		t.Error("d must not be invented")
	}
}

func TestCoerceToNumbersAcceptsQuotedNumbers(t *testing.T) {
	raw := []byte(`{"expire_time":"1768000000","uid":"u"}`)
	coerced, errCoerce := CoerceToNumbers(raw, "expire_time")
	if errCoerce != nil {
		t.Fatalf("CoerceToNumbers: %v", errCoerce)
	}
	var decoded struct {
		ExpireTime int64  `json:"expire_time"`
		UID        string `json:"uid"`
	}
	if errUnmarshal := json.Unmarshal(coerced, &decoded); errUnmarshal != nil {
		t.Fatalf("decoding into an int64 field must succeed: %v", errUnmarshal)
	}
	if decoded.ExpireTime != 1768000000 {
		t.Fatalf("expire_time = %d, want 1768000000", decoded.ExpireTime)
	}
}

// TestCoerceToNumbersKeepsNonNumericStrings lets the caller's own decode error
// name the offending value instead of silently dropping it.
func TestCoerceToNumbersKeepsNonNumericStrings(t *testing.T) {
	raw := []byte(`{"expire_time":"soon"}`)
	coerced, errCoerce := CoerceToNumbers(raw, "expire_time")
	if errCoerce != nil {
		t.Fatalf("CoerceToNumbers: %v", errCoerce)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(coerced, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if decoded["expire_time"] != "soon" {
		t.Fatalf("expire_time = %#v, want the original string", decoded["expire_time"])
	}
}

func TestCoerceRejectsMalformedJSON(t *testing.T) {
	if _, errCoerce := CoerceToStrings([]byte(`{"a":`), "a"); errCoerce == nil {
		t.Fatal("malformed JSON must be reported, not swallowed")
	}
}

// TestCoerceToNumbersIsIdentityWhenNothingChanges mirrors the string case.
func TestCoerceToNumbersIsIdentityWhenNothingChanges(t *testing.T) {
	raw := []byte(`{"expire_time":1768000000}`)
	coerced, errCoerce := CoerceToNumbers(raw, "expire_time")
	if errCoerce != nil {
		t.Fatalf("CoerceToNumbers: %v", errCoerce)
	}
	if !bytes.Equal(coerced, raw) {
		t.Fatalf("document was rewritten despite needing no change: %s", coerced)
	}
}
