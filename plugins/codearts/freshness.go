package main

import (
	"encoding/json"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
)

// credentialRefresher is this provider's credential-freshness state: one
// cooldown and one single-flight per auth file, shared by the status page and
// the executor. A page load and an inference request that arrive together
// therefore cost the vendor ONE renewal, and a credential that cannot be
// renewed is not retried on every request.
//
// It exists because the host's own auto-refresh loop starts with the process
// and is reset by every restart — and every plugin deploy restarts the process.
// A token that expires inside such a window is never renewed by the host, which
// is exactly how the balance read ended up answering "the security token has
// expired" while the page still claimed 可自动续期.
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
		// second implementation of the STS exchange to drift out of step.
		Refresh: authrefresh.Handler(handleAuthRefresh),
	})
}

// credentialExpiry reports the expiry the credential actually carries.
//
// It deliberately does NOT use Credential.Expiry(), whose documented fallback
// is "24 hours from now" for an unparseable timestamp: a fabricated expiry
// would make an undatable credential look renewable-forever. No usable expiry
// means no action, which is what the helper does with ok=false.
func credentialExpiry(storage json.RawMessage) (time.Time, bool) {
	credential, errParse := ParseCredential(storage)
	if errParse != nil {
		return time.Time{}, false
	}
	return credential.ParsedExpiry()
}

// credentialRefreshable reports whether this credential can be renewed without
// a new login. A ticket-flow credential cannot, and is left alone.
func credentialRefreshable(storage json.RawMessage) bool {
	credential, errParse := ParseCredential(storage)
	return errParse == nil && credential.Refreshable()
}

// ensureCredentialFresh renews a credential before it is used, and returns the
// bytes to use.
//
// It is the helper's EnsureUsable: a renewal that failed only stops the caller
// when the credential is already past its expiry, because the honest reason for
// a failure is then the renewal failure — not a doomed upstream call that would
// report "the security token has expired".
func ensureCredentialFresh(h *abiboot.Host, request authrefresh.Request) (authrefresh.Result, error) {
	return credentialRefresher.EnsureUsable(h, request)
}
