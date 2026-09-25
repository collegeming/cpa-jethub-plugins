package main

import (
	"net/http"
	"strings"
	"testing"
)

// lobsterAIHeaders is the exact set of Tencent-family attribution headers that
// must never be sent to LobsterAI.
var tencentHeaders = []string{
	"X-Domain", "X-Enterprise-Id", "X-Tenant-Id", "X-No-Authorization",
	"X-Refresh-Token", "X-Product", "X-Product-Code", "X-IDE-Version",
}

func assertNoTencentHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	for _, name := range tencentHeaders {
		if value := headers.Get(name); value != "" {
			t.Fatalf("header %s = %q must not be sent to LobsterAI", name, value)
		}
	}
}

func TestAuthHeaders(t *testing.T) {
	credential := &Credential{AccessToken: "token-123"}
	headers := authHeaders(credential, "")

	if got := headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := headers.Get("User-Agent"); got != UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	// The control-plane header set is exactly four headers: no client headers.
	if got := headers.Get("X-LobsterAI-Client-Capabilities"); got != "" {
		t.Fatalf("auth headers must not carry client capabilities, got %q", got)
	}
	assertNoTencentHeaders(t, headers)
}

func TestChatHeaders(t *testing.T) {
	credential := &Credential{AccessToken: "token-123"}
	headers := chatHeaders(credential, "2026.9.4")

	if got := headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream, application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := headers.Get("X-LobsterAI-Client-Capabilities"); got != ClientCapabilities {
		t.Fatalf("X-LobsterAI-Client-Capabilities = %q", got)
	}
	if got := headers.Get("X-LobsterAI-Client-Version"); got != "2026.9.4" {
		t.Fatalf("X-LobsterAI-Client-Version = %q", got)
	}
	assertNoTencentHeaders(t, headers)
}

func TestModelsHeaders(t *testing.T) {
	credential := &Credential{AccessToken: "token-123"}
	headers := modelsHeaders(credential, "")

	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	// The capability header is a model-list admission condition, not decoration.
	if got := headers.Get("X-LobsterAI-Client-Capabilities"); got != ClientCapabilities {
		t.Fatalf("X-LobsterAI-Client-Capabilities = %q", got)
	}
	// An unresolved version must never render as an empty header.
	if got := headers.Get("X-LobsterAI-Client-Version"); got != FallbackClientVersion {
		t.Fatalf("X-LobsterAI-Client-Version = %q, want fallback", got)
	}
	assertNoTencentHeaders(t, headers)
}

func TestAnonymousHeaders(t *testing.T) {
	headers := anonymousHeaders()
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("exchange/refresh must not send Authorization, got %q", got)
	}
	if got := headers.Get("User-Agent"); got != UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	assertNoTencentHeaders(t, headers)
}

func TestResolveHeaderVersion(t *testing.T) {
	if got := resolveHeaderVersion("  2026.1.1 "); got != "2026.1.1" {
		t.Fatalf("resolveHeaderVersion trimmed = %q", got)
	}
	if got := resolveHeaderVersion(""); got != FallbackClientVersion {
		t.Fatalf("resolveHeaderVersion(empty) = %q", got)
	}
	if !strings.Contains(ClientCapabilities, "kimi-k3-agentic-v1") {
		t.Fatal("capabilities must include kimi-k3-agentic-v1")
	}
}
