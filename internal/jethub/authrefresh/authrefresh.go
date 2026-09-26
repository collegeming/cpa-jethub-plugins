// Package authrefresh keeps a provider credential usable without depending on
// the host's own refresh loop.
//
// CPA does run an auto-refresh loop, but it starts with the process
// (`sdk/cliproxy/service_lifecycle.go`: StartAutoRefresh(ctx, 15*time.Minute))
// and every restart resets its clock — and every plugin deploy restarts the
// process. A credential that expires inside a churny window is therefore never
// refreshed by the host: the first tick lands after the token is already dead,
// while requests that arrive in between keep using the expired bytes. That is
// exactly the state the CodeArts status page used to be in — the balance read
// answered "the security token has expired" while the same card still claimed
// 可自动续期, because the host timer that would have renewed it never fired.
//
// The remedy belongs in the plugin, because the plugin is the layer that is
// asked to *use* the credential. Before a credential is used — to read a quota,
// to sign an inference call — the plugin checks the credential's own expiry and,
// when it is expired or close to expiring, renews it through the provider's own
// `auth.refresh` handler and writes the result back to the auth file the host
// already knows.
//
// Four rules shape the behaviour:
//
//   - no usable expiry means no action. A credential that carries no parsable
//     expiry is left exactly as it is; inventing one would renew it forever.
//   - a short cooldown bounds the attempt rate, and a per-credential
//     single-flight makes concurrent callers share one vendor call, so a page
//     load plus an inference request cannot stampede the refresh endpoint.
//   - a terminal failure (the vendor saying the renewal grant is dead — the
//     401/403 the provider handlers already attach to it) marks the credential
//     dead: no further attempt until its bytes change. A transport failure is
//     never terminal, and is never reported as an expired credential.
//   - the refreshed credential is persisted through host.auth.save under the
//     same file name, so a refresh can neither mint a second auth file nor lose
//     the one the host is refreshing.
package authrefresh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authfile"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// DefaultCooldown is the gap enforced between two refresh attempts on the same
// credential. It is deliberately longer than a page load: the status page and
// the executor both call Ensure, and a credential that cannot be renewed must
// not turn every request into another vendor call.
const DefaultCooldown = 5 * time.Minute

// RefreshFunc performs one renewal for a provider. It is the provider's own
// `auth.refresh` handler, adapted with Handler, so the in-plugin path and the
// host-driven path can never drift apart.
type RefreshFunc func(h *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error)

// Policy is one provider's credential-freshness rules.
type Policy struct {
	// Provider is the provider key, used in log lines and in the refresh
	// request the handler receives.
	Provider string
	// Lead is how long before its expiry a credential is renewed. It is the
	// same window the provider already reports to the host through
	// NextRefreshAfter.
	Lead time.Duration
	// LeadFunc, when set, is read on every check instead of Lead. A provider
	// whose lead window is a live setting uses it, so a settings change takes
	// effect without rebuilding the refresher (and without racing a request).
	LeadFunc func() time.Duration
	// Cooldown is the minimum gap between two attempts on one credential.
	// Zero means DefaultCooldown.
	Cooldown time.Duration
	// Expiry reads the credential's own expiry. ok is false when the credential
	// carries no usable one; such a credential is never refreshed. It must not
	// invent a fallback: a fabricated expiry is a refresh loop.
	Expiry func(storage json.RawMessage) (expiry time.Time, ok bool)
	// Refreshable reports whether the credential can be renewed at all — a
	// ticket-flow credential without a refresh token cannot, and is left alone.
	// Nil means "renewable whenever Expiry says the credential is due".
	Refreshable func(storage json.RawMessage) bool
	// Refresh renews the credential. Nil means this provider has no refresh
	// path at all (Loomy's `auth.refresh` is a validity probe): Ensure then
	// never touches the vendor.
	Refresh RefreshFunc
}

// Request names one credential and carries the bytes the host handed over.
type Request struct {
	// Name is the auth file name the host already knows for this credential:
	// the `FileName` of `auth.parse`, the record id of an executor request, or
	// the entry name of a listing. It is both the identity used for
	// cooldown/single-flight and the name the refreshed credential is written
	// back to. An empty name falls back to the name the provider derives.
	Name string
	// StorageJSON is the credential exactly as the host supplied it.
	StorageJSON json.RawMessage
	// Attributes carries the credential's host attributes (`path`, `source`)
	// so the provider can still resolve a name when Name is empty.
	Attributes map[string]string
}

// Result reports what Ensure did and which bytes the caller should use.
type Result struct {
	// Storage is the credential to use: the refreshed bytes when Refreshed is
	// true, the caller's own bytes otherwise. It is never empty when the caller
	// supplied a credential.
	Storage json.RawMessage
	// Refreshed reports that the vendor issued a new credential and it was
	// written back through host.auth.save.
	Refreshed bool
	// Attempted reports that a refresh was tried — by this call, or by the
	// in-flight call it joined. It is false when the credential was left alone
	// or when the cooldown suppressed the attempt.
	Attempted bool
	// Expired reports that Storage is already past its own expiry. A caller
	// that also received an error must not use it.
	Expired bool
	// Dead reports that the provider called this credential's renewal terminal.
	// No further attempt is made until the credential bytes change.
	Dead bool
	// Err is the renewal failure, or nil. It is the same value Ensure returns,
	// carried here so a caller that keeps the result — a status page, for
	// instance — can report what happened without a second field.
	Err error
}

// Refresher applies one Policy. It holds the per-credential cooldown and
// single-flight state, so a provider creates exactly one (package level) and
// shares it between its page and its executor.
type Refresher struct {
	policy Policy
	now    func() time.Time

	mu     sync.Mutex
	states map[string]*state
}

// state is the bookkeeping for one credential.
type state struct {
	// inflight is the refresh every concurrent caller waits on.
	inflight *call
	// lastAttempt is when the last refresh finished, successfully or not.
	lastAttempt time.Time
	// lastErr is that attempt's failure, or nil after a success.
	lastErr error
	// dead marks a terminal failure, and deadFingerprint remembers the bytes
	// that failed so a re-login (new bytes) revives the credential.
	dead            bool
	deadFingerprint string
}

// call is one in-flight refresh; waiters read its result once done is closed.
type call struct {
	done   chan struct{}
	result Result
	err    error
}

// New returns a Refresher for one provider.
func New(policy Policy) *Refresher {
	if policy.Cooldown <= 0 {
		policy.Cooldown = DefaultCooldown
	}
	return &Refresher{policy: policy, now: time.Now, states: map[string]*state{}}
}

// Ensure makes sure a credential is still usable before a caller uses it, and
// returns the bytes to use.
//
// It fails only when a refresh was needed and did not produce a usable
// credential; the error is the provider's own, so its message and its HTTP
// status stay exactly as the provider classified them. Result.Expired says
// whether those bytes are past their expiry anyway, which is how a caller tells
// "renewal failed but the token still works" from "this credential is dead
// until the user logs in again". The failure is also carried in Result.Err, so
// a caller that keeps the result can render it later.
func (r *Refresher) Ensure(h *abiboot.Host, request Request) (Result, error) {
	result := Result{Storage: request.StorageJSON}
	if len(request.StorageJSON) == 0 {
		return result, nil
	}

	expiry, hasExpiry := r.expiryOf(request.StorageJSON)
	if !hasExpiry {
		// Without a usable expiry there is nothing to count down to, and
		// refreshing on every use would hammer the vendor for no reason.
		return result, nil
	}
	now := r.now()
	result.Expired = !expiry.After(now)
	if !result.Expired && now.Add(r.lead()).Before(expiry) {
		// Comfortably valid: this is the common case and it must not even take
		// the lock, let alone touch the network.
		return result, nil
	}
	if r.policy.Refresh == nil {
		// No refresh path exists for this provider (Loomy's probe-only
		// `auth.refresh`). The credential is left alone; the caller keeps its
		// own honest "cannot renew" handling.
		return result, nil
	}
	if r.policy.Refreshable != nil && !r.policy.Refreshable(request.StorageJSON) {
		// Nothing to renew with: a refresh would only fail.
		return result, nil
	}
	if h == nil {
		// Without a host handle there is no transport to renew through and no
		// way to persist the result, so the freshness check stands aside instead
		// of turning a missing context into a credential failure. Providers that
		// have their own renewal fallback still run it.
		return result, nil
	}

	key := r.keyFor(request)
	fingerprint := fingerprintOf(request.StorageJSON)

	r.mu.Lock()
	entry := r.stateLocked(key)
	if entry.dead && entry.deadFingerprint != fingerprint {
		// The bytes changed since the terminal failure, so this is a newly
		// authorised credential: the old verdict does not apply to it.
		entry.dead, entry.deadFingerprint = false, ""
	}
	switch {
	case entry.dead:
		err := entry.lastErr
		r.mu.Unlock()
		result.Dead = true
		return fail(result, err)
	case entry.inflight != nil:
		// Single-flight: join the refresh already running for this credential
		// instead of starting a second one against the vendor.
		inflight := entry.inflight
		r.mu.Unlock()
		<-inflight.done
		return inflight.result, inflight.err
	case !entry.lastAttempt.IsZero() && now.Sub(entry.lastAttempt) < r.policy.Cooldown:
		// Backing off: report the recorded failure rather than retrying, so the
		// page keeps saying the truth while the vendor is left in peace.
		err := entry.lastErr
		r.mu.Unlock()
		return fail(result, err)
	}
	inflight := &call{done: make(chan struct{})}
	entry.inflight = inflight
	r.mu.Unlock()

	renewed, errRenew := r.renew(h, request, result)
	renewed, errRenew = fail(renewed, errRenew)
	r.finish(key, fingerprint, inflight, renewed, errRenew)
	return renewed, errRenew
}

// fail stamps the failure onto the result the caller will keep.
func fail(result Result, err error) (Result, error) {
	result.Err = err
	return result, err
}

// EnsureUsable is Ensure plus the rule every caller that is about to USE a
// credential follows.
//
// A renewal that failed only stops the caller when the credential is already
// past its expiry: the honest reason for a failure is then the renewal failure,
// never a doomed upstream call reporting "the security token has expired". A
// credential that is merely inside the lead window is still usable, so a
// transport failure must not fail the request — it is logged, and the caller
// proceeds with the credential it already had.
func (r *Refresher) EnsureUsable(h *abiboot.Host, request Request) (Result, error) {
	result, errRefresh := r.Ensure(h, request)
	if errRefresh == nil {
		return result, nil
	}
	if result.Expired {
		return result, errRefresh
	}
	if h != nil {
		h.Log("warn", fmt.Sprintf("%s 凭据续期失败，继续使用尚未过期的凭据", r.policy.Provider), map[string]any{
			"auth":  request.Name,
			"error": errRefresh.Error(),
		})
	}
	return result, nil
}

// renew performs the vendor call and the write-back. It runs on the single
// caller that won the single-flight.
func (r *Refresher) renew(h *abiboot.Host, request Request, result Result) (Result, error) {
	result.Attempted = true
	response, errRefresh := r.policy.Refresh(h, pluginapi.AuthRefreshRequest{
		AuthID:       request.Name,
		AuthProvider: r.policy.Provider,
		StorageJSON:  request.StorageJSON,
		Attributes:   request.Attributes,
	})
	if errRefresh != nil {
		result.Dead = IsTerminal(errRefresh)
		// A transport failure is logged as a failure, never as an expired
		// credential.
		h.Log(refreshFailureLevel(result.Dead), fmt.Sprintf("%s 自动续期失败", r.policy.Provider), map[string]any{
			"auth":  request.Name,
			"dead":  result.Dead,
			"error": errRefresh.Error(),
		})
		return result, errRefresh
	}

	refreshed := response.Auth.StorageJSON
	if len(refreshed) == 0 {
		err := abiboot.Errorf("refresh_empty", "%s 自动续期没有返回新凭据", r.policy.Provider)
		return result, err
	}
	// Persist through the one rule every provider follows: the refreshed
	// credential replaces the file the host already knows. A name the plugin
	// invented would leave the host's credential untouched and grow a second
	// entry for the same account.
	name := authfile.Name(nil, request.Name, response.Auth.FileName, response.Auth.ID)
	if name == "" {
		return result, abiboot.Errorf("refresh_unnamed", "%s 自动续期无法确定凭据文件名，已放弃写回", r.policy.Provider)
	}
	if responseName := authfile.Name(nil, response.Auth.FileName); responseName != "" && !strings.EqualFold(responseName, name) {
		// The provider derived a different name than the one the host knows.
		// The host's name wins — writing to the derived one is the bug that
		// used to mint a second auth file per refresh cycle.
		h.Log("warn", fmt.Sprintf("%s 自动续期的文件名与宿主已知的不一致，按宿主文件名写回", r.policy.Provider), map[string]any{
			"host_name":     name,
			"provider_name": responseName,
		})
	}
	if _, errSave := h.SaveAuth(name, refreshed); errSave != nil {
		// The new bytes are usable for this caller's own request, but they are
		// not persisted: say so instead of pretending the credential was
		// renewed.
		result.Storage = refreshed
		return result, abiboot.Errorf("refresh_save", "%s 自动续期写回失败：%v", r.policy.Provider, errSave)
	}

	result.Storage = refreshed
	result.Refreshed = true
	expiresAt := time.Time{}
	if expiry, ok := r.expiryOf(refreshed); ok {
		result.Expired = !expiry.After(r.now())
		expiresAt = expiry
	} else {
		// The vendor answered without a usable expiry: the server is now the
		// only authority on this credential, exactly as a no-expiry credential
		// is treated everywhere else.
		result.Expired = false
	}
	// The credential itself is never logged: it is a bearer secret. Only what
	// the renewal changed is.
	h.Log("info", fmt.Sprintf("%s 凭据已自动续期", r.policy.Provider), map[string]any{
		"auth":       name,
		"expires_at": expiresAt.Format(time.RFC3339),
		"expired":    result.Expired,
	})
	return result, nil
}

// finish records the outcome and releases the waiters of the single-flight.
func (r *Refresher) finish(key, fingerprint string, inflight *call, result Result, err error) {
	r.mu.Lock()
	entry := r.stateLocked(key)
	entry.inflight = nil
	entry.lastAttempt = r.now()
	entry.lastErr = err
	if err != nil && IsTerminal(err) {
		entry.dead = true
		entry.deadFingerprint = fingerprint
	}
	r.mu.Unlock()

	inflight.result, inflight.err = result, err
	close(inflight.done)
}

// stateLocked returns the bookkeeping for a key, creating it on first use. The
// caller must hold the mutex.
func (r *Refresher) stateLocked(key string) *state {
	entry, ok := r.states[key]
	if !ok {
		entry = &state{}
		r.states[key] = entry
	}
	return entry
}

// lead is the pre-expiry window in force right now.
func (r *Refresher) lead() time.Duration {
	if r.policy.LeadFunc != nil {
		return r.policy.LeadFunc()
	}
	return r.policy.Lead
}

// expiryOf applies the policy's expiry reader.
func (r *Refresher) expiryOf(storage json.RawMessage) (time.Time, bool) {
	if r.policy.Expiry == nil {
		return time.Time{}, false
	}
	return r.policy.Expiry(storage)
}

// keyFor is the identity a credential is tracked under: the auth file name the
// host knows, or — when the provider could not name it — the credential's own
// bytes, which still keep two different credentials apart.
func (r *Refresher) keyFor(request Request) string {
	if name := authfile.Name(nil, request.Name); name != "" {
		return name
	}
	return "bytes:" + fingerprintOf(request.StorageJSON)
}

// fingerprintOf identifies one credential's bytes, so a re-login can be told
// apart from the credential that failed terminally.
func fingerprintOf(storage json.RawMessage) string {
	sum := sha256.Sum256(storage)
	return hex.EncodeToString(sum[:8])
}

// refreshFailureLevel keeps a transport failure visible without dressing it up
// as a dead credential.
func refreshFailureLevel(dead bool) string {
	if dead {
		return "error"
	}
	return "warn"
}

// IsTerminal reports whether a refresh failure ends the credential's life.
//
// The repo's providers already make that call: a renewal grant that is dead is
// answered with a 401/403 (and a `refresh_token_expired`/`session_dead`-style
// code), while a transport failure, a 5xx or a 429 is not. This function only
// reads that existing classification — it never invents one, so a transport
// failure can never be reported as an expired credential.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	var envelope *abiboot.EnvelopeError
	if !errors.As(err, &envelope) || envelope == nil {
		return false
	}
	if envelope.Retryable {
		// An explicit "retry me" marker always wins over a status code.
		return false
	}
	switch envelope.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	switch strings.ToLower(strings.TrimSpace(envelope.Code)) {
	case "refresh_token_expired", "refresh_token_invalid", "invalid_grant", "session_dead", "unauthorized":
		return true
	}
	return false
}

// Handler adapts a provider's own `auth.refresh` method handler into a
// RefreshFunc. The in-plugin refresh therefore runs exactly the code the host
// would have run, which is what keeps one provider from having two renewal
// implementations.
func Handler(handler func(h *abiboot.Host, raw json.RawMessage) (any, error)) RefreshFunc {
	return func(h *abiboot.Host, request pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
		if handler == nil {
			return pluginapi.AuthRefreshResponse{}, abiboot.Errorf("refresh_unavailable", "该插件没有 auth.refresh 处理函数")
		}
		raw, errMarshal := json.Marshal(request)
		if errMarshal != nil {
			return pluginapi.AuthRefreshResponse{}, abiboot.Errorf("refresh_request", "encode auth.refresh request: %v", errMarshal)
		}
		result, errCall := handler(h, raw)
		if errCall != nil {
			return pluginapi.AuthRefreshResponse{}, errCall
		}
		switch typed := result.(type) {
		case pluginapi.AuthRefreshResponse:
			return typed, nil
		case *pluginapi.AuthRefreshResponse:
			if typed == nil {
				return pluginapi.AuthRefreshResponse{}, abiboot.Errorf("refresh_contract", "auth.refresh 返回了空响应")
			}
			return *typed, nil
		default:
			return pluginapi.AuthRefreshResponse{}, abiboot.Errorf("refresh_contract", "auth.refresh 返回了意外的响应类型 %T", result)
		}
	}
}
