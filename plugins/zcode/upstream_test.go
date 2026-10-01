package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Low-level upstream helpers and numeric-boundary tests.
//
// These are kept separate from the provider-flow tests because the failures are
// usually silent: a JSON number outside Go's `int` range becomes a negative count on
// 32-bit builds, a missing header turns into a business code with HTTP 200, and a URL
// escape mismatch only shows up for a future version string containing punctuation.

// TestBuildHeadersMatchesTheAuthoritativeList pins the official client's source-
// identity header set.
func TestBuildHeadersMatchesTheAuthoritativeList(t *testing.T) {
	credential := sampleCredential()
	cfg := DefaultConfig()
	headers := buildHeaders(credential, cfg, headerOptions{
		JSON: true, Authorization: true, Accept: "text/event-stream",
	})
	expected := map[string]string{
		"Authorization":       "Bearer " + credential.ZCodeJWT,
		"Content-Type":        "application/json",
		"Accept":              "text/event-stream",
		"anthropic-version":   "2023-06-01",
		"HTTP-Referer":        Origin,
		"X-Device-Mid":        credential.DeviceMid,
		"X-ZCode-App-Version": credential.AppVersion,
		"X-Release-Channel":   "stable",
		"X-Client-Language":   DefaultClientLanguage,
		"X-Client-Timezone":   DefaultClientTimezone,
		"X-Platform":          DefaultPlatform,
		"X-Os-Category":       DefaultOSCategory,
		"User-Agent":          "ZCode/" + credential.AppVersion,
	}
	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

// TestBuildHeadersHonoursTheEndpointSpecificOptions covers the differences among
// balance, preview and event-report requests.
func TestBuildHeadersHonoursTheEndpointSpecificOptions(t *testing.T) {
	credential := sampleCredential()
	cfg := DefaultConfig()
	cases := []struct {
		name              string
		options           headerOptions
		wantAuthorization bool
		wantContentType   bool
		wantAccept        string
	}{
		{"balance", headerOptions{Authorization: true, Accept: "application/json"}, true, false, "application/json"},
		{"preview", headerOptions{Accept: "application/json"}, false, false, "application/json"},
		{"event report", headerOptions{JSON: true, Accept: "application/json"}, false, true, "application/json"},
		{"inference", headerOptions{JSON: true, Authorization: true, Accept: "text/event-stream"}, true, true, "text/event-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := buildHeaders(credential, cfg, tc.options)
			if (headers.Get("Authorization") != "") != tc.wantAuthorization {
				t.Errorf("Authorization present = %v, want %v", headers.Get("Authorization") != "", tc.wantAuthorization)
			}
			if (headers.Get("Content-Type") != "") != tc.wantContentType {
				t.Errorf("Content-Type present = %v, want %v", headers.Get("Content-Type") != "", tc.wantContentType)
			}
			if headers.Get("Accept") != tc.wantAccept {
				t.Errorf("Accept = %q, want %q", headers.Get("Accept"), tc.wantAccept)
			}
			// X-Device-Mid is unconditional: every endpoint in this family needs it.
			if headers.Get("X-Device-Mid") != credential.DeviceMid {
				t.Errorf("X-Device-Mid = %q", headers.Get("X-Device-Mid"))
			}
			for name := range headers {
				if name == "X-Aliyun-Captcha-Verify-Param" || name == "X-Aliyun-Captcha-Verify-Region" {
					t.Errorf("a deprecated captcha header %s is present", name)
				}
			}
		})
	}
}

// TestBuildHeadersFallsBackToTheConfiguredVersion covers an imported credential
// written before `app_version` was persisted.
func TestBuildHeadersFallsBackToTheConfiguredVersion(t *testing.T) {
	credential := sampleCredential()
	credential.AppVersion = ""
	cfg := DefaultConfig()
	cfg.AppVersion = "7.6.5"
	headers := buildHeaders(credential, cfg, headerOptions{})
	if got := headers.Get("X-ZCode-App-Version"); got != "7.6.5" {
		t.Errorf("version header = %q, want the configured fallback", got)
	}
	if got := headers.Get("User-Agent"); got != "ZCode/7.6.5" {
		t.Errorf("user agent = %q", got)
	}
}

// TestHostRequestNeverDialsWithoutAHost covers the repository's core rule: the
// plugin must never open a network connection itself.
func TestHostRequestNeverDialsWithoutAHost(t *testing.T) {
	response, errDo := hostRequest(nil, http.MethodGet, Origin+BalancePath, http.Header{}, nil)
	if response != nil {
		t.Fatalf("response = %#v, want nil", response)
	}
	if errDo == nil {
		t.Fatal("a request without a host handle succeeded")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errDo, envelope) {
		t.Fatalf("error type = %T", errDo)
	}
	if envelope.Code != "host_unavailable" {
		t.Errorf("code = %q, want host_unavailable", envelope.Code)
	}
}

// TestHostRequestGoesThroughHostHTTPDo covers the positive path: the fake host sees
// the exact request because no other transport exists.
func TestHostRequestGoesThroughHostHTTPDo(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL != Origin+MessagesPath {
			t.Errorf("url = %q", request.URL)
		}
		if string(request.Body) != `{"x":1}` {
			t.Errorf("body = %q", request.Body)
		}
		return httpResponse(http.StatusAccepted, `{"ok":true}`), nil
	}
	response, errDo := hostRequest(testHost(), http.MethodPost, Origin+MessagesPath,
		http.Header{"X-Test": {"yes"}}, []byte(`{"x":1}`))
	if errDo != nil {
		t.Fatalf("hostRequest: %v", errDo)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", response.StatusCode)
	}
	calls := fake.callsFor(MessagesPath)
	if len(calls) != 1 {
		t.Fatalf("host calls = %d, want 1", len(calls))
	}
	if calls[0].Headers.Get("X-Test") != "yes" {
		t.Errorf("custom header = %q", calls[0].Headers.Get("X-Test"))
	}
}

// TestNonNegativeIntAcceptsWireSpellings covers the generic number reader.
func TestNonNegativeIntAcceptsWireSpellings(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
		ok   bool
	}{
		{"int", 7, 7, true},
		{"int64", int64(8), 8, true},
		{"float64", float64(9), 9, true},
		{"json number", json.Number("10"), 10, true},
		{"numeric string", "11", 11, true},
		{"string with whitespace", " 12 ", 12, true},
		{"nil", nil, 0, false},
		{"bad string", "nope", 0, false},
		{"an object", map[string]any{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nonNegativeInt(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("nonNegativeInt(%#v) = %d,%v; want %d,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestNonNegativeInt64KeepsLargeTokenCounts covers the count fields that may exceed
// a 32-bit int.
func TestNonNegativeInt64KeepsLargeTokenCounts(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"an int64", int64(9_000_000_000), 9_000_000_000},
		{"a json number", json.Number("9000000000"), 9_000_000_000},
		{"a string", "9000000000", 9_000_000_000},
		{"a float", float64(9_000_000_000), 9_000_000_000},
		{"a negative int", int64(-1), 0},
		{"a negative string", "-2", 0},
		{"a bad string", "no", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nonNegativeInt64(tc.in); got != tc.want {
				t.Fatalf("nonNegativeInt64(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestURLQueryEscapeMatchesURLQueryEscape covers the handwritten escape helper.
//
// No network is involved: it is compared against the standard library for the
// values this provider actually emits.
func TestURLQueryEscapeMatchesURLQueryEscape(t *testing.T) {
	cases := []string{
		"3.14.3",
		"win32",
		"Asia/Shanghai",
		"x y",
		"x+y",
		"中文",
		"~._-",
		"",
	}
	for _, input := range cases {
		got := urlQueryEscape(input)
		want := url.QueryEscape(input)
		// The two legal space spellings differ: the standard library uses `+`, the
		// plugin uses `%20`. Normalise them before comparison.
		want = strings.ReplaceAll(want, "+", "%20")
		if got != want {
			t.Errorf("urlQueryEscape(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestUnixSecondsToRFC3339 covers quota reset-time rendering.
func TestUnixSecondsToRFC3339(t *testing.T) {
	if got := unixSecondsToRFC3339(0); got != "" {
		t.Fatalf("zero timestamp = %q, want empty", got)
	}
	if got := unixSecondsToRFC3339(1_700_000_000); got != "2023-11-14T22:13:20Z" {
		t.Fatalf("timestamp = %q", got)
	}
}

// TestJSONBodyRoundTripUsesNumbersWithoutPrecisionLoss covers token counts in the
// response body.
func TestJSONBodyRoundTripUsesNumbersWithoutPrecisionLoss(t *testing.T) {
	value := int64(9_000_000_000)
	body := jsonBody(t, map[string]any{"code": 0, "data": map[string]any{
		"balances": []map[string]any{{"remaining_units": value, "total_units": value}},
	}})
	var parsed map[string]any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&parsed); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	data, _ := parsed["data"].(map[string]any)
	balances, _ := data["balances"].([]any)
	bucket, _ := balances[0].(map[string]any)
	if got := nonNegativeInt64(bucket["remaining_units"]); got != value {
		t.Fatalf("remaining = %d, want %d", got, value)
	}
}

func TestNonNegativeInt64HandlesTheLargestSignedValue(t *testing.T) {
	const max = int64(^uint64(0) >> 1)
	if got := nonNegativeInt64(json.Number("9223372036854775807")); got != max {
		t.Fatalf("max int64 = %d, want %d", got, max)
	}
	if got := nonNegativeInt64("9223372036854775808"); got != 0 {
		t.Fatalf("overflow = %d, want 0", got)
	}
}

// TestSleepMillisReturnsImmediatelyForNonPositiveValues covers the test-friendly
// zero-backoff semantics.
func TestSleepMillisReturnsImmediatelyForNonPositiveValues(t *testing.T) {
	start := time.Now()
	sleepMillis(0)
	sleepMillis(-1)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("non-positive sleeps took %s", elapsed)
	}
}
