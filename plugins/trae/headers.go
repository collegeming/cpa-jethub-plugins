package main

import "net/http"

// Request header assembly. Every builder returns a fresh http.Header; values are
// installed with Set, which canonicalises the header name. That is deliberate:
// the documented TRAE failure mode ③ is a `content-type` key that differs only in
// case from an existing `Content-Type`, which net/http would keep as a *second*
// entry and send as `"application/json, application/json"`
// (docs/agents/trae.md:85-87). Canonical names make that impossible.

// soloHeaders builds the chat / model-catalog headers (trae.ts:176-208).
//
// Three headers carry the same token on purpose (Authorization, X-Cloudide-Token,
// X-Ide-Token); dropping any of them has been observed to be rejected upstream
// (trae.ts:169-172).
//
// machineIDGeneration is the rotation generation; 0 means "use machine_id as
// stored", which is the default.
func soloHeaders(credential *Credential, product product, stream bool, machineIDGeneration int) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	headers.Set("User-Agent", product.UserAgent)
	headers.Set("Authorization", "Cloud-IDE-JWT "+credential.AccessToken)
	headers.Set("X-Cloudide-Token", credential.AccessToken)
	headers.Set("X-Ide-Token", credential.AccessToken)
	headers.Set("X-Uid", credential.UID)
	headers.Set("X-App-Id", product.AppID)
	headers.Set("X-App-Version", "default")
	headers.Set("X-Ide-Version", product.IDEVersion)
	headers.Set("X-Ide-Version-Code", product.IDECode)
	headers.Set("X-App-Version-Code", product.IDECode)
	headers.Set("X-Ide-Version-Type", "stable")
	headers.Set("X-Device-Type", "macos")
	headers.Set("X-OS-Version", product.OSVersion)
	headers.Set("X-Device-Brand", product.DeviceBrand)
	headers.Set("Request-Traffic-Type", "prod")
	if credential.MachineID != "" {
		headers.Set("X-Machine-Id", deriveRotatingMachineID(credential.MachineID, machineIDGeneration))
	}
	if credential.DeviceID != "" {
		headers.Set("X-Device-Id", credential.DeviceID)
	}
	return headers
}

// checkinHeaders builds the full client header set the check-in and balance
// endpoints expect (trae.ts:253-286). The device identity is derived from the
// account's uid so that every account presents a stable, distinct device
// (docs/agents/trae.md:422-448).
//
// The X-Request-Id / X-Tt-Trace-Id values are per-request; the function reports
// an error only if the system random source fails.
func checkinHeaders(credential *Credential, product product, uid string) (http.Header, error) {
	traceRandom, errTrace := randomHexBytes(8) // 16 hex characters, trae.ts:261
	if errTrace != nil {
		return nil, errTrace
	}
	requestID, errRequest := uuidV4()
	if errRequest != nil {
		return nil, errRequest
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	// Kept verbatim from the reference client. The host transport does not
	// transparently decompress when Accept-Encoding is set explicitly, so
	// postJSON decodes gzip/deflate itself.
	headers.Set("Accept-Encoding", "gzip, deflate")
	headers.Set("Accept-Language", "zh-CN")
	headers.Set("User-Agent", "VSCode 1.107.1 (TRAE SOLO CN)")
	headers.Set("Authorization", "Cloud-IDE-JWT "+credential.AccessToken)
	headers.Set("X-Market-Client-Id", "VSCode 1.107.1")
	headers.Set("X-Market-User-Id", deriveMarketUserID(uid))
	headers.Set("X-User-Region", "CN")
	headers.Set("X-Device-Id", deriveDeviceID15(uid))
	headers.Set("X-Lgw-Req-Sdk-Type", "3")
	headers.Set("Package-Type", "stable_cn")
	headers.Set("X-Lscbd-Aid", "787976")
	headers.Set("X-Lscbd-Platform", "windows")
	headers.Set("App-Version", product.IDEVersion)
	headers.Set("X-Tt-Trace-Id", "00-"+traceRandom+"-01")
	headers.Set("Vscode-Sessionid", deriveSessionID(uid))
	headers.Set("X-Request-Id", requestID)
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "no-cors")
	headers.Set("Sec-Fetch-Site", "none")
	return headers, nil
}

// oauthHeaders builds the ExchangeToken / GetUserInfo headers (trae.ts:380-386).
// GetUserInfo additionally needs `X-Cloudide-Token`, which the caller adds.
func oauthHeaders(product product) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", product.UserAgent)
	return headers
}
