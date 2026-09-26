package main

import (
	"net"
	"strings"
	"testing"
	"time"

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

// TestPinnedCallbackPortSurvivesRetry is the container regression guard. A
// pinned port stays bound for the life of the pending session, so starting a
// second sign-in — what the panel's 重试 button does — used to fail with
// "address already in use" until the first session's TTL lapsed. The earlier
// attempt now gives the port up before the retry binds it.
func TestPinnedCallbackPortSurvivesRetry(t *testing.T) {
	port := freeLobsterPort(t)

	cfg := DefaultConfig()
	cfg.CallbackPort = port
	cfg.CallbackBindHost = "127.0.0.1"
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })

	now := time.Now()
	first, errFirst := startLoginSession(now)
	if errFirst != nil {
		t.Fatalf("first startLoginSession: %v", errFirst)
	}
	if first.Port != port {
		first.fail("test teardown")
		t.Fatalf("first session port = %d, want the pinned %d", first.Port, port)
	}

	second, errSecond := startLoginSession(now)
	if errSecond != nil {
		first.fail("test teardown")
		t.Fatalf("retry failed, want the previous attempt superseded: %v", errSecond)
	}
	defer second.fail("test teardown")

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
// same port. Only a pinned deployment supersedes.
func TestUnpinnedCallbackPortKeepsConcurrentSessions(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CallbackPort != 0 {
		t.Fatalf("default CallbackPort = %d, want 0 (unpinned)", cfg.CallbackPort)
	}
	setSettings(cfg)
	t.Cleanup(func() { setSettings(DefaultConfig()) })

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
}
