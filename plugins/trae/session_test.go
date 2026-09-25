package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSeededStreamAlgorithm pins the deterministic byte stream: it is
// SHA256("<salt>:<seed>" || counterBE32) concatenated until nbytes bytes exist
// (trae.ts:325-345). Recomputing it here keeps a future refactor from silently
// changing every derived device identity.
func TestSeededStreamAlgorithm(t *testing.T) {
	const seed = "user-1"
	const salt = "devid"
	const nbytes = 20
	got := seededStream(seed, salt, nbytes)
	if len(got) != nbytes {
		t.Fatalf("length = %d, want %d", len(got), nbytes)
	}

	expected := []byte{}
	prefix := []byte(salt + ":" + seed)
	for counter := uint32(0); len(expected) < nbytes; counter++ {
		var counterBytes [4]byte
		binary.BigEndian.PutUint32(counterBytes[:], counter)
		digest := sha256.New()
		digest.Write(prefix)
		digest.Write(counterBytes[:])
		for _, b := range digest.Sum(nil) {
			expected = append(expected, b)
			if len(expected) >= nbytes {
				break
			}
		}
	}
	if hex.EncodeToString(got) != hex.EncodeToString(expected[:nbytes]) {
		t.Fatalf("seededStream mismatch:\n got = %s\n want = %s",
			hex.EncodeToString(got), hex.EncodeToString(expected[:nbytes]))
	}
	// Deterministic across calls, and sensitive to both inputs.
	if hex.EncodeToString(seededStream(seed, salt, nbytes)) != hex.EncodeToString(got) {
		t.Fatal("seededStream must be deterministic")
	}
	if hex.EncodeToString(seededStream(seed, "market", nbytes)) == hex.EncodeToString(got) {
		t.Fatal("a different salt must produce different bytes")
	}
}

// TestDerivedCheckinIdentity checks the three deterministic per-account
// identities against their formats (trae.ts:296-317).
func TestDerivedCheckinIdentity(t *testing.T) {
	uid := "8847309959"
	device := deriveDeviceID15(uid)
	if len(device) != 15 {
		t.Fatalf("deriveDeviceID15 length = %d, want 15: %s", len(device), device)
	}
	if !isDigits(device) {
		t.Fatalf("deriveDeviceID15 = %q, want digits only", device)
	}
	if deriveDeviceID15("other") == device {
		t.Fatal("different accounts must get different device ids")
	}

	market := deriveMarketUserID(uid)
	if len(market) != 36 {
		t.Fatalf("market user id = %q", market)
	}
	parts := strings.Split(market, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("market user id shape = %q", market)
	}
	if parts[2][0] != '4' {
		t.Fatalf("market user id must be version 4: %q", market)
	}
	if !strings.ContainsRune("89ab", rune(parts[3][0])) {
		t.Fatalf("market user id must carry the RFC 4122 variant: %q", market)
	}
	if deriveMarketUserID("other") == market {
		t.Fatal("different accounts must get different market ids")
	}

	session := deriveSessionID(uid)
	if len(session) != 64 {
		t.Fatalf("session id length = %d, want 64: %s", len(session), session)
	}
	if _, err := hex.DecodeString(session); err != nil {
		t.Fatalf("session id must be hex: %v", err)
	}
}

// TestMachineTraceID: the login trace is the tail of machine+device.
func TestMachineTraceID(t *testing.T) {
	if got := machineTraceID("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"); got != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("trace = %q", got)
	}
	if got := machineTraceID("aa", "bb"); got != "000000000000aabb" {
		t.Fatalf("short trace must be left-padded, got %q", got)
	}
	if got := machineTraceID("0123456789abcdef", "fedcba9876543210"); got != "fedcba9876543210" {
		t.Fatalf("trace = %q, want the last 16 characters", got)
	}
}

// TestRotatingFingerprints: generation 0 is a no-op and every other generation
// stays a 32-hex value.
func TestRotatingFingerprints(t *testing.T) {
	base := "0123456789abcdef0123456789abcdef"
	if got := deriveRotatingMachineID(base, 0); got != base {
		t.Fatalf("generation 0 must return the base id, got %q", got)
	}
	if got := deriveCheckinDeviceID(base, -1); got != base {
		t.Fatalf("a negative generation must return the base id, got %q", got)
	}
	first := deriveRotatingMachineID(base, 1)
	second := deriveRotatingMachineID(base, 2)
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("rotated ids must be 32 hex, got %q / %q", first, second)
	}
	if first == second || first == base {
		t.Fatalf("rotated ids must differ: %q %q %q", base, first, second)
	}
	if got := deriveCheckinDeviceID(base, 1); len(got) != 32 || got == base {
		t.Fatalf("check-in rotation = %q", got)
	}
}

// TestRandomIdentifiers checks the formats the protocol requires.
func TestRandomIdentifiers(t *testing.T) {
	machine, errMachine := newMachineID()
	if errMachine != nil {
		t.Fatalf("machine id: %v", errMachine)
	}
	device, errDevice := newDeviceID()
	if errDevice != nil {
		t.Fatalf("device id: %v", errDevice)
	}
	if len(machine) != 32 || len(device) != 32 {
		t.Fatalf("machine/device ids must be 32 hex: %q %q", machine, device)
	}
	if machine == device {
		t.Fatal("two draws must differ")
	}
	state, errState := newState()
	if errState != nil || len(state) != 16 {
		t.Fatalf("state = %q err=%v, want 16 hex", state, errState)
	}
	uuid, errUUID := uuidV4()
	if errUUID != nil {
		t.Fatalf("uuid: %v", errUUID)
	}
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 {
		t.Fatalf("uuid = %q", uuid)
	}
	if uuid[14] != '4' {
		t.Fatalf("uuid version nibble = %q", uuid[14])
	}
}

// TestSoloHeaders covers the credential triple, the identity headers and the
// header-casing trap: net/http must never end up with two Content-Type entries.
func TestSoloHeaders(t *testing.T) {
	credential := &Credential{
		AccessToken: "tok",
		UID:         "uid-1",
		MachineID:   "0123456789abcdef0123456789abcdef",
		DeviceID:    "fedcba9876543210fedcba9876543210",
	}
	product := productFor(RegionCN)
	headers := soloHeaders(credential, product, true, 0)

	if got := headers.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, name := range []string{"X-Cloudide-Token", "X-Ide-Token"} {
		if got := headers.Get(name); got != "tok" {
			t.Fatalf("%s = %q, want the same token", name, got)
		}
	}
	if got := headers.Get("X-Uid"); got != "uid-1" {
		t.Fatalf("X-Uid = %q", got)
	}
	if got := headers.Get("X-Machine-Id"); got != credential.MachineID {
		t.Fatalf("X-Machine-Id = %q, want the stored id at generation 0", got)
	}
	if got := headers.Get("X-Device-Id"); got != credential.DeviceID {
		t.Fatalf("X-Device-Id = %q", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
	if len(headers.Values("Content-Type")) != 1 || headers.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %#v, want exactly one application/json", headers.Values("Content-Type"))
	}
	if got := headers.Get("X-Ide-Version"); got != product.IDEVersion {
		t.Fatalf("X-Ide-Version = %q", got)
	}
	if got := headers.Get("X-App-Version"); got != "default" {
		t.Fatalf("X-App-Version = %q", got)
	}

	// A non-streaming catalog request asks for JSON.
	if got := soloHeaders(credential, product, false, 0).Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	// Rotation changes the machine id but nothing else.
	rotated := soloHeaders(credential, product, true, 3).Get("X-Machine-Id")
	if rotated == credential.MachineID || len(rotated) != 32 {
		t.Fatalf("rotated X-Machine-Id = %q", rotated)
	}
	// Empty fingerprints are omitted rather than sent blank.
	bare := soloHeaders(&Credential{AccessToken: "tok"}, product, false, 0)
	if _, present := bare["X-Machine-Id"]; present {
		t.Fatal("X-Machine-Id must be omitted when machine_id is empty")
	}
	if _, present := bare["X-Device-Id"]; present {
		t.Fatal("X-Device-Id must be omitted when device_id is empty")
	}
}

// TestCheckinHeaders checks the full client header set and its derived device
// identity (trae.ts:253-286).
func TestCheckinHeaders(t *testing.T) {
	credential := &Credential{AccessToken: "tok", UID: "uid-checkin"}
	headers, errHeaders := checkinHeaders(credential, productFor(RegionCN), credential.UID)
	if errHeaders != nil {
		t.Fatalf("checkinHeaders: %v", errHeaders)
	}
	if got := headers.Get("X-Device-Id"); !isDigits(got) || len(got) != 15 {
		t.Fatalf("X-Device-Id = %q, want the 15-digit derived id", got)
	}
	if got := headers.Get("X-Market-User-Id"); len(got) != 36 {
		t.Fatalf("X-Market-User-Id = %q", got)
	}
	if got := headers.Get("Vscode-Sessionid"); len(got) != 64 {
		t.Fatalf("Vscode-Sessionid = %q", got)
	}
	if got := headers.Get("X-Request-Id"); len(got) != 36 {
		t.Fatalf("X-Request-Id = %q", got)
	}
	trace := headers.Get("X-Tt-Trace-Id")
	if !strings.HasPrefix(trace, "00-") || !strings.HasSuffix(trace, "-01") || len(trace) != 22 {
		t.Fatalf("X-Tt-Trace-Id = %q", trace)
	}
	if got := headers.Get("X-Lscbd-Aid"); got != "787976" {
		t.Fatalf("X-Lscbd-Aid = %q", got)
	}
	if got := headers.Get("Package-Type"); got != "stable_cn" {
		t.Fatalf("Package-Type = %q", got)
	}
	if got := headers.Get("Authorization"); got != "Cloud-IDE-JWT tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Accept-Encoding"); got != "gzip, deflate" {
		t.Fatalf("Accept-Encoding = %q", got)
	}
	if got := headers.Get("X-User-Region"); got != "CN" {
		t.Fatalf("X-User-Region = %q", got)
	}
	// Per-request randomness: two calls must not share request ids.
	other, errOther := checkinHeaders(credential, productFor(RegionCN), credential.UID)
	if errOther != nil {
		t.Fatalf("checkinHeaders: %v", errOther)
	}
	if other.Get("X-Request-Id") == headers.Get("X-Request-Id") {
		t.Fatal("X-Request-Id must be refreshed per request")
	}
	// The derived identity is stable per account.
	if other.Get("X-Device-Id") != headers.Get("X-Device-Id") {
		t.Fatal("the derived device id must be stable per account")
	}
}

// TestOAuthHeaders is the minimal ExchangeToken/GetUserInfo header set.
func TestOAuthHeaders(t *testing.T) {
	headers := oauthHeaders(productFor(RegionCN))
	if headers.Get("Content-Type") != "application/json" || headers.Get("Accept") != "application/json" {
		t.Fatalf("headers = %#v", headers)
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("the OAuth headers carry no token; GetUserInfo adds X-Cloudide-Token")
	}
}
