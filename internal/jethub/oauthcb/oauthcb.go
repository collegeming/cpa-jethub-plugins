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
	MinPort int
	// TTL bounds the wait for a callback. Defaults to DefaultTTL.
	TTL time.Duration
	// RedirectURL is where the browser is sent (307) after a successful
	// capture. Empty means the request is answered with a 200 HTML page
	// instead.
	RedirectURL string
	// BindAttempts is the number of ephemeral ports to try before failing.
	// Defaults to DefaultBindAttempts.
	BindAttempts int
	// SuccessHTML overrides the 200 page body served when RedirectURL is empty.
	// The built-in page is used when empty.
	SuccessHTML string
}

// Server is one loopback callback listener. It is safe for concurrent use.
type Server struct {
	path        string
	redirectURL string
	successHTML string
	expiresAt   time.Time

	port       int
	listener   net.Listener
	httpServer *http.Server

	results   chan Result
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Start binds 127.0.0.1 on a port >= MinPort and begins serving.
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

	server := &Server{
		path:        opts.Path,
		redirectURL: opts.RedirectURL,
		successHTML: opts.SuccessHTML,
		expiresAt:   time.Now().Add(ttl),
		results:     make(chan Result, resultBuffer),
		closed:      make(chan struct{}),
	}
	if server.successHTML == "" {
		server.successHTML = defaultSuccessHTML
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		listener, errListen := net.Listen("tcp", "127.0.0.1:0")
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

		server.port = addr.Port
		server.listener = listener
		mux := http.NewServeMux()
		mux.HandleFunc("/", server.handleCallback)
		server.httpServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
		}
		go func() {
			// Serve returns http.ErrServerClosed after Close; nothing to report.
			_ = server.httpServer.Serve(listener)
		}()
		return server, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no loopback port available")
	}
	return nil, fmt.Errorf("bind loopback callback listener: %w", lastErr)
}

// Port is the bound loopback port.
func (s *Server) Port() int { return s.port }

// RedirectURI is the loopback URL the portal must call back.
func (s *Server) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.port, s.path)
}

// ExpiresAt is the instant Wait stops accepting callbacks.
func (s *Server) ExpiresAt() time.Time { return s.expiresAt }

// Wait blocks until a callback arrives, the TTL elapses, or ctx is done.
//
// A callback that arrives after Wait returned is retained (up to the internal
// buffer) and delivered to the next Wait call, which lets a caller discard, say,
// a callback missing the parameter it needs and keep waiting.
func (s *Server) Wait(ctx context.Context) (Result, error) {
	// Prefer an already-captured callback over an expired timer.
	select {
	case result := <-s.results:
		return result, nil
	default:
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
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.httpServer != nil {
			s.closeErr = s.httpServer.Close()
			return
		}
		if s.listener != nil {
			s.closeErr = s.listener.Close()
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
