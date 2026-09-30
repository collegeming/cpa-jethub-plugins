package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The ZCode upstream client (quota, check-in, model catalogue, inference).
//
// What makes this provider different from its siblings is that ONE header set is
// reused across three call families, and the AUTH REQUIREMENT DIFFERS PER
// ENDPOINT. Measured by the reference (`zcode-upstream.ts:20-42`):
//
//	| endpoint                            | Authorization | X-Device-Mid |
//	|-------------------------------------|---------------|--------------|
//	| GET  /zcode-plan/billing/balance    | YES (401)     | YES (400/3001)|
//	| GET  /zcode-plan/billing/preview    | no            | YES          |
//	| POST /api/v1/event/report           | no            | YES          |
//	| POST /zcode-plan/billing/claim      | YES           | YES (+captcha)|
//	| GET  /api/v1/client/configs         | YES           | YES          |
//
// Both mistakes were made and observed: omitting `Authorization` on the balance
// call yields 401, and omitting `X-Device-Mid` yields
// `400 {"code":3001,"msg":"parameter error"}`.
//
// ⚠ The captcha header is deliberately NOT part of this header set. It was
// measured to be unnecessary for inference (with a correct identity block a
// request returns 200 with no captcha header at all, and even a deliberately
// bogus captcha parameter returns 200), and the official client's own bundle now
// attaches it to the claim endpoint alone. The plugin therefore never mints one
// and reports plainly if the server ever demands one.

// buildHeaders assembles the official client's "source identity" header set.
//
// These are the headers the official client sends in real traffic. They are NOT
// the `3012` criterion — that is the request body — but they are part of looking
// like the official client, and `X-Device-Mid` is a hard requirement.
//
// `jsonContent` adds `Content-Type`; the reference's own default is `true`, so
// every caller that posts a body keeps it and only the GET paths drop it.
func buildHeaders(credential *Credential, cfg Config, options headerOptions) http.Header {
	appVersion := credential.appVersionOrDefault(cfg)
	header := http.Header{}
	header.Set("User-Agent", "ZCode/"+appVersion)
	header.Set("HTTP-Referer", Origin)
	header.Set("X-ZCode-App-Version", appVersion)
	header.Set("X-Release-Channel", "stable")
	header.Set("X-Client-Language", cfg.ClientLanguage)
	header.Set("X-Client-Timezone", cfg.ClientTimezone)
	header.Set("X-Device-Mid", credential.DeviceMid)
	header.Set("X-Platform", cfg.Platform)
	header.Set("X-Os-Category", cfg.OSGroup)
	header.Set("anthropic-version", "2023-06-01")
	if options.JSON {
		header.Set("Content-Type", "application/json")
	}
	if options.Accept != "" {
		header.Set("Accept", options.Accept)
	}
	if options.Authorization {
		header.Set("Authorization", credential.Bearer())
	}
	return header
}

// headerOptions selects the endpoint-specific parts of the header set.
type headerOptions struct {
	// JSON adds `Content-Type: application/json`.
	JSON bool
	// Authorization adds the bearer token. Only the endpoints that document it
	// set this; sending it where it is not wanted is harmless but undocumented,
	// so the distinction is kept.
	Authorization bool
	// Accept is the `Accept` header value.
	Accept string
}

// hostRequest performs one buffered upstream call through the host transport.
//
// Every outbound call goes through the host so proxy, TLS and request logging
// stay under the host's control: the plugin never opens a socket itself.
func hostRequest(h *abiboot.Host, method, rawURL string, headers http.Header, body []byte) (*pluginapi.HTTPResponse, error) {
	if h == nil {
		return nil, transportError("host_unavailable", "插件未通过宿主调用（缺少 host 句柄）")
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: headers, Body: body})
}

// reencodeBody re-marshals a decoded JSON object.
func reencodeBody(body map[string]any) ([]byte, error) {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, statusError(false, "encode_request", http.StatusInternalServerError, "编码请求体失败：%v", errMarshal)
	}
	return encoded, nil
}

// decodeJSONObject decodes a request or response body into a generic object.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&object); errDecode != nil {
		return nil, requestError("invalid_request", "解码 JSON 对象失败：%v", errDecode)
	}
	return object, nil
}

// nonNegativeInt64 reads a JSON number that may also arrive as a numeric string.
func nonNegativeInt64(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int:
		if typed < 0 {
			return 0
		}
		return int64(typed)
	case int64:
		if typed < 0 {
			return 0
		}
		return typed
	case float64:
		if typed < 0 || typed > float64(^uint64(0)>>1) {
			return 0
		}
		return int64(typed)
	case json.Number:
		parsed, errParse := typed.Int64()
		if errParse != nil || parsed < 0 {
			return 0
		}
		return parsed
	case string:
		parsed, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if errParse != nil || parsed < 0 {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// unixSecondsToRFC3339 renders a Unix-second timestamp, or "" when absent.
//
// The quota bucket's `resetTime` is a string field in the host's type, so a zero
// timestamp has to become the empty string rather than 1970-01-01.
func unixSecondsToRFC3339(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

// nonNegativeInt reads a JSON number that may also arrive as a numeric string.
func nonNegativeInt(value any) (int, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, errParse := typed.Int64()
		if errParse != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(typed))
		if errParse != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// boolField reads a boolean field, accepting a string spelling as well.
func boolField(source map[string]any, key string) bool {
	if source == nil {
		return false
	}
	switch typed := source[key].(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "1":
			return true
		}
	}
	return false
}

// stringField reads a non-empty string field.
func stringField(source map[string]any, key string) string {
	if source == nil {
		return ""
	}
	if text, ok := source[key].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
