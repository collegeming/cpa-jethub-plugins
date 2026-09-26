package main

import (
	"net"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// freeCallbackPort returns a port that was free a moment ago.
func freeCallbackPort(t *testing.T) int {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve free port: %v", errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// pinnedCallbackConfig is the container-shaped deployment the regression is
// about: the callback port is pinned to the published mapping, bound on loopback
// so the test can dial it the way the browser does.
func pinnedCallbackConfig(t *testing.T) (Config, int) {
	t.Helper()
	port := freeCallbackPort(t)
	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"
	return cfg, port
}

// TestPinnedCallbackPortSurvivesRetry is the container regression guard. A
// pinned port used to belong to the pending session, so starting a second
// sign-in — which is what the panel's 重试 button does — failed with "address
// already in use" until the first session's TTL lapsed. The listener is now
// shared and outlives sessions, so a retry cannot contend for the port at all;
// it only has to settle the attempt it replaces.
func TestPinnedCallbackPortSurvivesRetry(t *testing.T) {
	cfg, port := pinnedCallbackConfig(t)
	t.Cleanup(shutdownLoginSessions)

	first, errFirst := startLoginSession(cfg)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	if first.Port != port {
		t.Fatalf("first session port = %d, want the pinned %d", first.Port, port)
	}

	second, errSecond := startLoginSession(cfg)
	if errSecond != nil {
		t.Fatalf("retry failed, want the previous attempt superseded: %v", errSecond)
	}

	if second.Port != port {
		t.Fatalf("retry bound port %d, want the pinned %d", second.Port, port)
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
}

// TestUnpinnedCallbackPortKeepsConcurrentSessions protects native installs: with
// no pinned port two sign-ins may coexist, because they never contend for the
// same port. Only a pinned deployment supersedes. Both attempts share the one
// listener either way, so they report the port it is bound to.
func TestUnpinnedCallbackPortKeepsConcurrentSessions(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CallbackPort != 0 {
		t.Fatalf("default CallbackPort = %d, want 0 (unpinned)", cfg.CallbackPort)
	}
	t.Cleanup(shutdownLoginSessions)

	first, errFirst := startLoginSession(cfg)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}

	second, errSecond := startLoginSession(cfg)
	if errSecond != nil {
		t.Fatalf("second startLoginSession: %v", errSecond)
	}

	if _, _, _, finished := first.snapshot(); finished {
		t.Fatal("unpinned start superseded an earlier session, want both left running")
	}
	if first.Port != second.Port {
		t.Fatalf("sessions report ports %d and %d, want the one shared listener", first.Port, second.Port)
	}
}
