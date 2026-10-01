package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Credits / check-in / quota tests.
//
// Three measured facts shape these:
//
//   - `billing/preview` is EMPTY until the client has reported activity. Without
//     `event/report` with `app_launch` and `app_daily_active`, the daily benefit is
//     never offered, and a page that queries without reporting first shows "nothing
//     to claim" every single day;
//   - the balance buckets publish `unit_type` (measured `"token"`) and `meter`, so
//     the panel renders tokens rather than a bare number that reads like credits;
//   - `code:1003` on claim is an IDEMPOTENT SUCCESS. Treating it as an error makes
//     a scheduled check-in report a false failure on every subsequent run.

// balanceBody renders a balance response in the measured shape.
func balanceBody(remaining, total int64) string {
	encoded, _ := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{
			"displayMode": "standard",
			"balances": []map[string]any{{
				"plan_id":         "zcode-v3-start-plan-trust-abc",
				"show_name":       "GLM-5.3-Flash",
				"unit_type":       "token",
				"meter":           "model_usage",
				"total_units":     total,
				"used_units":      total - remaining,
				"remaining_units": remaining,
				"expires_at":      nowMillis()/1000 + 86_400,
			}},
		},
	})
	return string(encoded)
}

// TestFetchBalanceRequiresBothCredentialsHeaders covers the measured auth contract.
//
// ⚠ `Authorization` is required here (401 without it) and so is `X-Device-Mid`
// (`400 {"code":3001}` without it). The balance call is the only read-only endpoint
// that exercises BOTH, which is why it doubles as the credential probe.
func TestFetchBalanceRequiresBothCredentialsHeaders(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Headers.Get("Authorization") == "" {
			return httpResponse(http.StatusUnauthorized, `{"code":1002,"msg":"unauthorized"}`), nil
		}
		if request.Headers.Get("X-Device-Mid") == "" {
			return httpResponse(http.StatusBadRequest, `{"code":3001,"msg":"parameter error"}`), nil
		}
		return httpResponse(http.StatusOK, balanceBody(94_539_275, 100_000_000)), nil
	}
	credential := sampleCredential()

	balance, errBalance := fetchBalance(testHost(), credential, settings())
	if errBalance != nil {
		t.Fatalf("fetchBalance: %v", errBalance)
	}
	if balance.Remaining != 94_539_275 {
		t.Fatalf("remaining = %d, want the bucket's value", balance.Remaining)
	}
	if balance.Total != 100_000_000 {
		t.Errorf("total = %d", balance.Total)
	}
	if balance.PlanName != "GLM-5.3-Flash" {
		t.Errorf("plan name = %q", balance.PlanName)
	}
	if len(balance.Buckets) != 1 {
		t.Fatalf("bucket count = %d", len(balance.Buckets))
	}
	// ⚠ The unit fields must be carried through: they are what makes the panel
	// render tokens instead of a bare number.
	if balance.Buckets[0].UnitType != "token" {
		t.Errorf("unit_type = %q, want token", balance.Buckets[0].UnitType)
	}
	if balance.Buckets[0].Meter != "model_usage" {
		t.Errorf("meter = %q, want model_usage", balance.Buckets[0].Meter)
	}

	// With the device id removed the call must fail as a parameter error, not as a
	// successful zero balance.
	broken := sampleCredential()
	broken.DeviceMid = ""
	if _, errBroken := fetchBalance(testHost(), broken, settings()); errBroken == nil {
		t.Fatal("a balance call without X-Device-Mid succeeded")
	} else {
		envelope := &abiboot.EnvelopeError{}
		if !asEnvelope(errBroken, envelope) {
			t.Fatalf("error type = %T", errBroken)
		}
		if envelope.HTTPStatus != http.StatusBadRequest {
			t.Errorf("http status = %d, want 400 for a parameter error", envelope.HTTPStatus)
		}
	}
}

// TestParseBalanceEnterpriseModePublishesNoNumbers covers the enterprise shape.
//
// `displayMode: "enterprise"` gives a link instead of numbers. Rendering that as 0
// would read as "used up", which is the opposite of the truth.
func TestParseBalanceEnterpriseModePublishesNoNumbers(t *testing.T) {
	data := map[string]any{"displayMode": "enterprise"}
	balance := parseBalance(data)
	if !balance.Enterprise {
		t.Fatal("the enterprise mode was not recognised")
	}
	if len(balance.Buckets) != 0 || balance.Remaining != 0 || balance.Total != 0 {
		t.Fatalf("the enterprise balance published numbers: %#v", balance)
	}
}

// TestParseBalancePrefersAvailableUnits covers the bucket aggregation.
func TestParseBalancePrefersAvailableUnits(t *testing.T) {
	data := map[string]any{
		"balances": []any{
			map[string]any{
				"plan_id": "a", "show_name": "A", "unit_type": "token",
				"total_units": float64(1000), "remaining_units": float64(400), "available_units": float64(300),
			},
			map[string]any{
				"plan_id": "b", "show_name": "B", "unit_type": "token",
				"total_units": float64(500), "remaining_units": float64(100),
				"expires_at": float64(1_700_000_000),
			},
		},
	}
	balance := parseBalance(data)
	// `available_units` wins where present, `remaining_units` otherwise.
	if balance.Remaining != 400 {
		t.Fatalf("remaining = %d, want 300 + 100", balance.Remaining)
	}
	if balance.Total != 1500 {
		t.Errorf("total = %d, want 1500", balance.Total)
	}
	if balance.ExpiresAt != 1_700_000_000 {
		t.Errorf("expires at = %d, want the only expiry present", balance.ExpiresAt)
	}
}

// TestParseBalanceToleratesMissingAndMalformedFields covers the lenient read.
func TestParseBalanceToleratesMissingAndMalformedFields(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"no buckets", map[string]any{}},
		{"a null balances member", map[string]any{"balances": nil}},
		{"a non-array balances member", map[string]any{"balances": "nope"}},
		{"an entry that is not an object", map[string]any{"balances": []any{"nope", 42}}},
		{"numeric strings are accepted", map[string]any{"balances": []any{
			map[string]any{"remaining_units": "1234", "total_units": "2000"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			balance := parseBalance(tc.data)
			if balance == nil {
				t.Fatal("parseBalance returned nil")
			}
			if balance.Enterprise {
				t.Error("a normal response was read as enterprise")
			}
		})
	}
	// The string-number case must actually parse.
	balance := parseBalance(cases[4].data)
	if balance.Remaining != 1234 {
		t.Fatalf("remaining = %d, want the numeric string to be parsed", balance.Remaining)
	}
}

func TestQuotaDescribeDeclaresNoReset(t *testing.T) {
	value, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("quota.describe: %v", errDescribe)
	}
	response := decodeResult[pluginapi.QuotaDescribeResponse](t, value)
	if len(response.SupportedProviders) != 1 || response.SupportedProviders[0] != ProviderKey {
		t.Fatalf("supported providers = %v", response.SupportedProviders)
	}
	if response.SupportsReset {
		t.Error("a reset capability was declared, but the allowance resets on the server's own schedule")
	}
	value, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("quota.reset: %v", errReset)
	}
	if decodeResult[pluginapi.QuotaResetResponse](t, value).Success {
		t.Error("quota.reset reported success, but there is no reset endpoint")
	}
}

// TestQuotaFetchRendersTokensNotCredits is the unit rule.
//
// ⚠ A real defect came from rendering `remaining_units` as a generic number: the UI
// showed `94539275`, which reads like ninety-four million credits rather than
// 94.54M tokens. The unit is therefore published explicitly, from upstream's own
// `unit_type` field.
func TestQuotaFetchRendersTokensNotCredits(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	entry := storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, balanceBody(94_539_275, 100_000_000)), nil
	}
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{
		AuthIndex: entry.AuthIndex, AuthID: entry.ID, Provider: ProviderKey,
	})
	value, errFetch := handleQuotaFetch(testHost(), raw)
	if errFetch != nil {
		t.Fatalf("quota.fetch: %v", errFetch)
	}
	response := decodeResult[pluginapi.QuotaFetchResponse](t, value)

	var remaining *pluginapi.QuotaMetric
	for index := range response.Summary {
		if response.Summary[index].Key == "remaining" {
			remaining = &response.Summary[index]
		}
	}
	if remaining == nil {
		t.Fatalf("no remaining metric was published: %#v", response.Summary)
	}
	if remaining.Value != 94_539_275 {
		t.Errorf("value = %v, want the raw count", remaining.Value)
	}
	// The unit is a TOKEN magnitude, taken from upstream's own field.
	if !strings.Contains(remaining.Unit, "94.54M") || !strings.Contains(remaining.Unit, "tokens") {
		t.Fatalf("unit = %q, want it to render 94.54M tokens", remaining.Unit)
	}
	if strings.Contains(remaining.Unit, "积分") || strings.Contains(remaining.Unit, "credit") {
		t.Fatalf("unit = %q, which presents tokens as credits", remaining.Unit)
	}

	// The per-bucket group carries a fraction and the upstream reset time.
	if len(response.Groups) == 0 || len(response.Groups[0].Buckets) == 0 {
		t.Fatalf("no bucket group was published: %#v", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.RemainingFraction <= 0 || bucket.RemainingFraction > 1 {
		t.Errorf("fraction = %v, want it inside (0,1]", bucket.RemainingFraction)
	}
	if bucket.ResetTime == "" {
		t.Error("no reset time was published although the bucket carries one")
	}
	if !strings.Contains(bucket.Description, "token") {
		t.Errorf("the bucket description does not name the unit: %q", bucket.Description)
	}
}

// TestQuotaFetchReportsTheEnterpriseShape covers the no-numbers case.
func TestQuotaFetchReportsTheEnterpriseShape(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	entry := storedCredential(t, fake, "auth-1", "zcode-1.json", sampleCredential())
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"displayMode":"enterprise"}}`), nil
	}
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{AuthIndex: entry.AuthIndex, Provider: ProviderKey})
	value, errFetch := handleQuotaFetch(testHost(), raw)
	if errFetch != nil {
		t.Fatalf("quota.fetch: %v", errFetch)
	}
	response := decodeResult[pluginapi.QuotaFetchResponse](t, value)
	if len(response.Summary) != 1 {
		t.Fatalf("summary = %#v, want one explanatory metric", response.Summary)
	}
	// The unit says "enterprise" rather than presenting a zero, which would read
	// as "used up".
	if !strings.Contains(response.Summary[0].Unit, "企业版") {
		t.Fatalf("unit = %q, want it to name the enterprise shape", response.Summary[0].Unit)
	}
}

// TestQuotaFetchWithoutACredentialIsAnAuthFailure covers the missing-credential path.
func TestQuotaFetchWithoutACredentialIsAnAuthFailure(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{Provider: ProviderKey})
	_, errFetch := handleQuotaFetch(testHost(), raw)
	if errFetch == nil {
		t.Fatal("a quota fetch with no credential succeeded")
	}
	envelope := &abiboot.EnvelopeError{}
	if !asEnvelope(errFetch, envelope) {
		t.Fatalf("error type = %T", errFetch)
	}
	if envelope.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http status = %d, want 401", envelope.HTTPStatus)
	}
}

// TestFormatTokenMagnitude covers the magnitude rendering.
func TestFormatTokenMagnitude(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1_000, "1K"},
		{1_500, "1.5K"},
		{94_539_275, "94.54M"},
		{100_000_000, "100M"},
		{1_000_000, "1M"},
		{1_000_000_000, "1B"},
		{1_234_567_890, "1.23B"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := formatTokenMagnitude(tc.in); got != tc.want {
				t.Fatalf("formatTokenMagnitude(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClampFraction covers the negative-balance guard.
//
// The server can send a negative remainder after a metering rollback, and passing
// that through renders a negative progress bar.
func TestClampFraction(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-0.5, 0},
		{0, 0},
		{0.5, 0.5},
		{1, 1},
		{2, 1},
	}
	for _, tc := range cases {
		if got := clampFraction(tc.in); got != tc.want {
			t.Errorf("clampFraction(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
