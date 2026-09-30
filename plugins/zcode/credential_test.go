package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// Credential tests: the JSON record itself, and the AES-256-GCM store the
// official client writes (the optional import path).
//
// The encryption tests matter beyond the import feature: the derivation is a
// four-part string that has to match the Node implementation character for
// character, and the ONLY symptom of a mismatch is a GCM authentication failure.
// A tamper case is therefore included explicitly — a round-trip alone would also
// pass if the implementation ignored the tag.

// TestCredentialRoundTrip covers the auth-file record.
func TestCredentialRoundTrip(t *testing.T) {
	original := sampleCredential()
	encoded, errEncode := original.Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	parsed, errParse := ParseCredential(encoded)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.ZCodeJWT != original.ZCodeJWT {
		t.Errorf("zcode_jwt = %q, want %q", parsed.ZCodeJWT, original.ZCodeJWT)
	}
	if parsed.DeviceMid != original.DeviceMid {
		t.Errorf("device_mid = %q, want %q", parsed.DeviceMid, original.DeviceMid)
	}
	if parsed.UserID != original.UserID {
		t.Errorf("user_id = %q, want %q", parsed.UserID, original.UserID)
	}
	if parsed.AccountLabel != original.AccountLabel {
		t.Errorf("account_label = %q, want %q", parsed.AccountLabel, original.AccountLabel)
	}
	if parsed.Source != "plugin" {
		t.Errorf("source = %q, want plugin", parsed.Source)
	}
	if !parsed.Usable() {
		t.Error("a round-tripped credential is not usable")
	}
}

// TestCredentialFieldNamesMatchTheReference pins the JSON spelling.
//
// A credential written by the reference has to read back here, and vice versa:
// the names are the wire contract between the two implementations.
func TestCredentialFieldNamesMatchTheReference(t *testing.T) {
	encoded, errEncode := sampleCredential().Encode()
	if errEncode != nil {
		t.Fatalf("Encode: %v", errEncode)
	}
	var members map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(encoded, &members); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	for _, name := range []string{
		"zcode_jwt", "device_mid", "user_id", "bigmodel_access_token",
		"account_label", "app_version", "source",
	} {
		if _, ok := members[name]; !ok {
			t.Errorf("the record is missing the %q member", name)
		}
	}
	// ⚠ There must be NO `refresh_token` member: the reference types it as
	// `undefined` precisely because the channel has no renewal, and a member named
	// after one invites a reader to believe a refresh is possible.
	if _, ok := members["refresh_token"]; ok {
		t.Error("the record carries a refresh_token member although ZCode credentials are not refreshable")
	}
}

// TestParseCredentialRejectsIncompleteRecords covers the usability test.
//
// A record is accepted on the two fields upstream actually requires. Everything
// else is optional: a missing coding-plan key only removes the ultra channel, and
// a missing user_id only disables de-duplication.
func TestParseCredentialRejectsIncompleteRecords(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantErr  bool
		missing  string
		wantCode string
	}{
		{
			name:    "both hard requirements present",
			raw:     `{"zcode_jwt":"jwt","device_mid":"mid"}`,
			wantErr: false,
		},
		{
			name:    "optional members absent is fine",
			raw:     `{"zcode_jwt":"jwt","device_mid":"mid","user_id":"","bigmodel_access_token":""}`,
			wantErr: false,
		},
		{
			name:     "missing jwt",
			raw:      `{"device_mid":"mid"}`,
			wantErr:  true,
			missing:  "zcode_jwt",
			wantCode: "incomplete_credential",
		},
		{
			name:     "missing device id",
			raw:      `{"zcode_jwt":"jwt"}`,
			wantErr:  true,
			missing:  "device_mid",
			wantCode: "incomplete_credential",
		},
		{
			name:     "whitespace-only values count as absent",
			raw:      `{"zcode_jwt":"   ","device_mid":"mid"}`,
			wantErr:  true,
			missing:  "zcode_jwt",
			wantCode: "incomplete_credential",
		},
		{
			name:     "empty document",
			raw:      `   `,
			wantErr:  true,
			wantCode: "missing_credential",
		},
		{
			name:     "malformed json",
			raw:      `{"zcode_jwt":`,
			wantErr:  true,
			wantCode: "invalid_credential",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errParse := ParseCredential([]byte(tc.raw))
			if !tc.wantErr {
				if errParse != nil {
					t.Fatalf("ParseCredential: %v", errParse)
				}
				return
			}
			if errParse == nil {
				t.Fatal("expected an error")
			}
			envelope := &abiboot.EnvelopeError{}
			if !errors.As(errParse, &envelope) {
				t.Fatalf("error type = %T, want a plugin envelope error", errParse)
			}
			if envelope.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
			if tc.missing != "" && !strings.Contains(envelope.Message, tc.missing) {
				t.Errorf("message %q does not name the missing field %q", envelope.Message, tc.missing)
			}
		})
	}
}

// TestCredentialNormalisationTrimsValues guards the hand-edited-file case: the
// auth file is edited by people, and a trailing newline must not create a second
// account.
func TestCredentialNormalisationTrimsValues(t *testing.T) {
	credential, errParse := ParseCredential([]byte(
		`{"zcode_jwt":"  jwt  ","device_mid":"\tmid\n","user_id":" id ","account_label":" label "}`))
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if credential.ZCodeJWT != "jwt" || credential.DeviceMid != "mid" ||
		credential.UserID != "id" || credential.AccountLabel != "label" {
		t.Fatalf("values were not trimmed: %#v", credential)
	}
}

// TestCredentialIdentityKeyIsUserIDOnly covers the de-duplication rule.
//
// ⚠ `device_mid` must NOT be the fallback. It is generated fresh on every login,
// so using it as an identity key is exactly how one account becomes several — the
// real defect the reference records.
func TestCredentialIdentityKeyIsUserIDOnly(t *testing.T) {
	withUserID := sampleCredential()
	if got := withUserID.identityKey(); got != "user-1234567890" {
		t.Fatalf("identityKey = %q, want the user id", got)
	}
	withoutUserID := sampleCredential()
	withoutUserID.UserID = ""
	if got := withoutUserID.identityKey(); got != "" {
		t.Fatalf("identityKey = %q, want an EMPTY string so the caller skips de-duplication", got)
	}
	// The device id is present in both cases and must never leak into the key.
	if strings.Contains(withoutUserID.identityKey(), withoutUserID.DeviceMid) {
		t.Fatal("the identity key fell back to device_mid")
	}
}

// TestCredentialDisplayLabelNeverFallsBackToDeviceMid guards the same rule for
// what the user sees.
func TestCredentialDisplayLabelNeverFallsBackToDeviceMid(t *testing.T) {
	cases := []struct {
		name       string
		credential *Credential
		want       string
		notWant    string
	}{
		{
			name:       "label wins",
			credential: &Credential{AccountLabel: "尾号8000", UserID: "user-1", ZCodeJWT: "jwt-abcdefgh", DeviceMid: "mid"},
			want:       "尾号8000",
		},
		{
			name:       "user id is the fallback",
			credential: &Credential{UserID: "user-1", ZCodeJWT: "jwt-abcdefgh", DeviceMid: "mid"},
			want:       "user-1",
		},
		{
			name:       "then a masked jwt fragment",
			credential: &Credential{ZCodeJWT: "jwt-abcdefgh", DeviceMid: "device-should-not-appear"},
			want:       "…abcdefgh",
			notWant:    "device-should-not-appear",
		},
		{
			name:       "never the device id",
			credential: &Credential{DeviceMid: "device-should-not-appear"},
			want:       "ZCode 账号",
			notWant:    "device-should-not-appear",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.credential.displayLabel()
			if got != tc.want {
				t.Errorf("displayLabel = %q, want %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("displayLabel = %q, which leaks %q", got, tc.notWant)
			}
		})
	}
}

// TestAuthFileNameIsStableAcrossLogins is the regression for the "one account,
// two files" defect.
//
// The derived name must depend only on values that survive a re-login. The device
// id changes on every login, so a name built on it would put a second file next to
// the first for the same account.
func TestAuthFileNameIsStableAcrossLogins(t *testing.T) {
	first := sampleCredential()
	second := sampleCredential()
	// Same account, a fresh device id — what a second login actually produces.
	second.DeviceMid = "ffffffff-1111-2222-3333-444444444444"
	if defaultAuthFileName(first) != defaultAuthFileName(second) {
		t.Fatalf("the derived file name changed when only device_mid changed: %q vs %q",
			defaultAuthFileName(first), defaultAuthFileName(second))
	}
	if got := defaultAuthFileName(first); got != "zcode-user-1234567890.json" {
		t.Fatalf("derived name = %q, want it built from user_id", got)
	}
	// A credential without a user id still produces a usable, constant name.
	bare := &Credential{ZCodeJWT: "jwt", DeviceMid: "mid"}
	if got := defaultAuthFileName(bare); got != "zcode-account.json" {
		t.Fatalf("derived name for a bare credential = %q, want zcode-account.json", got)
	}
}

// TestAuthNameForHostPrefersTheHostSuppliedName covers the rule the shared
// `authfile` helper exists for.
func TestAuthNameForHostPrefersTheHostSuppliedName(t *testing.T) {
	credential := sampleCredential()
	cases := []struct {
		name     string
		incoming string
		path     string
		source   string
		want     string
	}{
		{"the parse-time file name wins", "existing.json", "", "", "existing.json"},
		{"then the path attribute", "", "/auths/from-path.json", "", "from-path.json"},
		{"then the source attribute", "", "", "from-source.json", "from-source.json"},
		{"a runtime index is skipped, not used as a name", "runtime-index-7", "", "", "zcode-user-1234567890.json"},
		{"a traversal segment is refused", "../escape.json", "", "", "zcode-user-1234567890.json"},
		{"nothing usable falls back to the derived name", "", "", "", "zcode-user-1234567890.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authNameForHost(tc.incoming, tc.path, tc.source, credential); got != tc.want {
				t.Fatalf("authNameForHost = %q, want %q", got, tc.want)
			}
		})
	}
}

// ───────────────────── official-client credential decryption ─────────────────────

// TestDecryptClientCredentialRoundTrip proves the two independent implementations
// agree: the encrypted value is built with crypto/aes the way Node's
// `createCipheriv` would, and decrypted by the plugin's own reader.
func TestDecryptClientCredentialRoundTrip(t *testing.T) {
	const secret = "test-explicit-secret"
	t.Setenv(credentialSecretEnv, secret)
	key := deriveTestKey(secret)

	cases := []struct {
		name  string
		plain string
	}{
		{"ascii", "zcode-jwt-value"},
		{"a multi-byte payload", "zcode-jwt-测试值"},
		{"a long payload", strings.Repeat("A1b2C3d4", 512)},
		{"a payload that is itself json", `{"user_id":"u1","phone":"13800138000"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted := encryptClientValue(t, tc.plain, key)
			if !strings.HasPrefix(encrypted, credentialPrefix) {
				t.Fatalf("ciphertext does not carry the %q prefix", credentialPrefix)
			}
			decrypted, errDecrypt := decryptClientCredentialValue(encrypted)
			if errDecrypt != nil {
				t.Fatalf("decrypt: %v", errDecrypt)
			}
			if decrypted != tc.plain {
				t.Fatalf("round trip = %q, want %q", decrypted, tc.plain)
			}
		})
	}
}

// TestDecryptClientCredentialReturnsPlaintextUnchanged covers the official
// `decrypt()` behaviour: no prefix means the value was never encrypted.
func TestDecryptClientCredentialReturnsPlaintextUnchanged(t *testing.T) {
	for _, value := range []string{"plain-token", "", "enc:v2:something-else", "enc:v1"} {
		got, errDecrypt := decryptClientCredentialValue(value)
		if errDecrypt != nil {
			t.Fatalf("decrypt(%q): %v", value, errDecrypt)
		}
		if got != value {
			t.Errorf("decrypt(%q) = %q, want the input unchanged", value, got)
		}
	}
}

// TestDecryptClientCredentialRejectsMalformedCiphertext covers the shape checks.
func TestDecryptClientCredentialRejectsMalformedCiphertext(t *testing.T) {
	validIV := base64.RawURLEncoding.EncodeToString(make([]byte, credentialIVLength))
	validTag := base64.RawURLEncoding.EncodeToString(make([]byte, credentialTagLength))
	shortIV := base64.RawURLEncoding.EncodeToString(make([]byte, credentialIVLength-4))
	shortTag := base64.RawURLEncoding.EncodeToString(make([]byte, credentialTagLength-8))
	data := base64.RawURLEncoding.EncodeToString([]byte("payload"))

	cases := []struct {
		name    string
		value   string
		wantSub string
	}{
		{"one segment", credentialPrefix + "only-one-part", "三段"},
		{"an empty segment", credentialPrefix + "a..c", "空段"},
		{"iv too short", credentialPrefix + shortIV + "." + validTag + "." + data, "IV 长度非法"},
		{"tag too short", credentialPrefix + validIV + "." + shortTag + "." + data, "AuthTag 长度非法"},
		{"iv not base64url", credentialPrefix + "!!!." + validTag + "." + data, "IV 不是 base64url"},
		{"tag not base64url", credentialPrefix + validIV + ".!!!." + data, "AuthTag 不是 base64url"},
		{"data not base64url", credentialPrefix + validIV + "." + validTag + ".!!!", "密文不是 base64url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errDecrypt := decryptClientCredentialValue(tc.value)
			if errDecrypt == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(errDecrypt.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to mention %q", errDecrypt.Error(), tc.wantSub)
			}
		})
	}
}

// TestDecryptClientCredentialFailsOnTamper is the GCM authentication case.
//
// ⚠ This is the one that proves the tag is actually verified. A round-trip test
// alone would still pass an implementation that used CTR mode or that ignored the
// tag entirely; this one cannot.
func TestDecryptClientCredentialFailsOnTamper(t *testing.T) {
	const secret = "tamper-secret"
	t.Setenv(credentialSecretEnv, secret)
	key := deriveTestKey(secret)
	encrypted := encryptClientValue(t, "original-payload", key)
	segments := strings.Split(strings.TrimPrefix(encrypted, credentialPrefix), ".")
	if len(segments) != 3 {
		t.Fatalf("ciphertext has %d segments, want 3", len(segments))
	}

	flip := func(segment string, index int) string {
		raw, errDecode := base64.RawURLEncoding.DecodeString(segment)
		if errDecode != nil {
			t.Fatalf("decode segment: %v", errDecode)
		}
		raw[index] ^= 0xff
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	cases := []struct {
		name  string
		value string
	}{
		{
			name:  "a flipped ciphertext byte",
			value: credentialPrefix + segments[0] + "." + segments[1] + "." + flip(segments[2], 0),
		},
		{
			name:  "a flipped auth-tag byte",
			value: credentialPrefix + segments[0] + "." + flip(segments[1], 0) + "." + segments[2],
		},
		{
			name:  "a flipped IV byte",
			value: credentialPrefix + flip(segments[0], 0) + "." + segments[1] + "." + segments[2],
		},
		{
			name:  "a truncated ciphertext",
			value: credentialPrefix + segments[0] + "." + segments[1] + "." + segments[2][:2],
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errDecrypt := decryptClientCredentialValue(tc.value)
			if errDecrypt == nil {
				t.Fatal("a tampered ciphertext decrypted successfully")
			}
			if !strings.Contains(errDecrypt.Error(), "解密失败") {
				t.Fatalf("error = %q, want the GCM authentication failure wording", errDecrypt.Error())
			}
		})
	}
}

// TestDecryptClientCredentialFailsUnderTheWrongKey pins the derivation's
// sensitivity: the key is a four-part string, and getting any part wrong produces
// exactly this failure.
func TestDecryptClientCredentialFailsUnderTheWrongKey(t *testing.T) {
	t.Setenv(credentialSecretEnv, "the-right-secret")
	encrypted := encryptClientValue(t, "payload", deriveTestKey("the-right-secret"))

	// Same value, a different secret: must not decrypt.
	t.Setenv(credentialSecretEnv, "a-different-secret")
	if _, errDecrypt := decryptClientCredentialValue(encrypted); errDecrypt == nil {
		t.Fatal("the value decrypted under a different secret, so the secret does not really participate")
	}

	// No secret at all: the fallback derivation must not accidentally match.
	t.Setenv(credentialSecretEnv, "")
	if _, errDecrypt := decryptClientCredentialValue(encrypted); errDecrypt == nil {
		t.Fatal("the value decrypted under the fallback derivation, which uses a different secret string")
	}
}

// TestDeriveClientCredentialKeyComposition pins the fallback secret.
//
// ⚠ The reference reproduces the official algorithm character for character and
// warns that "normalising" any part produces garbage. This test cannot recompute
// the secret with `user.Current()` inside, so it verifies the parts that ARE
// controllable: the environment override shape, and that the prefix is present in
// the derived key's preimage when it is rebuilt here.
func TestDeriveClientCredentialKeyComposition(t *testing.T) {
	t.Run("the environment override is exactly sha256 of its value", func(t *testing.T) {
		t.Setenv(credentialSecretEnv, "explicit-secret")
		got := deriveClientCredentialKey()
		want := deriveTestKey("explicit-secret")
		if len(got) != len(want) {
			t.Fatalf("key length = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("key byte %d differs; the override must be sha256(value) with nothing else mixed in", i)
			}
		}
	})

	t.Run("the fallback derives a 32-byte key", func(t *testing.T) {
		t.Setenv(credentialSecretEnv, "")
		key := deriveClientCredentialKey()
		if len(key) != sha256BlockSizeForTest {
			t.Fatalf("key length = %d, want %d", len(key), sha256BlockSizeForTest)
		}
		// The prefix is part of the preimage; rebuilding the secret by hand must
		// reproduce the same key, which is what pins every separator and ordering.
		rebuilt := deriveTestKey(rebuiltFallbackSecret())
		for i := range rebuilt {
			if key[i] != rebuilt[i] {
				t.Fatalf("key byte %d differs from a hand-built secret; the fallback composition changed", i)
			}
		}
	})
}

// sha256BlockSizeForTest is the digest length, spelled out so a change to the
// hash is a visible test failure rather than a silent behavioural change.
const sha256BlockSizeForTest = sha256.Size

// rebuiltFallbackSecret reconstructs the fallback secret with the same four parts
// the implementation uses, so any deviation in prefix, separator or ordering shows
// up as a key mismatch.
func rebuiltFallbackSecret() string {
	username := credentialUnknownUse
	if current, errUser := user.Current(); errUser == nil && strings.TrimSpace(current.Username) != "" {
		username = strings.TrimSpace(current.Username)
	}
	home := ""
	if resolved, errHome := os.UserHomeDir(); errHome == nil {
		home = resolved
	}
	return credentialKeyPrefix + ":" + credentialPlatform() + ":" + home + ":" + username
}

// TestCredentialPlatformUsesNodeSpelling guards the one platform where Go and
// Node disagree. The algorithm is defined by the official Node client, so `windows`
// in the key preimage is a GCM authentication failure waiting to happen.
func TestCredentialPlatformUsesNodeSpelling(t *testing.T) {
	got := credentialPlatform()
	switch runtime.GOOS {
	case "windows":
		if got != "win32" {
			t.Fatalf("credentialPlatform = %q, want Node's win32 spelling", got)
		}
	case "darwin", "linux":
		if got != runtime.GOOS {
			t.Fatalf("credentialPlatform = %q, want %q", got, runtime.GOOS)
		}
	default:
		if got != runtime.GOOS {
			t.Fatalf("credentialPlatform = %q, want the GOOS fallback %q", got, runtime.GOOS)
		}
	}
}

// ───────────────────────────── credential file discovery ─────────────────────────────

// TestCredentialFileCandidates covers the search order.
func TestCredentialFileCandidates(t *testing.T) {
	t.Run("the home directory is always a candidate", func(t *testing.T) {
		isolateClientStore(t)
		candidates := credentialFileCandidates("")
		home, errHome := os.UserHomeDir()
		if errHome != nil {
			t.Skipf("no home directory in this environment: %v", errHome)
		}
		want := filepath.Join(home, ".zcode", "v2", "credentials.json")
		found := false
		for _, candidate := range candidates {
			if candidate == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("candidates %v do not include the official default %q", candidates, want)
		}
	})

	t.Run("an explicit data root takes priority", func(t *testing.T) {
		candidates := credentialFileCandidates("/custom/root")
		if len(candidates) == 0 || candidates[0] != filepath.Join("/custom/root", ".zcode", "v2", "credentials.json") {
			t.Fatalf("candidates[0] = %q, want the explicit root", candidates[0])
		}
	})

	t.Run("the environment variable accepts a semicolon-separated list", func(t *testing.T) {
		t.Setenv("ZCODE_DATA_BASE_DIR", "/first;/second")
		candidates := credentialFileCandidates("")
		wantFirst := filepath.Join("/first", ".zcode", "v2", "credentials.json")
		wantSecond := filepath.Join("/second", ".zcode", "v2", "credentials.json")
		joined := strings.Join(candidates, "\n")
		if !strings.Contains(joined, wantFirst) || !strings.Contains(joined, wantSecond) {
			t.Fatalf("candidates %v do not include both environment entries", candidates)
		}
	})

	t.Run("duplicates are collapsed", func(t *testing.T) {
		t.Setenv("ZCODE_DATA_BASE_DIR", "/same")
		candidates := credentialFileCandidates("/same")
		seen := map[string]int{}
		for _, candidate := range candidates {
			seen[candidate]++
		}
		for candidate, count := range seen {
			if count > 1 {
				t.Fatalf("candidate %q appears %d times", candidate, count)
			}
		}
	})
}

// TestImportClientCredentialEndToEnd exercises the whole optional path against a
// store written to a temporary directory.
func TestImportClientCredentialEndToEnd(t *testing.T) {
	isolateClientStore(t)
	const secret = "import-secret"
	t.Setenv(credentialSecretEnv, secret)
	key := deriveTestKey(secret)

	dir := t.TempDir()
	clientDir := filepath.Join(dir, ".zcode", "v2")
	if errMkdir := os.MkdirAll(clientDir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	store := map[string]string{
		"some-uuid:zcodejwttoken":                   encryptClientValue(t, "the-jwt", key),
		"some-uuid:oauth:bigmodel:access_token":     encryptClientValue(t, "the-access-token", key),
		"some-uuid:oauth:bigmodel:user_info":        encryptClientValue(t, `{"phone":"13800138000"}`, key),
		"some-uuid:zai-individual-coding-plan":      encryptClientValue(t, "zai-key", key),
		"some-uuid:bigmodel-individual-coding-plan": encryptClientValue(t, "bigmodel-key", key),
		// A plaintext entry must survive too: the official decryptor returns it
		// unchanged, and users hand-edit these files.
		"plain-entry": "not-encrypted",
	}
	encoded, errMarshal := json.Marshal(store)
	if errMarshal != nil {
		t.Fatalf("marshal store: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(clientDir, "credentials.json"), encoded, 0o600); errWrite != nil {
		t.Fatalf("write store: %v", errWrite)
	}
	telemetry, _ := json.Marshal(map[string]any{"deviceMid": "official-device-mid"})
	if errWrite := os.WriteFile(filepath.Join(clientDir, "telemetry-state.json"), telemetry, 0o600); errWrite != nil {
		t.Fatalf("write telemetry: %v", errWrite)
	}

	credential, errImport := importClientCredential(dir)
	if errImport != nil {
		t.Fatalf("importClientCredential: %v", errImport)
	}
	if credential.ZCodeJWT != "the-jwt" {
		t.Errorf("zcode_jwt = %q, want the-jwt", credential.ZCodeJWT)
	}
	if credential.DeviceMid != "official-device-mid" {
		t.Errorf("device_mid = %q, want the client's own value", credential.DeviceMid)
	}
	if credential.BigModelAccessToken != "the-access-token" {
		t.Errorf("bigmodel_access_token = %q", credential.BigModelAccessToken)
	}
	if credential.CodingPlanKeyZai != "zai-key" || credential.CodingPlanKeyBigmodel != "bigmodel-key" {
		t.Errorf("coding-plan keys were not carried: %#v", credential)
	}
	if credential.AccountLabel != "尾号8000" {
		t.Errorf("account_label = %q, want the masked phone number", credential.AccountLabel)
	}
	if credential.Source != "ide" {
		t.Errorf("source = %q, want ide", credential.Source)
	}
	if !credential.Usable() {
		t.Error("the imported credential is not usable")
	}
}

// TestImportClientCredentialGeneratesADeviceId covers the telemetry-less case.
//
// The device id is required but not validated, so generating one keeps the import
// usable on a machine whose telemetry file is absent. The label says so, because a
// generated id is not the one the official client is reporting its usage under.
func TestImportClientCredentialGeneratesADeviceId(t *testing.T) {
	// The telemetry file is absent BECAUSE of this isolation; on a host that runs
	// the official client the fallback candidates would otherwise find a real one.
	isolateClientStore(t)
	t.Setenv(credentialSecretEnv, "no-telemetry-secret")
	key := deriveTestKey("no-telemetry-secret")

	dir := t.TempDir()
	clientDir := filepath.Join(dir, ".zcode", "v2")
	if errMkdir := os.MkdirAll(clientDir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	store := map[string]string{"k:zcodejwttoken": encryptClientValue(t, "the-jwt", key)}
	encoded, _ := json.Marshal(store)
	if errWrite := os.WriteFile(filepath.Join(clientDir, "credentials.json"), encoded, 0o600); errWrite != nil {
		t.Fatalf("write store: %v", errWrite)
	}

	credential, errImport := importClientCredential(dir)
	if errImport != nil {
		t.Fatalf("importClientCredential: %v", errImport)
	}
	if credential.DeviceMid == "" {
		t.Fatal("no device id was produced, so every billing call would answer 400 code 3001")
	}
	if !strings.Contains(credential.AccountLabel, "由插件生成") {
		t.Errorf("account_label = %q, want it to disclose the generated device id", credential.AccountLabel)
	}
}

// TestImportClientCredentialReportsWhyItFailed covers the failure messages: an
// import that silently does nothing is worse than one that explains itself.
func TestImportClientCredentialReportsWhyItFailed(t *testing.T) {
	t.Run("a missing file names the path it looked for", func(t *testing.T) {
		isolateClientStore(t)
		_, errImport := importClientCredential(t.TempDir())
		if errImport == nil {
			t.Fatal("expected an error for a missing store")
		}
		if !strings.Contains(errImport.Error(), "credentials.json") {
			t.Fatalf("error %q does not name the file", errImport.Error())
		}
	})

	t.Run("a store without the jwt fragment explains what is missing", func(t *testing.T) {
		isolateClientStore(t)
		t.Setenv(credentialSecretEnv, "irrelevant")
		dir := t.TempDir()
		clientDir := filepath.Join(dir, ".zcode", "v2")
		if errMkdir := os.MkdirAll(clientDir, 0o755); errMkdir != nil {
			t.Fatalf("mkdir: %v", errMkdir)
		}
		encoded, _ := json.Marshal(map[string]string{"unrelated-key": "unrelated-value"})
		if errWrite := os.WriteFile(filepath.Join(clientDir, "credentials.json"), encoded, 0o600); errWrite != nil {
			t.Fatalf("write store: %v", errWrite)
		}
		_, errImport := importClientCredential(dir)
		if errImport == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(errImport.Error(), keyFragmentJWT) {
			t.Fatalf("error %q does not name the key fragment", errImport.Error())
		}
	})

	t.Run("a malformed file is reported as such", func(t *testing.T) {
		isolateClientStore(t)
		dir := t.TempDir()
		clientDir := filepath.Join(dir, ".zcode", "v2")
		if errMkdir := os.MkdirAll(clientDir, 0o755); errMkdir != nil {
			t.Fatalf("mkdir: %v", errMkdir)
		}
		if errWrite := os.WriteFile(filepath.Join(clientDir, "credentials.json"), []byte("[1,2,3]"), 0o600); errWrite != nil {
			t.Fatalf("write store: %v", errWrite)
		}
		_, errImport := importClientCredential(dir)
		if errImport == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(errImport.Error(), "JSON") {
			t.Fatalf("error %q does not say the file is not JSON", errImport.Error())
		}
	})
}

// TestUserInfoLabel covers the display-label extraction.
func TestUserInfoLabel(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"phone is masked to the last four digits", `{"phone":"13800138000"}`, "尾号8000"},
		{"mobile works too", `{"mobile":"13900139000"}`, "尾号9000"},
		{"a name is used as-is", `{"name":"Alice"}`, "Alice"},
		{"an email is used as-is", `{"email":"a@b.c"}`, "a@b.c"},
		{"an over-long name is truncated to 24 characters", `{"name":"` + strings.Repeat("x", 40) + `"}`, strings.Repeat("x", 24)},
		{"an id falls back to its last six characters", `{"id":"abcdef123456"}`, "id:123456"},
		{"a short id is ignored", `{"id":"abc"}`, ""},
		{"not json", `nonsense`, ""},
		{"empty", ``, ""},
		{"no recognised field", `{"something":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := userInfoLabel(tc.raw); got != tc.want {
				t.Fatalf("userInfoLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPickClientCredentialSkipsUndecryptableEntries covers the tolerance rule: one
// bad key must not stop the others.
func TestPickClientCredentialSkipsUndecryptableEntries(t *testing.T) {
	t.Setenv(credentialSecretEnv, "pick-secret")
	key := deriveTestKey("pick-secret")
	values := map[string]string{
		"a:zcodejwttoken": credentialPrefix + "garbage.garbage.garbage",
		"b:zcodejwttoken": "",
		"c:zcodejwttoken": encryptClientValue(t, "the-good-one", key),
	}
	if got := pickClientCredential(values, keyFragmentJWT); got != "the-good-one" {
		t.Fatalf("pickClientCredential = %q, want the decryptable value", got)
	}
	if got := pickClientCredential(values, "not-present"); got != "" {
		t.Fatalf("pickClientCredential = %q, want an empty result for an unknown fragment", got)
	}
}

// TestDetectInstalledAppVersionFallsBackNeutral covers the version probe: it must
// never make the provider unavailable.
func TestDetectInstalledAppVersionFallsBackNeutral(t *testing.T) {
	isolateClientStore(t)
	t.Setenv("PROGRAMFILES", "")
	t.Setenv("PROGRAMFILES(X86)", "")
	t.Setenv("ZCODE_APP_VERSION", "")
	if got := detectInstalledAppVersion(); got != DefaultAppVersion {
		t.Fatalf("detectInstalledAppVersion = %q, want the built-in fallback %q", got, DefaultAppVersion)
	}
	t.Setenv("ZCODE_APP_VERSION", "9.9.9")
	if got := detectInstalledAppVersion(); got != "9.9.9" {
		t.Fatalf("detectInstalledAppVersion = %q, want the environment override", got)
	}
}
