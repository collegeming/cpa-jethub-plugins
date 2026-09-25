package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// This file ports the deterministic identity derivations of trae.ts:288-372.
//
// Two families of identifiers exist:
//
//   - login-time random fingerprints: `machine_id` and `device_id`, both 32 hex
//     characters (16 random bytes, trae.ts:1096-1118). They are generated once
//     per login and persisted. `machine_id` must never be regenerated on refresh
//     and `device_id` must differ per account (docs/agents/trae.md:35-37).
//   - per-account *derived* identities for the check-in endpoint: a 15-digit
//     device id, a UUID-v4 market user id and a 64-hex session id, all derived
//     from the account's uid so that one account always presents the same device
//     and different accounts never collide (trae.ts:288-353).

// randomHexBytes returns a lower-case hex string of 2*n characters drawn from
// crypto/rand. trae.ts:365-372 generates `ceil(n/2)` random bytes and slices the
// hex down to n characters; every caller here wants whole bytes, so the helper
// takes a byte count instead of a character count to keep the two call sites
// (32-hex machine/device ids) unambiguous.
func randomHexBytes(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// newMachineID returns a 32-hex device fingerprint (trae.ts:1096-1100).
func newMachineID() (string, error) { return randomHexBytes(16) }

// newDeviceID returns a 32-hex check-in device number (trae.ts:1114-1118).
// `login.sh:34` generates it with `openssl rand -hex 16`.
func newDeviceID() (string, error) { return randomHexBytes(16) }

// newState returns an opaque 16-hex poll handle for one login session.
func newState() (string, error) { return randomHexBytes(8) }

// machineTraceID derives the login URL's `login_trace_id` from the machine and
// device fingerprints (trae-oauth.ts:71-76): the last 16 characters of their
// concatenation, zero-padded on the left when too short. The callback carries it
// back, which is how a callback is associated with a pending login.
func machineTraceID(machineID, deviceID string) string {
	joined := machineID + deviceID
	if len(joined) >= 16 {
		return joined[len(joined)-16:]
	}
	return strings.Repeat("0", 16-len(joined)) + joined
}

// seededStream is the SHA-256 based deterministic byte stream of
// trae.ts:325-345: `SHA256(utf8("<salt>:<seed>") || counterBE32)` repeated until
// nbytes bytes have been produced.
func seededStream(seed, salt string, nbytes int) []byte {
	if nbytes <= 0 {
		return nil
	}
	prefix := []byte(salt + ":" + seed)
	out := make([]byte, 0, nbytes)
	var counter [4]byte
	for counterIndex := uint32(0); len(out) < nbytes; counterIndex++ {
		binary.BigEndian.PutUint32(counter[:], counterIndex)
		digest := sha256.New()
		digest.Write(prefix)
		digest.Write(counter[:])
		for _, b := range digest.Sum(nil) {
			out = append(out, b)
			if len(out) >= nbytes {
				break
			}
		}
	}
	return out[:nbytes]
}

// seededDigits renders n digits, each byte taken modulo 10 (trae.ts:350-353).
func seededDigits(n int, seed, salt string) string {
	bytes := seededStream(seed, salt, n)
	var builder strings.Builder
	builder.Grow(n)
	for _, b := range bytes {
		builder.WriteByte(byte('0' + b%10))
	}
	return builder.String()
}

// deriveDeviceID15 is the 15-digit check-in device id (trae.ts:296-298).
func deriveDeviceID15(uid string) string { return seededDigits(15, uid, "devid") }

// deriveMarketUserID is a deterministic UUID v4 built from the uid
// (trae.ts:303-309).
func deriveMarketUserID(uid string) string {
	bytes := seededStream(uid, "market", 16)
	bytes[6] = (bytes[6] & 0x0F) | 0x40 // version 4
	bytes[8] = (bytes[8] & 0x3F) | 0x80 // variant RFC 4122
	hexed := hex.EncodeToString(bytes)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// deriveSessionID is the 64-hex VS Code session id (trae.ts:314-317).
func deriveSessionID(uid string) string {
	return hex.EncodeToString(seededStream(uid, "sess", 32))
}

// deriveCheckinDeviceID rotates the check-in device number by generation
// (trae.ts:1147-1153). Generation <= 0 returns the base id untouched.
//
// Retained for compatibility with existing accounts only: the current check-in
// path derives its device identity from the uid (deriveDeviceID15), so this is
// not used when claiming (trae.ts:265-276).
func deriveCheckinDeviceID(baseDeviceID string, generation int) string {
	if generation <= 0 {
		return baseDeviceID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#gen%d", baseDeviceID, generation)))
	return hex.EncodeToString(sum[:])[:32]
}

// deriveRotatingMachineID rotates the machine fingerprint by generation
// (trae.ts:1174-1180). Generation <= 0 returns the base id untouched.
//
// Rotation is off by default: the device identity is meant to stay stable, and
// only a cluster of 401s justifies trying this switch
// (docs/agents/trae.md:520-529).
func deriveRotatingMachineID(baseMachineID string, generation int) string {
	if generation <= 0 {
		return baseMachineID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#machine%d", baseMachineID, generation)))
	return hex.EncodeToString(sum[:])[:32]
}

// uuidV4 returns a random RFC 4122 version 4 UUID (trae.ts:358-363).
func uuidV4() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0F) | 0x40
	bytes[8] = (bytes[8] & 0x3F) | 0x80
	hexed := hex.EncodeToString(bytes)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32], nil
}
