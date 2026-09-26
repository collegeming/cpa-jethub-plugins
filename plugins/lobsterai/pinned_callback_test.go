package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/oauthcb"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// freeLobsterPort returns a port that was free a moment ago.
func freeLobsterPort(t *testing.T) int {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve free port: %v", errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// pinnedLobsterSettings configures the plugin the way a container deployment
// does — a pinned callback port bound on a reachable host — and restores the
// defaults afterwards. The cleanup shuts the plugin down because the callback
// listener now outlives the sessions a test starts.
func pinnedLobsterSettings(t *testing.T) int {
	t.Helper()
	port := freeLobsterPort(t)

	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"
	setSettings(cfg)
	t.Cleanup(func() {
		setSettings(DefaultConfig())
		shutdownLoginSessions()
	})
	return port
}

// callbackClient issues one callback request without a kept-alive connection, so
// a released listener really refuses the connection instead of being served
// from a pooled socket.
func callbackClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   2 * time.Second,
	}
}

// assertCallbackReachable fetches the callback URL exactly like the portal's
// browser redirect does and requires the plugin's success page. A refused
// connection here is the production symptom — the browser's
// ERR_CONNECTION_REFUSED — that this contract exists to prevent.
func assertCallbackReachable(t *testing.T, callbackURI string) {
	t.Helper()
	response, errGet := callbackClient().Get(callbackURI)
	if errGet != nil {
		t.Fatalf("no listener bound at %s: %v", callbackURI, errGet)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback %s returned %d, want 200", callbackURI, response.StatusCode)
	}
}

// waitForPendingCode waits for the dispatcher goroutine to hand the captured
// code to the session. Routing is asynchronous by design: the listener's reader
// is one goroutine shared by every sign-in.
func waitForPendingCode(t *testing.T, session *loginSession, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code := session.pendingCode(); code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never captured %q, captured %q", want, session.pendingCode())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPinnedCallbackPortSurvivesRetry is the container regression guard. The
// pinned port belongs to the plugin's persistent listener, so starting a second
// sign-in — what the panel's 重试 button does — never rebinds it and cannot fail
// with "address already in use".
func TestPinnedCallbackPortSurvivesRetry(t *testing.T) {
	port := pinnedLobsterSettings(t)

	now := time.Now()
	first, errFirst := startLoginSession(now)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	if first.Port != port {
		t.Fatalf("first session port = %d, want the pinned %d", first.Port, port)
	}
	firstCallbackURI := first.RedirectURI()

	second, errSecond := startLoginSession(now)
	if errSecond != nil {
		t.Fatalf("retry failed, want the previous attempt superseded: %v", errSecond)
	}
	defer second.fail("test teardown")

	if second.Port != port {
		t.Fatalf("retry bound port %d, want the pinned %d", second.Port, port)
	}
	if second.RedirectURI() != firstCallbackURI {
		t.Fatalf("retry redirect = %q, want the shared listener's %q", second.RedirectURI(), firstCallbackURI)
	}

	// The replaced session must settle with an explanation; leaving it pending
	// would keep the panel polling a state that can never complete.
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

	// Settling the replaced session must not take the port down with it: the
	// browser may still be on its way to that exact redirect.
	assertCallbackReachable(t, firstCallbackURI)
}

// TestCallbackListenerOutlivesEndedSession reproduces the reported failure. The
// browser lands on the published callback port after the session that started
// the sign-in has already ended (TTL, supersede or restart), so the listener has
// to stay bound with nothing in flight — and the next sign-in has to reuse it.
func TestCallbackListenerOutlivesEndedSession(t *testing.T) {
	port := pinnedLobsterSettings(t)

	first, errFirst := startLoginSession(time.Now())
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	// A TTL expiry ends the session exactly like this.
	first.fail("登录会话已超时，请重新发起")

	// Nothing is in flight right now: precisely the moment the old per-session
	// listener had already released the pinned port.
	assertCallbackReachable(t, first.RedirectURI())

	second, errSecond := startLoginSession(time.Now())
	if errSecond != nil {
		t.Fatalf("startLoginSession after the first ended: %v", errSecond)
	}
	defer second.fail("test teardown")
	if second.Port != port {
		t.Fatalf("session port = %d, want the reused pinned %d", second.Port, port)
	}

	// A late callback for another state must not be adopted. Routing is async,
	// so let the dispatcher consume it before asserting the session still waits;
	// TestDeliverRoutesByState proves the same rule synchronously.
	foreignURI := second.RedirectURI() + "?code=foreign-code&state=not-" + url.QueryEscape(second.State)
	assertCallbackReachable(t, foreignURI)
	time.Sleep(100 * time.Millisecond)
	if code := second.pendingCode(); code != "" {
		t.Fatalf("session adopted a callback for another state: %q", code)
	}

	// The real callback, arriving on the reused listener, must reach the later
	// session even though a different session owned the listener when it was
	// built.
	assertCallbackReachable(t, second.RedirectURI()+"?code=late-code&state="+url.QueryEscape(second.State))
	waitForPendingCode(t, second, "late-code")
}

// TestRepeatedPinnedSignInsNeverFail is the "address already in use" guard: the
// panel's 重试 may start any number of sign-ins on the pinned port, and because
// the persistent listener owns the port for the process lifetime none of them
// rebinds it.
func TestRepeatedPinnedSignInsNeverFail(t *testing.T) {
	port := pinnedLobsterSettings(t)

	callbackURI := ""
	for attempt := 1; attempt <= 8; attempt++ {
		session, errStart := startLoginSession(time.Now())
		if errStart != nil {
			t.Fatalf("sign-in %d on the pinned port %d failed: %v", attempt, port, errStart)
		}
		if session.Port != port {
			t.Fatalf("sign-in %d bound %d, want the pinned %d", attempt, session.Port, port)
		}
		// End it the way an expiry or a supersede does, so the next attempt
		// starts with no session in flight.
		session.fail("attempt finished")
		callbackURI = session.RedirectURI()
	}

	assertCallbackReachable(t, callbackURI)
}

// TestDeliverRoutesByState pins the per-session validation the shared listener
// cannot do: it does not know which sign-in a callback belongs to, so deliver
// accepts only a callback carrying this session's state and a code.
func TestDeliverRoutesByState(t *testing.T) {
	setSettings(DefaultConfig())
	t.Cleanup(func() {
		setSettings(DefaultConfig())
		shutdownLoginSessions()
	})

	session, errStart := startLoginSession(time.Now())
	if errStart != nil {
		t.Fatalf("startLoginSession: %v", errStart)
	}
	defer session.fail("test teardown")

	// A callback for another flow must be discarded, not adopted.
	session.deliver(oauthcb.Result{
		Query: url.Values{"state": {"another-flow"}, "code": {"foreign-code"}},
		Code:  "foreign-code",
	})
	if code := session.pendingCode(); code != "" {
		t.Fatalf("deliver adopted a callback for another state: %q", code)
	}

	// A matching state without a code is unusable too: the exchange would POST
	// an empty authCode.
	session.deliver(oauthcb.Result{Query: url.Values{"state": {session.State}}})
	if code := session.pendingCode(); code != "" {
		t.Fatalf("deliver captured a codeless callback: %q", code)
	}

	// The usable callback must land.
	session.deliver(oauthcb.Result{
		Query: url.Values{"state": {session.State}, "code": {"usable-code"}},
		Code:  "usable-code",
	})
	if code := session.pendingCode(); code != "usable-code" {
		t.Fatalf("captured code = %q, want usable-code", code)
	}

	// A settled session must not be revived by a callback that arrives late.
	session.fail("test teardown")
	session.deliver(oauthcb.Result{
		Query: url.Values{"state": {session.State}, "code": {"late-code"}},
		Code:  "late-code",
	})
	if code := session.pendingCode(); code != "usable-code" {
		t.Fatalf("a finished session absorbed a late callback: %q", code)
	}
}

// TestUnrelatedSettingsKeepTheListener covers the "rebuild only when the
// listener itself changes" half of the cache rule. plugin.reconfigure runs on
// every settings save, and a needless rebuild would drop the pinned port in the
// window between closing and rebinding it.
func TestUnrelatedSettingsKeepTheListener(t *testing.T) {
	pinnedLobsterSettings(t)

	first, errFirst := startLoginSession(time.Now())
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	defer first.fail("test teardown")

	callbackMu.Lock()
	before := callbackServer
	callbackMu.Unlock()

	cfg := settings()
	cfg.DailyCheckin = !cfg.DailyCheckin
	cfg.ModelCacheTTLMS += 1000
	setSettings(cfg)

	second, errSecond := startLoginSession(time.Now())
	if errSecond != nil {
		t.Fatalf("startLoginSession after a settings save: %v", errSecond)
	}
	defer second.fail("test teardown")

	callbackMu.Lock()
	after := callbackServer
	callbackMu.Unlock()
	if before == nil || after != before {
		t.Fatal("a settings change that does not shape the listener rebuilt it")
	}
	if second.Port != first.Port {
		t.Fatalf("session port = %d, want the kept %d", second.Port, first.Port)
	}
}

// TestCallbackListenerRebuildsOnSettingsChange is the other half: a reconfigured
// bind address or port must not keep answering on the old one, and the replaced
// listener has to release the old port instead of holding it for the process
// lifetime.
func TestCallbackListenerRebuildsOnSettingsChange(t *testing.T) {
	firstPort := pinnedLobsterSettings(t)

	first, errFirst := startLoginSession(time.Now())
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	defer first.fail("test teardown")
	if first.Port != firstPort {
		t.Fatalf("first session port = %d, want the pinned %d", first.Port, firstPort)
	}

	secondPort := freeLobsterPort(t)
	cfg := DefaultConfig()
	cfg.CallbackPort = secondPort
	cfg.CallbackBindHost = "127.0.0.1"
	setSettings(cfg)

	second, errSecond := startLoginSession(time.Now())
	if errSecond != nil {
		t.Fatalf("startLoginSession after the port moved: %v", errSecond)
	}
	defer second.fail("test teardown")
	if second.Port != secondPort {
		t.Fatalf("rebuilt session port = %d, want the new pinned %d", second.Port, secondPort)
	}
	assertCallbackReachable(t, second.RedirectURI())

	if _, errGet := callbackClient().Get(first.RedirectURI()); errGet == nil {
		t.Fatalf("the replaced listener is still bound at %s", first.RedirectURI())
	}
}

// TestUnpinnedCallbackPortKeepsConcurrentSessions protects native installs:
// without a pinned port a second sign-in replaces nothing, because the two can
// be told apart by state. They do share the plugin's one listener, which is why
// the shared port is asserted here.
func TestUnpinnedCallbackPortKeepsConcurrentSessions(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CallbackPort != 0 {
		t.Fatalf("default CallbackPort = %d, want 0 (unpinned)", cfg.CallbackPort)
	}
	setSettings(cfg)
	t.Cleanup(func() {
		setSettings(DefaultConfig())
		shutdownLoginSessions()
	})

	now := time.Now()
	first, errFirst := startLoginSession(now)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	defer first.fail("test teardown")

	second, errSecond := startLoginSession(now)
	if errSecond != nil {
		t.Fatalf("second startLoginSession: %v", errSecond)
	}
	defer second.fail("test teardown")

	if _, _, _, finished := first.snapshot(); finished {
		t.Fatal("unpinned start superseded an earlier session, want both left running")
	}
	if second.Port != first.Port {
		t.Fatalf("second session port = %d, want the shared listener's %d", second.Port, first.Port)
	}
}
