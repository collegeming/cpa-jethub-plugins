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
// renews it itself, before it is used. The executor's own "refresh once, retry
// once" path stays as the last line of defence.
var credentialRefresher = newCredentialRefresher()

// newCredentialRefresher builds the freshness state. It is a function so a test
// can start from a clean slate instead of inheriting another test's cooldown.
func newCredentialRefresher() *authrefresh.Refresher {
	return authrefresh.New(authrefresh.Policy{
		Provider: ProviderKey,
		// The lead is read live: it is the same configured window the provider
		// reports to the host through NextRefreshAfter, so the plugin and the
		// host agree on "due" even after a settings change.
		LeadFunc:    func() time.Duration { return refreshWindow(settings()) },
		Expiry:      credentialExpiry,
		Refreshable: credentialRefreshable,
		// The provider's own `auth.refresh` handler is the renewal: there is no
		// second implementation of the OAuth refresh exchange to drift.
		Refresh: authrefresh.Handler(handleAuthRefresh),
	})
}

// credentialExpiry reports the expiry the credential actually carries.
//
// Expiry() resolves `expires_at` first and the access token's `exp` claim
// second, and that JWT fallback is load-bearing here: reading only `expires_at`
// leaves old or hand-imported credentials permanently "not expiring", so their
// renewal would never fire. A credential with neither source is left alone.
func credentialExpiry(storage json.RawMessage) (time.Time, bool) {
	credential, errParse := ParseCredential(storage)
	if errParse != nil {
		return time.Time{}, false
	}
	expiry := credential.Expiry()
	if expiry.IsZero() {
		return time.Time{}, false
	}
	return expiry, true
}

// credentialRefreshable reports whether this credential can be renewed without
// a new login. The provider's `auth.refresh` returns an unrefreshable
// credential unchanged on purpose, so such a credential is never sent here.
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
