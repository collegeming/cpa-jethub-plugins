// Package oauthcb implements the shared loopback callback listener used by the
// Jet-Hub provider ports for interactive sign-in.
//
// It is the generalisation of the bespoke listener that used to live inside
// plugins/codearts/loginserver.go: bind 127.0.0.1 on a port the upstream portal
// accepts, serve exactly one callback path, capture the query parameters and
// hand them to a blocking waiter.
//
// Two callback styles are supported, both taken from the CodeArts flows:
//
//   - OAuth/PKCE: the portal redirects the browser to the callback path and the
//     plugin answers with a 307 back to the portal's success page
//     (Options.RedirectURL).
//   - Legacy ticket: the portal redirects to the callback path and the plugin
//     answers with a small HTML page telling the user to return to the panel
//     (Options.RedirectURL empty).
//
// The package is deliberately transport-only: it knows nothing about tokens,
// PKCE or the plugin host. Callers start a server, publish RedirectURI() to the
// portal, and block in Wait.
package oauthcb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults applied by Start when the matching Options field is zero.
const (
	// DefaultMinPort matches the constraint the CodeArts portal enforces on its
	// loopback redirect: the port must be 10000 or above.
	DefaultMinPort = 10000
	// DefaultTTL bounds how long an interactive sign-in may stay pending.
	DefaultTTL = 5 * time.Minute
	// DefaultBindAttempts is how many ephemeral ports Start tries before giving
	// up. Ports below MinPort are rejected and retried.
	DefaultBindAttempts = 32
	// DefaultBindHost is the loopback address. It is correct on a workstation,
	// where the browser and this process share a loopback interface, and wrong
	// in a container, where they do not: a published container port is
	// forwarded to the container's own address, so a 127.0.0.1-only listener is
	// unreachable through `-p`. Container deployments set "0.0.0.0".
	DefaultBindHost = "127.0.0.1"
	// DefaultPublicHost is the host placed in RedirectURI(). The browser always
	// dials the machine it runs on, so this stays "127.0.0.1" even when the
	// listener binds "0.0.0.0".
	DefaultPublicHost = "127.0.0.1"
	// readHeaderTimeout bounds a slow-loris callback request.
	readHeaderTimeout = 10 * time.Second
	// resultBuffer is the number of callbacks retained before the oldest is
	// dropped. A normal flow produces exactly one; the small backlog keeps a
	// repeated/stray callback from blocking the browser request.
	resultBuffer = 4
)

var (
	// ErrTimeout is returned by Wait when the server's TTL elapses before a
	// callback arrives.
	ErrTimeout = errors.New("oauthcb: timed out waiting for the browser callback")
	// ErrClosed is returned by Wait once Close has released the listener.
	ErrClosed = errors.New("oauthcb: callback server is closed")
)

// Result is one captured browser callback.
type Result struct {
	// Query holds every query parameter of the callback request.
	Query url.Values
	// Code is Query.Get("code") (OAuth/PKCE flows).
	Code string
	// Secret is Query.Get("secret") (legacy ticket flows).
	Secret string
	// Path is the request path; always Options.Path because other paths 404.
	Path string
}

// Options configures Start.
type Options struct {
	// Path is the single callback path this server answers, e.g.
	// "/oauth/callback". Required; every other path returns 404.
	Path string
	// MinPort rejects ephemeral ports below it. Defaults to DefaultMinPort.
	// Ignored when Port is set.
	MinPort int
	// TTL bounds the wait for a callback. Defaults to DefaultTTL. Ignored when
	// Persistent is set.
	TTL time.Duration
	// Persistent keeps the listener alive indefinitely: Wait blocks until a
	// callback, ctx cancellation or Close, and never returns ErrTimeout, while
	// ExpiresAt reports the zero time.
	//
	// Use it when the listener must outlive an individual sign-in. A pinned
	// callback port is reachable by the browser only while something is bound to
	// it, so tying the bind to one session means the browser lands on a closed
	// port whenever that session has already ended — timed out, superseded, or
	// lost to a restart. Callers then impose their own deadline per sign-in.
	Persistent bool
	// RedirectURL is where the browser is sent (307) after a successful
	// capture. Empty means the request is answered with a 200 HTML page
	// instead.
	RedirectURL string
	// BindAttempts is the number of ephemeral ports to try before failing.
	// Defaults to DefaultBindAttempts. Ignored when Port is set.
	BindAttempts int
	// SuccessHTML overrides the 200 page body served when RedirectURL is empty.
	// The built-in page is used when empty.
	SuccessHTML string
	// BindHost is the local address the listener binds, e.g. "0.0.0.0".
	// Defaults to DefaultBindHost ("127.0.0.1"), which only works when the
	// browser and this process share a loopback interface — never the case when
	// CPA runs in a container. A container deployment must bind "0.0.0.0" (the
	// same choice the host's own callback forwarder makes) and publish the port.
	BindHost string
	// Port pins the listener to an exact TCP port instead of an ephemeral one.
	// A published port is only reachable from the browser when it is known in
	// advance, so a container deployment must set it. Zero means ephemeral.
	//
	// A pinned port is a single attempt: if it is taken, Start fails rather than
	// silently moving to a port the browser was not told about.
	Port int
	// PreferredPort is tried before falling back to an ephemeral port. Use it
	// for a vendor's documented port when a different port is still acceptable,
	// which is how the reference implementations treat 18080: preferred, with a
	// random fallback on EADDRINUSE/EACCES. Ignored when Port is set.
	PreferredPort int
	// PublicHost and PublicPort are the address placed in RedirectURI(), i.e.
	// the address the BROWSER must use. They default to BindHost and the bound
	// port. They exist because the bind address and the browser-visible address
	// differ: a container binds "0.0.0.0" but the browser still dials
	// "127.0.0.1" on the host side of the published port.
	PublicHost string
	PublicPort int
}

// Server is one loopback callback listener. It is safe for concurrent use.
type Server struct {
	path        string
	redirectURL string
	successHTML string
	expiresAt   time.Time

	bindHost   string
	publicHost string
	publicPort int

	port       int
	listener   net.Listener
	httpServer *http.Server

	results   chan Result
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Start binds the callback listener and begins serving.
//
// With no Options.Port it behaves like the original implementation: bind
// 127.0.0.1 on an ephemeral port >= MinPort. That only works when the browser
// and this process share a loopback interface, which is never true when CPA
// runs in a container — there the container's 127.0.0.1 is not the host's and
// an ephemeral port cannot be published in advance. Such a deployment sets
// Port (publish the same port) and BindHost ("0.0.0.0").
//
// The returned server owns its listener; callers must Close it (directly or via
// a session lifetime) to release the port.
func Start(opts Options) (*Server, error) {
	if opts.Path == "" {
		return nil, errors.New("oauthcb: Options.Path is required")
	}
	minPort := opts.MinPort
	if minPort <= 0 {
		minPort = DefaultMinPort
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	attempts := opts.BindAttempts
	if attempts <= 0 {
		attempts = DefaultBindAttempts
	}
	bindHost := strings.TrimSpace(opts.BindHost)
	if bindHost == "" {
		bindHost = DefaultBindHost
	}

	server := &Server{
		path:        opts.Path,
		redirectURL: opts.RedirectURL,
		successHTML: opts.SuccessHTML,
		expiresAt:   time.Now().Add(ttl),
		bindHost:    bindHost,
		publicHost:  strings.TrimSpace(opts.PublicHost),
		publicPort:  opts.PublicPort,
		results:     make(chan Result, resultBuffer),
		closed:      make(chan struct{}),
	}
	if opts.Persistent {
		// The zero time is what marks the listener as TTL-free, for Wait and for
		// ExpiresAt alike.
		server.expiresAt = time.Time{}
	}
	if server.successHTML == "" {
		server.successHTML = defaultSuccessHTML
	}

	// A pinned port is a single attempt: retrying would silently move the
	// listener off the port the browser was told to call.
	if opts.Port > 0 {
		listener, errListen := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(opts.Port)))
		if errListen != nil {
			return nil, fmt.Errorf("bind callback listener on %s:%d: %w", bindHost, opts.Port, errListen)
		}
		return server.serve(listener)
	}

	// A preferred port keeps the vendor's documented value when it is free but
	// still falls back, which is what the reference implementations do for 18080
	// (EADDRINUSE/EACCES → random port, trae-oauth.ts:508-535). A failure here is
	// not fatal: the ephemeral loop below covers it.
	var preferredErr error
	if opts.PreferredPort > 0 {
		listener, errListen := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(opts.PreferredPort)))
		if errListen == nil {
			return server.serve(listener)
		}
		preferredErr = errListen
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		listener, errListen := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
		if errListen != nil {
			lastErr = errListen
			break
		}
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			lastErr = fmt.Errorf("unexpected listener address %T", listener.Addr())
			_ = listener.Close()
			break
		}
		if addr.Port < minPort {
			lastErr = fmt.Errorf("ephemeral port %d below the portal minimum %d", addr.Port, minPort)
			_ = listener.Close()
			continue
		}
		return server.serve(listener)
	}

	if lastErr == nil {
		lastErr = errors.New("no callback port available")
	}
	if preferredErr != nil {
		lastErr = fmt.Errorf("%w (preferred port %d also unavailable: %v)", lastErr, opts.PreferredPort, preferredErr)
	}
	return nil, fmt.Errorf("bind callback listener: %w", lastErr)
}

// serve wires the accepted listener into the HTTP server and records its port.
// A listener whose address is not a TCP address is unusable, so it is closed
// and reported rather than returned as a partially built server.
func (s *Server) serve(listener net.Listener) (*Server, error) {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	s.port = addr.Port
	s.listener = listener
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleCallback)
	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		// Serve returns http.ErrServerClosed after Close; nothing to report.
		_ = s.httpServer.Serve(listener)
	}()
	return s, nil
}

// Port is the bound port.
func (s *Server) Port() int { return s.port }

// RedirectURI is the URL the portal must call back.
//
// The host defaults to 127.0.0.1 because vendor portals require a loopback
// redirect. Under Docker the published port makes the HOST's 127.0.0.1 reach
// the container listener, so this stays correct as long as the published port
// equals the bound port.
func (s *Server) RedirectURI() string {
	host := s.publicHost
	if host == "" {
		host = DefaultPublicHost
	}
	port := s.publicPort
	if port <= 0 {
		port = s.port
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, s.path)
}

// ExpiresAt is the instant Wait stops accepting callbacks, or the zero time for
// a persistent listener.
func (s *Server) ExpiresAt() time.Time { return s.expiresAt }

// persistent reports whether this listener ignores TTL.
func (s *Server) persistent() bool { return s.expiresAt.IsZero() }

// Wait blocks until a callback arrives, the TTL elapses, or ctx is done.
//
// A callback that arrives after Wait returned is retained (up to the internal
// buffer) and delivered to the next Wait call, which lets a caller discard, say,
// a callback missing the parameter it needs and keep waiting.
//
// A persistent listener never returns ErrTimeout: it keeps accepting callbacks
// for as long as it is open.
func (s *Server) Wait(ctx context.Context) (Result, error) {
	// Prefer an already-captured callback over an expired timer.
	select {
	case result := <-s.results:
		return result, nil
	default:
	}

	if s.persistent() {
		select {
		case result := <-s.results:
			return result, nil
		case <-s.closed:
			return Result{}, ErrClosed
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	remaining := time.Until(s.expiresAt)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case result := <-s.results:
		return result, nil
	case <-timer.C:
		return Result{}, ErrTimeout
	case <-s.closed:
		return Result{}, ErrClosed
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Close releases the listener exactly once. It is safe to call concurrently and
// repeatedly; the first error is returned to every caller.
//
// The raw listener is closed before the HTTP server, and it is closed even when
// an HTTP server exists. http.Server.Close only closes the listeners Serve has
// already registered, and Serve runs in its own goroutine, so delegating to it
// alone can return while the port is still bound — long enough for a retry to
// fail with "address already in use" when the callback port is pinned.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.listener != nil {
			// The HTTP server closes the same listener through onceCloseListener,
			// so the redundant close error is expected and not worth surfacing.
			s.closeErr = s.listener.Close()
		}
		if s.httpServer != nil {
			_ = s.httpServer.Close()
		}
	})
	return s.closeErr
}

// handleCallback captures the callback for the configured path and answers the
// browser. Every other path is a 404.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.path {
		http.NotFound(w, r)
		return
	}

	query := r.URL.Query()
	result := Result{
		Query:  query,
		Code:   query.Get("code"),
		Secret: query.Get("secret"),
		Path:   r.URL.Path,
	}
	// Never block the browser on a slow consumer: the waiter polls the channel,
	// and a full buffer means nobody is listening anyway.
	select {
	case s.results <- result:
	default:
	}

	if s.redirectURL != "" {
		w.Header().Set("Location", s.redirectURL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.successHTML)
}

// defaultSuccessHTML is the built-in 200 page for flows without a portal
// success URL.
const defaultSuccessHTML = "<html><head><meta charset=\"utf-8\"><title>Authorization complete</title></head><body>Login callback received. You may close this tab and return to the CLIProxyAPI panel.</body></html>"
