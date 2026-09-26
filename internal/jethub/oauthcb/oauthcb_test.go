package oauthcb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// noRedirectClient returns the raw response instead of following the 307.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 5 * time.Second,
	}
}

func TestStartCapturesOAuthCallback(t *testing.T) {
	server, err := Start(Options{
		Path:        "/oauth/callback",
		MinPort:     10000,
		TTL:         5 * time.Second,
		RedirectURL: "https://portal.example/login?login_succeed=true",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	if server.Port() < 10000 {
		t.Fatalf("Port() = %d, want >= 10000", server.Port())
	}
	want := "http://127.0.0.1:" + itoa(server.Port()) + "/oauth/callback"
	if got := server.RedirectURI(); got != want {
		t.Fatalf("RedirectURI() = %q, want %q", got, want)
	}
	if time.Until(server.ExpiresAt()) <= 0 {
		t.Fatalf("ExpiresAt() = %v, want in the future", server.ExpiresAt())
	}

	response, err := noRedirectClient().Get(server.RedirectURI() + "?code=TESTCODE123&extra=a%20b")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("callback status = %d, want 307", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "https://portal.example/login?login_succeed=true" {
		t.Fatalf("Location = %q", location)
	}

	result, err := server.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Code != "TESTCODE123" {
		t.Fatalf("Code = %q, want TESTCODE123", result.Code)
	}
	if result.Path != "/oauth/callback" {
		t.Fatalf("Path = %q", result.Path)
	}
	if got := result.Query.Get("extra"); got != "a b" {
		t.Fatalf("Query extra = %q, want \"a b\"", got)
	}
}

func TestWrongPathReturns404(t *testing.T) {
	server, err := Start(Options{Path: "/authentication", TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	response, err := http.Get("http://127.0.0.1:" + itoa(server.Port()) + "/nope?secret=x")
	if err != nil {
		t.Fatalf("GET wrong path: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path status = %d, want 404", response.StatusCode)
	}
}

func TestTicketFlowRendersHTMLPage(t *testing.T) {
	server, err := Start(Options{
		Path:        "/authentication",
		TTL:         5 * time.Second,
		SuccessHTML: "<html>ticket ok</html>",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	response, err := noRedirectClient().Get("http://127.0.0.1:" + itoa(server.Port()) + "/authentication?secret=S3CR3T")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html; charset=utf-8") {
		t.Fatalf("Content-Type = %q", got)
	}
	if !strings.Contains(string(body), "ticket ok") {
		t.Fatalf("body = %q", body)
	}

	result, err := server.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Secret != "S3CR3T" {
		t.Fatalf("Secret = %q, want S3CR3T", result.Secret)
	}
	if result.Code != "" {
		t.Fatalf("Code = %q, want empty", result.Code)
	}
}

func TestDefaultSuccessPageWhenNoRedirect(t *testing.T) {
	server, err := Start(Options{Path: "/oauth/callback", TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	response, err := noRedirectClient().Get("http://127.0.0.1:" + itoa(server.Port()) + "/oauth/callback?code=x")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), "Authorization complete") {
		t.Fatalf("built-in page missing title: %q", body)
	}
}

func TestWaitTimesOutWithSentinel(t *testing.T) {
	server, err := Start(Options{Path: "/oauth/callback", TTL: 80 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	_, err = server.Wait(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Wait error = %v, want ErrTimeout", err)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	server, err := Start(Options{Path: "/oauth/callback", TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = server.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want context.DeadlineExceeded", err)
	}
}

func TestWaitReturnsAfterClose(t *testing.T) {
	server, err := Start(Options{Path: "/oauth/callback", TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if errClose := server.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
	// Idempotent: the second call reuses the first result.
	if errClose := server.Close(); errClose != nil {
		t.Fatalf("second Close: %v", errClose)
	}
	if _, errWait := server.Wait(context.Background()); !errors.Is(errWait, ErrClosed) {
		t.Fatalf("Wait error = %v, want ErrClosed", errWait)
	}
}

func TestStartRequiresPath(t *testing.T) {
	if _, err := Start(Options{}); err == nil {
		t.Fatal("Start with empty Path succeeded, want error")
	}
}

func TestStartAcceptsZeroMinPortAndTTL(t *testing.T) {
	server, err := Start(Options{Path: "/oauth/callback"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()
	if server.Port() < DefaultMinPort {
		t.Fatalf("Port() = %d, want >= %d", server.Port(), DefaultMinPort)
	}
	if remaining := time.Until(server.ExpiresAt()); remaining < DefaultTTL-time.Second {
		t.Fatalf("ExpiresAt - now = %v, want about %v", remaining, DefaultTTL)
	}
}

// busyPort returns a TCP port that is currently held open, plus a release
// function. Tests use it to prove how a pinned and a preferred port react to a
// conflict.
func busyPort(t *testing.T) (int, func()) {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve port: %v", errListen)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		t.Fatalf("reserve port: unexpected addr %T", listener.Addr())
	}
	return addr.Port, func() { _ = listener.Close() }
}

// TestPinnedPortBindsExactly is the container contract. A container deployment
// can only reach the callback through a published port, so the bound port has to
// be the one the user mapped — not merely a floor.
func TestPinnedPortBindsExactly(t *testing.T) {
	port, release := busyPort(t)
	release() // Start needs it free; the port number stays known.

	server, err := Start(Options{
		Path:     "/oauth/callback",
		Port:     port,
		BindHost: "0.0.0.0",
		TTL:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start with pinned port %d: %v", port, err)
	}
	defer func() { _ = server.Close() }()

	if server.Port() != port {
		t.Fatalf("Port() = %d, want the pinned %d", server.Port(), port)
	}

	// The browser still dials loopback even though the listener binds the
	// wildcard address: a published port forwards to the container, and the
	// vendor portals only accept a loopback redirect. Getting this wrong is the
	// whole bug being fixed, so it is asserted rather than assumed.
	want := "http://127.0.0.1:" + itoa(port) + "/oauth/callback"
	if got := server.RedirectURI(); got != want {
		t.Fatalf("RedirectURI() = %q, want %q", got, want)
	}

	response, errGet := noRedirectClient().Get(want + "?code=PINNED")
	if errGet != nil {
		t.Fatalf("GET pinned callback: %v", errGet)
	}
	_ = response.Body.Close()
	result, errWait := server.Wait(context.Background())
	if errWait != nil {
		t.Fatalf("Wait: %v", errWait)
	}
	if result.Code != "PINNED" {
		t.Fatalf("Code = %q, want PINNED", result.Code)
	}
}

// TestPinnedPortFailsLoudlyWhenTaken proves a pinned port is a single attempt. A
// silent fallback would leave the login page advertising a URL that nothing is
// listening on, which is far harder to diagnose than a bind error.
func TestPinnedPortFailsLoudlyWhenTaken(t *testing.T) {
	port, release := busyPort(t)
	defer release()

	if _, err := Start(Options{Path: "/oauth/callback", Port: port}); err == nil {
		t.Fatalf("Start on busy pinned port %d succeeded, want error", port)
	}
}

// TestPreferredPortFallsBackWhenTaken is the workstation contract: the vendor's
// documented port is a preference, and the reference implementation falls back
// to a random port on EADDRINUSE (trae-oauth.ts:508-535).
func TestPreferredPortFallsBackWhenTaken(t *testing.T) {
	port, release := busyPort(t)
	defer release()

	server, err := Start(Options{
		Path:          "/oauth/callback",
		PreferredPort: port,
		MinPort:       1,
		TTL:           5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start with busy preferred port %d: %v", port, err)
	}
	defer func() { _ = server.Close() }()

	if server.Port() == port {
		t.Fatalf("Port() = %d, want a fallback port distinct from the busy %d", server.Port(), port)
	}
	if server.Port() <= 0 {
		t.Fatalf("Port() = %d, want a real port", server.Port())
	}
}

// TestPublicAddressOverridesRedirectURI covers the split between where the
// listener binds and what the browser dials, e.g. a host that maps 443 onto an
// unprivileged container port.
func TestPublicAddressOverridesRedirectURI(t *testing.T) {
	server, err := Start(Options{
		Path:       "/auth/callback",
		Port:       0,
		BindHost:   "0.0.0.0",
		PublicHost: "auth.example.test",
		PublicPort: 443,
		TTL:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	want := "http://auth.example.test:443/auth/callback"
	if got := server.RedirectURI(); got != want {
		t.Fatalf("RedirectURI() = %q, want %q", got, want)
	}
}

// itoa avoids pulling strconv into the test's import list for a single call.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [12]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
