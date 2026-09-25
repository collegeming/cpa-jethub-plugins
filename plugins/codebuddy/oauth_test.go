package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestResponseHelpers(t *testing.T) {
	body := map[string]any{"code": float64(CodeTokenNotReady), "message": "not ready", "data": map[string]any{"a": 1}}
	if got := responseCode(body); got != CodeTokenNotReady {
		t.Errorf("responseCode = %d", got)
	}
	if got := responseMessage(body); got != "not ready" {
		t.Errorf("responseMessage = %q", got)
	}
	if _, ok := responseData(body); !ok {
		t.Error("responseData should find the data object")
	}
	if _, ok := responseData(map[string]any{"data": nil}); ok {
		t.Error("a null data field must be treated as absent")
	}
	if responseCode(nil) != 0 || responseMessage("string") != "" {
		t.Error("non-object bodies must be tolerated")
	}
}

func TestLoginSessionLifecycle(t *testing.T) {
	// A nil host means the transport is unavailable, which is exactly the
	// offline case: auth/state fails and the CodeBuddy product falls back to the
	// URL documented at buddy.ts:6.
	shutdownLoginSessions()
	defer shutdownLoginSessions()
	previous := settings()
	setSettings(DefaultConfig())
	defer setSettings(previous)

	result, errStart := startLogin(nil, settings())
	if errStart != nil {
		t.Fatalf("startLogin: %v", errStart)
	}
	if !result.Fallback {
		t.Error("an unreachable auth/state must be flagged as a fallback")
	}
	if result.AuthURL != WebsiteHome+"/login/?platform=ide&state="+result.State {
		t.Errorf("fallback URL = %q", result.AuthURL)
	}
	if result.State == "" || result.ExpiresAt.IsZero() {
		t.Errorf("session = %+v", result)
	}

	// The session must be reachable by the state the host echoes back, and one
	// poll must report "pending" rather than an error while the transport is
	// down (buddy-oauth.ts:207-210 keeps polling on network failures).
	response := pollLogin(nil, result.State)
	if response.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("poll status = %q (%s), want pending", response.Status, response.Message)
	}

	// An unknown state is reported, not silently accepted.
	unknown := pollLogin(nil, "does-not-exist")
	if unknown.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("unknown state status = %q, want error", unknown.Status)
	}

	// A finished/failed session is dropped, so a second poll says "not found".
	lookup, found := lookupLoginSession(result.State)
	if !found {
		t.Fatal("the session should still be registered")
	}
	lookup.fail("boom")
	forgetLoginSession(result.State)
	if _, stillThere := lookupLoginSession(result.State); stillThere {
		t.Error("a forgotten session must not be reachable")
	}
}

func TestStartLoginNeedsAFallbackOnlyForCodeBuddy(t *testing.T) {
	shutdownLoginSessions()
	defer shutdownLoginSessions()
	previous := settings()
	defer setSettings(previous)

	// Products without a documented website home must surface the failure
	// rather than inventing a URL.
	setSettings(Config{Product: ProductWorkBuddy})
	if _, errStart := startLogin(nil, settings()); errStart == nil {
		t.Fatal("WorkBuddy has no documented fallback login URL, so startLogin must fail")
	}
	if len(loginSessions) != 0 {
		t.Errorf("a failed start must not register a session: %d", len(loginSessions))
	}
}

func TestRefreshCredentialRejectsNonRefreshable(t *testing.T) {
	_, errRefresh := refreshCredential(nil, &Credential{AccessToken: "tok"}, ProductDefault())
	if errRefresh == nil {
		t.Fatal("a credential without a refresh token must be rejected")
	}
	if errRefresh != errRefreshTokenExpired {
		t.Fatalf("error = %v, want the refresh-token-expired sentinel", errRefresh)
	}
}

func TestShutdownClearsSessions(t *testing.T) {
	shutdownLoginSessions()
	registerLoginSession(newTestSession("s-1"))
	registerLoginSession(newTestSession("s-2"))
	if len(loginSessions) != 2 {
		t.Fatalf("registered %d sessions, want 2", len(loginSessions))
	}
	shutdownLoginSessions()
	if len(loginSessions) != 0 {
		t.Fatalf("shutdown left %d sessions", len(loginSessions))
	}
}

func newTestSession(state string) *loginSession {
	return &loginSession{
		State:     state,
		Product:   ProductCodeBuddy,
		ExpiresAt: time.Now().Add(LoginTimeoutMS * time.Millisecond),
		status:    pluginapi.AuthLoginStatusPending,
	}
}
