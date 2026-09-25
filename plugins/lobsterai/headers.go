package main

import (
	"net/http"
	"strings"
)

// Header builders, ported from jethub-src/src/lobsterai.ts:412-499.
//
// LobsterAI is pure `Bearer` authentication with no request signing at all —
// unlike CodeArts there is no HMAC, DPoP or PKCE, and unlike the Tencent family
// there are no `X-Domain` / `X-Product` / `X-Product-Code` / `X-IDE-*`
// attribution headers. Sending those would at best be ignored and at worst
// attribute the request to the wrong client shape.

// authHeaders builds the four-header set used by the control-plane reads
// (credits, profile summary). accept defaults to application/json.
func authHeaders(credential *Credential, accept string) http.Header {
	if strings.TrimSpace(accept) == "" {
		accept = "application/json"
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+credential.AccessToken)
	headers.Set("Accept", accept)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", UserAgent)
	return headers
}

// chatHeaders adds the two X-LobsterAI-Client-* headers the chat endpoint
// expects and switches Accept to SSE (lobsterai.ts:446-456).
func chatHeaders(credential *Credential, clientVersion string) http.Header {
	headers := authHeaders(credential, "text/event-stream, application/json")
	headers.Set("X-LobsterAI-Client-Capabilities", ClientCapabilities)
	headers.Set("X-LobsterAI-Client-Version", resolveHeaderVersion(clientVersion))
	return headers
}

// modelsHeaders is the model-listing variant: same two client headers, JSON
// accept. They are *required* on this endpoint, not decorative — the server
// filters the model set by the declared capabilities, and without
// kimi-k3-agentic-v1 the model `kimi-k3` is never returned
// (lobsterai.ts:458-484).
func modelsHeaders(credential *Credential, clientVersion string) http.Header {
	headers := authHeaders(credential, "application/json")
	headers.Set("X-LobsterAI-Client-Capabilities", ClientCapabilities)
	headers.Set("X-LobsterAI-Client-Version", resolveHeaderVersion(clientVersion))
	return headers
}

// anonymousHeaders is used by exchange and refresh: neither endpoint takes an
// Authorization header (lobsterai.ts:486-499).
func anonymousHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", UserAgent)
	return headers
}

// resolveHeaderVersion never sends an empty version header; an unresolved
// version falls back to the known-good constant.
func resolveHeaderVersion(clientVersion string) string {
	if trimmed := strings.TrimSpace(clientVersion); trimmed != "" {
		return trimmed
	}
	return FallbackClientVersion
}
