package main

import (
	"encoding/json"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
)

// credentialRefresher is this provider's credential-freshness state: one
// cooldown and one single-flight per auth file, shared by the status page, the
// quota route and the executor, so a page load and an inference request
// arriving together cost the vendor ONE renewal.
//
// The host's own auto-refresh loop starts with the process and is reset by
// every restart, and every plugin deploy restarts the process: a token that
// expires inside such a window is never renewed by the host, so the plugin
// renews it itself, before it is used.
var credentialRefresher = newCredentialRefresher()

// newCredentialRefresher builds the freshness state. It is a function so a test
// can start from a clean slate instead of inheriting another test's cooldown.
func newCredentialRefresher() *authrefresh.Refresher {
	return authrefresh.New(authrefresh.Policy{
		Provider:    ProviderKey,
		Lead:        refreshLead,
		Expiry:      credentialExpiry,
		Refreshable: credentialRefreshable,
		// The provider's own `auth.refresh` handler is the renewal: there is no
		// second implementation of the token exchange to drift out of step.
		Refresh: authrefresh.Handler(handleAuthRefresh),
	})
}

// credentialExpiry reports the expiry the credential actually carries.
//
// ExpiresAtMS already resolves both sources the reference uses — the stored
// `expires_at` and the access token's `exp` claim — and says "no expiry" with
// ok=false when neither yields one, which is what leaves such a credential
// alone.
func credentialExpiry(storage json.RawMessage) (time.Time, bool) {
	credential, errParse := ParseCredential(storage)
	if errParse != nil {
		return time.Time{}, false
	}
	millis, ok := credential.ExpiresAtMS()
	if !ok || millis <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(millis), true
}

// credentialRefreshable reports whether this credential can be renewed without
// a new login.
func credentialRefreshable(storage json.RawMessage) bool {
	credential, errParse := ParseCredential(storage)
	return errParse == nil && credential.Refreshable()
}

// ensureCredentialFresh renews a credential before it is used, and returns the
// bytes to use. A renewal that failed only stops the caller when the credential
// is already past its expiry; a transport failure is never reported as an
// expired credential.
func ensureCredentialFresh(h *abiboot.Host, request authrefresh.Request) (authrefresh.Result, error) {
	return credentialRefresher.EnsureUsable(h, request)
}
