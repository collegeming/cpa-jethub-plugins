package main

import (
	"encoding/json"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
)

// credentialRefresher is this provider's credential-freshness state: one
// cooldown and one single-flight per auth file, shared by the management page
// and the executor, so a page load and an inference request arriving together
// cost the broker ONE renewal.
//
// The single-flight matters more here than for the sibling providers. A refresh
// ROTATES the credential and immediately invalidates the access token it
// replaces, so two concurrent renewals of the same account would leave one of
// them holding a token that is already dead — and the loser's write would
// clobber the winner's refresh token, making the account unrefreshable until the
// user logs in again.
var credentialRefresher = newCredentialRefresher()

// newCredentialRefresher builds the freshness state. It is a function so a test
// can start from a clean slate instead of inheriting another test's cooldown.
func newCredentialRefresher() *authrefresh.Refresher {
	return authrefresh.New(authrefresh.Policy{
		Provider:    ProviderKey,
		LeadFunc:    func() time.Duration { return refreshLeadFor(settings()) },
		Expiry:      credentialExpiry,
		Refreshable: credentialRefreshable,
		// The plugin's own `auth.refresh` handler is the renewal: there is no
		// second implementation of the exchange to drift out of step.
		Refresh: authrefresh.Handler(handleAuthRefresh),
	})
}

// credentialExpiry reports the expiry the credential actually carries.
//
// An AtomGit access token is opaque, so `created_at + expires_in` is the only
// source. A credential whose record is incomplete (a hand-imported token, for
// instance) reports ok=false and is left alone rather than refreshed in a loop.
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

// credentialRefreshable reports whether this credential can be renewed without a
// new login.
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
