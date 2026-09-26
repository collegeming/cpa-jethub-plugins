package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
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
