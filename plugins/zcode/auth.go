package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credentialHealthInterval is the re-probe cadence for a credential that cannot
// be renewed. The reference re-checks a static credential on a 30-minute health
// cadence rather than trusting the local clock.
const credentialHealthInterval = 30 * time.Minute

// handleAuthIdentifier advertises the provider key this plugin owns.
func handleAuthIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleAuthParse recognises an auth file already present in the auth directory.
func handleAuthParse(_ *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthParseRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	if request.Provider != "" && request.Provider != ProviderKey {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	credential, errParse := ParseCredential(request.RawJSON)
	if errParse != nil {
		// Not one of ours: let the host try other providers.
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	auth, errAuth := authDataFor(credential, authNameForHost(request.FileName, request.Path, "", credential))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// handleAuthRefresh PROBES the credential; it does not renew anything.
//
// ⚠ ZCode has NO refresh endpoint. The JWT's payload carries no `exp` claim
// (measured: `{user_id, token_version, sub, iat}`), the credential is static, and
// the reference's `isZcodeExpired` returns `false` unconditionally — it is an
// honest marker rather than an oversight (`zcode.ts:440-460`). What this handler
// does instead is a cheap read-only probe so the host still learns whether the
// credential is alive:
//
//   - probe succeeds -> the SAME credential comes back and the next probe is
//     scheduled;
//   - probe says the credential is dead -> 401, so the host retires it and the
//     user is asked to log in again;
//   - transport failure or any other business failure -> a retryable 502, never
//     an expiry: a flaky network must not retire a working account.
func handleAuthRefresh(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.AuthRefreshRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errParse := ParseCredential(request.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	cfg := settings()
	if errProbe := probeCredential(h, credential, cfg); errProbe != nil {
		return nil, errProbe
	}
	auth, errAuth := authDataFor(credential, authNameForHost(request.AuthID, request.Attributes["path"], request.Attributes["source"], credential))
	if errAuth != nil {
		return nil, errAuth
	}
	return pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: nextProbeAfter(time.Now(), cfg),
	}, nil
}

// probeCredential performs one read-only validity check through the balance
// endpoint, which is the cheapest call that exercises both the bearer token and
// the device id.
func probeCredential(h *abiboot.Host, credential *Credential, cfg Config) error {
	_, errBalance := fetchBalance(h, credential, cfg)
	return errBalance
}

// nextProbeAfter decides when the host should probe again.
//
// There is nothing to count down to — the credential carries no local expiry —
// so the probe simply runs on the reference's 30-minute health cadence. A
// zero time would mean "never", which would leave a credential that died
// server-side unnoticed until a request failed.
func nextProbeAfter(now time.Time, _ Config) time.Time {
	return now.Add(credentialHealthInterval)
}

// authNameForHost resolves the auth file name the host already uses for this
// credential: the name it supplies on `auth.parse` (the file it read the
// credential from) or on the probe the host calls `auth.refresh` (the auth record
// id, which for a file-backed credential is that same file name), and failing
// both, the `path`/`source` attribute naming that file.
//
// Deriving a name is the brand-new-login case only. A derived name has to be
// stable across logins, which is exactly why it walks down to `user_id` and never
// to `device_mid`: the device id is regenerated on every login, so a name built
// on it would put a second file next to the first one for the same account.
func authNameForHost(incoming, path, source string, credential *Credential) string {
	return authfile.Name(func() string { return defaultAuthFileName(credential) }, incoming, path, source)
}

// defaultAuthFileName derives a stable auth-file name for an account.
func defaultAuthFileName(credential *Credential) string {
	identity := ""
	if credential != nil {
		identity = credential.UserID
		if identity == "" {
			identity = credential.AccountLabel
		}
	}
	identity = sanitizeFileName(identity)
	if identity == "" {
		identity = "account"
	}
	if len(identity) > 48 {
		identity = identity[:48]
	}
	return ProviderKey + "-" + identity + ".json"
}

// authDataFor converts a credential into the host-facing AuthData record.
func authDataFor(credential *Credential, fileName string) (pluginapi.AuthData, error) {
	storage, errEncode := credential.Encode()
	if errEncode != nil {
		return pluginapi.AuthData{}, errEncode
	}
	if fileName == "" {
		fileName = defaultAuthFileName(credential)
	}
	label := credential.displayLabel()
	// ⚠ The prefix must NOT be derived from `device_mid`. The reference records a
	// real defect from exactly that: a prefix built on a value that changes per
	// login makes one account look like several, and model ids stop matching the
	// ones the user already selected. `user_id` is stable, so when it is absent
	// there is simply no prefix.
	prefix := credential.UserID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	if !settings().ModelPrefix {
		prefix = ""
	}

	metadata := map[string]any{"refreshable": false}
	if credential.UserID != "" {
		metadata["user_id"] = credential.UserID
	}
	if credential.Source != "" {
		metadata["source"] = credential.Source
	}
	if credential.AppVersion != "" {
		metadata["app_version"] = credential.AppVersion
	}

	return pluginapi.AuthData{
		Provider:    ProviderKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: storage,
		Metadata:    metadata,
		Attributes: map[string]string{
			"account":     label,
			"credential":  ProviderKey,
			"refreshable": "false",
			"source":      credential.Source,
		},
		// No NextRefreshAfter: the credential never expires locally, and the
		// host's own timer would only produce a probe it does not need. The
		// management page probes on demand instead.
		NextRefreshAfter: time.Time{},
	}, nil
}

// sanitizeFileName strips anything that cannot appear in an auth file name.
func sanitizeFileName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '_' || r == '@' || r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

// boolString renders a boolean for an attribute bag.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
