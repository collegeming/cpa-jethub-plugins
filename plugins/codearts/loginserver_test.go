package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// freePort returns a port that was free a moment ago. Tests use it for the
// pinned-callback cases, which must bind an exact port.
func freePort(t *testing.T) int {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve free port: %v", errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// withSettings runs fn with cfg installed as the plugin's live settings and
// restores the previous value afterwards.
func withSettings(t *testing.T, cfg Config, fn func()) {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })
	fn()
}

// TestPinnedCallbackPortBelowPortalMinimumFailsFast guards the misconfiguration
// the MinPort retry loop cannot catch: a pinned port bypasses that loop, so a
// value the Huawei portal rejects (below 10000, login.ts:338-341) would bind
// successfully and then simply never receive a callback — a failure that looks
// like a network problem instead of a bad setting.
func TestPinnedCallbackPortBelowPortalMinimumFailsFast(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CallbackPort = minCallbackPort - 1

	withSettings(t, cfg, func() {
		session, errStart := startLoginSession(LoginFlowOAuth)
		if errStart == nil {
			session.fail("test teardown")
			t.Fatalf("startLoginSession with callback_port=%d succeeded, want a fast failure", cfg.CallbackPort)
		}
		if !strings.Contains(errStart.Error(), "callback_port") {
			t.Fatalf("error = %v, want it to name the offending setting", errStart)
		}
		if !strings.Contains(errStart.Error(), strconv.Itoa(minCallbackPort)) {
			t.Fatalf("error = %v, want it to state the %d minimum", errStart, minCallbackPort)
		}
	})
}

// TestPinnedCallbackPortBindsExactlyThatPort is the container contract: the
// port handed to the portal must be the published one, so a pinned listener may
// not drift to another port.
func TestPinnedCallbackPortBindsExactlyThatPort(t *testing.T) {
	port := freePort(t)
	if port < minCallbackPort {
		t.Skipf("kernel assigned %d below the portal minimum; rerun", port)
	}

	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"

	withSettings(t, cfg, func() {
		session, errStart := startLoginSession(LoginFlowOAuth)
		if errStart != nil {
			t.Fatalf("startLoginSession: %v", errStart)
		}
		defer func() { session.fail("test teardown") }()

		if session.Port != port {
			t.Fatalf("session port = %d, want the pinned %d", session.Port, port)
		}
		if session.callback == nil {
			t.Fatal("session has no callback listener")
		}
		if !strings.Contains(session.callback.RedirectURI(), ":"+strconv.Itoa(port)+"/") {
			t.Fatalf("RedirectURI = %q, want it to carry the pinned port %d", session.callback.RedirectURI(), port)
		}
	})
}

// TestRetryWithPinnedPortSupersedesPreviousAttempt is the regression guard for
// the container retry loop. A pinned port stays bound for the whole life of the
// pending session, so before this fix pressing 重试 — or simply starting a second
// sign-in — failed with "address already in use" until the first session's TTL
// lapsed. Superseding the older attempt therefore has to happen before the bind.
func TestRetryWithPinnedPortSupersedesPreviousAttempt(t *testing.T) {
	port := freePort(t)
	if port < minCallbackPort {
		t.Skipf("kernel assigned %d below the portal minimum; rerun", port)
	}

	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"

	withSettings(t, cfg, func() {
		first, errFirst := startLoginSession(LoginFlowOAuth)
		if errFirst != nil {
			t.Fatalf("first startLoginSession: %v", errFirst)
		}

		// Exactly what the panel does when the user presses 重试.
		second, errSecond := startLoginSession(LoginFlowOAuth)
		if errSecond != nil {
			first.fail("test teardown")
			t.Fatalf("retry failed, want the previous attempt superseded: %v", errSecond)
		}
		defer func() { second.fail("test teardown") }()

		if second.Port != port {
			t.Fatalf("retry bound port %d, want the pinned %d", second.Port, port)
		}

		// The replaced session must settle with an explanation; leaving it
		// pending would keep the panel polling a state that can never complete.
		status, message, _, finished := first.snapshot()
		if !finished {
			t.Fatal("superseded session is still pending, want it settled")
		}
		if status != pluginapi.AuthLoginStatusError {
			t.Fatalf("superseded status = %v, want error", status)
		}
		if !strings.Contains(message, "取代") {
			t.Fatalf("superseded message = %q, want it to explain the replacement", message)
		}
	})
}

// TestUnpinnedCallbackPortKeepsEphemeralBehaviour protects native installs: with
// no callback_port the historical behaviour stands — an ephemeral port above the
// portal floor, chosen by the kernel.
func TestUnpinnedCallbackPortKeepsEphemeralBehaviour(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CallbackPort != 0 {
		t.Fatalf("default CallbackPort = %d, want 0 (ephemeral)", cfg.CallbackPort)
	}

	withSettings(t, cfg, func() {
		session, errStart := startLoginSession(LoginFlowOAuth)
		if errStart != nil {
			t.Fatalf("startLoginSession: %v", errStart)
		}
		defer func() { session.fail("test teardown") }()

		if session.Port < minCallbackPort {
			t.Fatalf("session port = %d, want >= %d", session.Port, minCallbackPort)
		}
	})
}

// resetCallbackListener drops the plugin's shared listener so a test decides
// which port the next sign-in binds; the listener is process-wide and would
// otherwise outlive the test that started it.
func resetCallbackListener(t *testing.T) {
	t.Helper()
	closeCallbackListener()
	t.Cleanup(closeCallbackListener)
}

// pinnedPortSettings is the container configuration: a published port bound on
// the loopback address the test can dial.
func pinnedPortSettings(t *testing.T) (Config, int) {
	t.Helper()
	port := freePort(t)
	if port < minCallbackPort {
		t.Skipf("kernel assigned %d below the portal minimum; rerun", port)
	}
	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"
	return cfg, port
}

// hitCallback performs one browser-style request against a bound callback port.
// Redirects are not followed: the OAuth callback answers with a 307 to the
// portal success page, which the test observes instead of fetching.
func hitCallback(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, errDo := client.Get(url)
	if errDo != nil {
		t.Fatalf("request callback %s: %v", url, errDo)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// waitForAuthCode waits for the dispatch goroutine to hand a code to a session.
func waitForAuthCode(t *testing.T, session *loginSession, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if session.pendingCode() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session never received code %q (last %q)", want, session.pendingCode())
}

// waitForTicketSecret is the ticket-flow counterpart of waitForAuthCode.
func waitForTicketSecret(t *testing.T, session *loginSession, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if session.pendingSecret() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session never received secret %q (last %q)", want, session.pendingSecret())
}

// TestCallbackListenerOutlivesTheSession is the production bug: the portal
// redirects the browser to the pinned port at a moment that has nothing to do
// with the session that started the flow, so a session ending must not release
// the port. The first session is failed before the browser acts, and the callback
// still has to reach the second sign-in on the same listener.
func TestCallbackListenerOutlivesTheSession(t *testing.T) {
	resetCallbackListener(t)
	cfg, port := pinnedPortSettings(t)

	withSettings(t, cfg, func() {
		first, errFirst := startLoginSession(LoginFlowOAuth)
		if errFirst != nil {
			t.Fatalf("first startLoginSession: %v", errFirst)
		}
		first.fail("test teardown")

		second, errSecond := startLoginSession(LoginFlowOAuth)
		if errSecond != nil {
			t.Fatalf("sign-in after a finished session: %v", errSecond)
		}
		if second.Port != first.Port {
			t.Fatalf("later session bound port %d, want the still-bound %d", second.Port, first.Port)
		}
		callbackMu.Lock()
		shared := callbackServer
		callbackMu.Unlock()
		if shared == nil || second.callback != shared {
			t.Fatal("later sign-in did not reuse the plugin's shared listener")
		}

		// Exactly what the Huawei portal does once the user finishes authorising.
		const code = "oauth-code-after-session-end"
		response := hitCallback(t, fmt.Sprintf("http://127.0.0.1:%d%s?code=%s", port, OAuthRedirectPath, code))
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("callback answered %d, want the OAuth 307 to the portal success page", response.StatusCode)
		}
		waitForAuthCode(t, second, code)
	})
}

// TestDeliverRoutesOnlyUsableCallbacks pins the per-flow rule the old
// per-session wait loop enforced: a callback without the parameter its flow needs
// is ignored so the session stays pending for the next one, and a session that
// already finished never takes another callback.
func TestDeliverRoutesOnlyUsableCallbacks(t *testing.T) {
	oauth := &loginSession{Flow: LoginFlowOAuth}
	oauth.deliver(oauthcb.Result{Query: url.Values{"error": {"access_denied"}}})
	if code := oauth.pendingCode(); code != "" {
		t.Fatalf("unusable callback stored code %q, want none", code)
	}

	oauth.deliver(oauthcb.Result{Code: "oauth-code"})
	if code := oauth.pendingCode(); code != "oauth-code" {
		t.Fatalf("code = %q, want %q", code, "oauth-code")
	}

	// A later stray callback must not clear a code the panel has not polled yet.
	oauth.deliver(oauthcb.Result{Query: url.Values{"error": {"access_denied"}}})
	if code := oauth.pendingCode(); code != "oauth-code" {
		t.Fatalf("code = %q after a stray callback, want %q kept", code, "oauth-code")
	}

	oauth.finish(nil, "登录成功")
	oauth.deliver(oauthcb.Result{Code: "late-code"})
	if code := oauth.pendingCode(); code != "oauth-code" {
		t.Fatalf("finished session took a late callback: code = %q", code)
	}

	ticket := &loginSession{Flow: LoginFlowTicket}
	ticket.deliver(oauthcb.Result{Code: "not-a-secret"})
	if secret := ticket.pendingSecret(); secret != "" {
		t.Fatalf("ticket session stored secret %q from a code-only callback, want none", secret)
	}
	ticket.deliver(oauthcb.Result{Secret: "ticket-secret"})
	if secret := ticket.pendingSecret(); secret != "ticket-secret" {
		t.Fatalf("secret = %q, want %q", secret, "ticket-secret")
	}
}

// TestRepeatedPinnedPortSignInsNeverFail is the container retry regression: with
// the bind owned by the session, a second sign-in raced the port the first one
// still held and failed with "address already in use". Repeated sign-ins must all
// succeed on the pinned port, and the last one must still receive the callback.
func TestRepeatedPinnedPortSignInsNeverFail(t *testing.T) {
	resetCallbackListener(t)
	cfg, port := pinnedPortSettings(t)

	withSettings(t, cfg, func() {
		sessions := make([]*loginSession, 0, 5)
		for attempt := 1; attempt <= 5; attempt++ {
			session, errStart := startLoginSession(LoginFlowOAuth)
			if errStart != nil {
				t.Fatalf("sign-in %d failed: %v", attempt, errStart)
			}
			if session.Port != port {
				t.Fatalf("sign-in %d bound port %d, want the pinned %d", attempt, session.Port, port)
			}
			sessions = append(sessions, session)
		}
		last := sessions[len(sessions)-1]

		const code = "oauth-code-on-retry"
		response := hitCallback(t, fmt.Sprintf("http://127.0.0.1:%d%s?code=%s", port, OAuthRedirectPath, code))
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("callback answered %d, want the OAuth 307", response.StatusCode)
		}
		waitForAuthCode(t, last, code)

		// A replaced attempt is settled, so it can neither answer with the new
		// attempt's code nor keep the panel polling.
		for _, superseded := range sessions[:len(sessions)-1] {
			if code := superseded.pendingCode(); code != "" {
				t.Fatalf("superseded session received code %q, want it settled and ignored", code)
			}
		}
	})
}

// TestFlowSwitchRebindsTheSharedListener guards the option key: the OAuth flow
// answers /oauth/callback and the ticket flow answers /authentication, so a
// cached listener from the other flow would 404 the browser — and on a pinned
// port that rebuild has to release the port before binding it again.
func TestFlowSwitchRebindsTheSharedListener(t *testing.T) {
	resetCallbackListener(t)
	cfg, port := pinnedPortSettings(t)

	withSettings(t, cfg, func() {
		oauthSession, errOAuth := startLoginSession(LoginFlowOAuth)
		if errOAuth != nil {
			t.Fatalf("oauth startLoginSession: %v", errOAuth)
		}
		oauthSession.fail("test teardown")

		ticketSession, errTicket := startLoginSession(LoginFlowTicket)
		if errTicket != nil {
			t.Fatalf("ticket startLoginSession on the same pinned port: %v", errTicket)
		}
		if ticketSession.Port != port {
			t.Fatalf("ticket session bound port %d, want the pinned %d", ticketSession.Port, port)
		}

		const secret = "ticket-secret-after-flow-switch"
		response := hitCallback(t, fmt.Sprintf("http://127.0.0.1:%d%s?secret=%s", port, LegacyCallbackPath, secret))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("ticket callback answered %d, want the 200 page (the OAuth path is gone)", response.StatusCode)
		}
		waitForTicketSecret(t, ticketSession, secret)
	})
}

// TestShutdownReleasesTheSharedListener covers the only event that may release
// the port: plugin shutdown. A later sign-in — the host can reload the plugin
// in-process — must then rebind instead of reusing the closed server.
func TestShutdownReleasesTheSharedListener(t *testing.T) {
	resetCallbackListener(t)
	cfg, port := pinnedPortSettings(t)

	withSettings(t, cfg, func() {
		if _, errStart := startLoginSession(LoginFlowOAuth); errStart != nil {
			t.Fatalf("startLoginSession: %v", errStart)
		}

		shutdownLoginSessions()

		listener, errListen := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if errListen != nil {
			t.Fatalf("port %d is still bound after shutdown: %v", port, errListen)
		}
		_ = listener.Close()

		session, errRestart := startLoginSession(LoginFlowOAuth)
		if errRestart != nil {
			t.Fatalf("sign-in after shutdown: %v", errRestart)
		}
		if session.callback == nil || session.callback.Port() != port {
			t.Fatalf("sign-in after shutdown did not rebind the pinned port %d", port)
		}
	})
}
