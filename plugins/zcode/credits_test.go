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

// TestActivityReportsPrecedeThePreview is the check-in ordering rule.
//
// ⚠ Without the two events the preview is empty every day: the server decides
// whether to offer the daily benefit from the activity signal. That is why the
// reference records "daily random distribution" as a misinterpretation — it is not
// a push at all.
func TestActivityReportsPrecedeThePreview(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	var order []string
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, EventReportPath):
			var payload map[string]any
			_ = json.Unmarshal(request.Body, &payload)
			order = append(order, "event:"+stringField(payload, "event"))
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		case strings.Contains(request.URL, PreviewPath):
			order = append(order, "preview")
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		default:
			t.Fatalf("unexpected call to %s", request.URL)
			return nil, nil
		}
	}

	if _, errStatus := fetchCheckinStatus(testHost(), sampleCredential(), settings()); errStatus != nil {
		t.Fatalf("fetchCheckinStatus: %v", errStatus)
	}

	// Both events were reported, and both BEFORE the preview.
	want := []string{"event:app_launch", "event:app_daily_active", "preview"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", order, want)
	}

	// The event body carries the device id, which is what the server de-duplicates
	// on, and it needs no bearer token.
	events := fake.callsFor(EventReportPath)
	if len(events) != 2 {
		t.Fatalf("event calls = %d, want 2", len(events))
	}
	for _, call := range events {
		if call.Headers.Get("X-Device-Mid") == "" {
			t.Error("an activity report was sent without X-Device-Mid")
		}
		if call.Headers.Get("Authorization") != "" {
			t.Error("an activity report carried a bearer token, which this endpoint does not want")
		}
		var payload map[string]any
		if errUnmarshal := json.Unmarshal(call.Body, &payload); errUnmarshal != nil {
			t.Fatalf("decode event body: %v", errUnmarshal)
		}
		if payload["device_mid"] != "8f14e45f-ceea-467a-9c1c-1b2c3d4e5f60" {
			t.Errorf("event device_mid = %v", payload["device_mid"])
		}
		if payload["platform"] != "win32" {
			t.Errorf("event platform = %v", payload["platform"])
		}
		if payload["app_version"] == "" {
			t.Error("the event carries no client version")
		}
	}
}

// TestPreviewDoesNotSendABearerToken covers the per-endpoint auth difference.
//
// The reference measured this: `preview` needs only `X-Device-Mid`, while `balance`
// needs both. Treating them alike is harmless here but wrong in the other direction,
// which is why the distinction is pinned.
func TestPreviewDoesNotSendABearerToken(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, PreviewPath) {
			if request.Headers.Get("Authorization") != "" {
				t.Error("the preview carried a bearer token")
			}
			if request.Headers.Get("X-Device-Mid") == "" {
				t.Error("the preview omitted X-Device-Mid, which it requires")
			}
			// The query carries the client version and platform.
			if !strings.Contains(request.URL, "app_version=") || !strings.Contains(request.URL, "platform=win32") {
				t.Errorf("preview url = %q, want app_version and platform", request.URL)
			}
		}
		return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
	}
	if _, errPlans := fetchClaimablePlans(testHost(), sampleCredential(), settings()); errPlans != nil {
		t.Fatalf("fetchClaimablePlans: %v", errPlans)
	}
}

// TestClaimablePlansAreSortedByDescendingPriority covers the claim order, which is
// what both the official client and the reference use.
func TestClaimablePlansAreSortedByDescendingPriority(t *testing.T) {
	data := map[string]any{
		"plans": []any{
			map[string]any{"plan_id": "low", "priority": float64(1)},
			map[string]any{"plan_id": "high", "priority": float64(9), "name": "the big one"},
			map[string]any{"plan_id": "mid", "priority": float64(5)},
			// A plan with no id is dropped rather than claimed blindly.
			map[string]any{"priority": float64(100)},
		},
	}
	plans := parseClaimablePlans(data)
	if len(plans) != 3 {
		t.Fatalf("plan count = %d, want 3", len(plans))
	}
	order := []string{plans[0].PlanID, plans[1].PlanID, plans[2].PlanID}
	if strings.Join(order, ",") != "high,mid,low" {
		t.Fatalf("order = %v, want high,mid,low", order)
	}
	if plans[0].Name != "the big one" {
		t.Errorf("name = %q", plans[0].Name)
	}
}

// TestCheckinStatusTreatsAnEmptyPlanListAsCheckedIn covers the state inference.
//
// ZCode has no separate "already claimed today" endpoint, so the only signal is
// whether anything is claimable. The page says so rather than asserting which of the
// two causes applies.
func TestCheckinStatusTreatsAnEmptyPlanListAsCheckedIn(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, PreviewPath) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}
	status, errStatus := fetchCheckinStatus(testHost(), sampleCredential(), settings())
	if errStatus != nil {
		t.Fatalf("fetchCheckinStatus: %v", errStatus)
	}
	if !status.Active {
		t.Error("the activity is reported as inactive; it must always be active once a credential exists")
	}
	if !status.TodayCheckedIn {
		t.Error("an empty plan list was not read as already-checked-in")
	}
	// The note must not assert a single cause, because the two are
	// indistinguishable from here.
	if !strings.Contains(status.Note, "可能") {
		t.Errorf("the note asserts one cause: %q", status.Note)
	}
	if !strings.Contains(status.Note, "活跃上报") {
		t.Errorf("the note does not mention the activity-report dependency: %q", status.Note)
	}
}

// TestCheckinStatusReportsClaimablePlans covers the positive case.
func TestCheckinStatusReportsClaimablePlans(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, PreviewPath) {
			return httpResponse(http.StatusOK, jsonBody(t, map[string]any{
				"code": 0,
				"data": map[string]any{
					"plans": []map[string]any{{
						"plan_id": "zcode-v3-start-plan-trust-abc", "priority": float64(10), "name": "Start Plan",
					}},
				},
			})), nil
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}
	status, errStatus := fetchCheckinStatus(testHost(), sampleCredential(), settings())
	if errStatus != nil {
		t.Fatalf("fetchCheckinStatus: %v", errStatus)
	}
	if status.TodayCheckedIn {
		t.Error("a claimable plan was read as already checked in")
	}
	if len(status.Claimable) != 1 || status.Claimable[0].PlanID != "zcode-v3-start-plan-trust-abc" {
		t.Fatalf("claimable plans = %#v", status.Claimable)
	}
	if status.ActivityName == "" {
		t.Error("the activity has no display name")
	}
}

// TestClaimDailyClaimsEveryPlan covers the claim loop.
func TestClaimDailyClaimsEveryPlan(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	claimed := []string{}
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, EventReportPath):
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[
				{"plan_id":"second","priority":1},{"plan_id":"first","priority":9}]}}`), nil
		case strings.HasSuffix(request.URL, ClaimPath):
			var payload map[string]any
			_ = json.Unmarshal(request.Body, &payload)
			claimed = append(claimed, stringField(payload, "plan_id"))
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		default:
			t.Fatalf("unexpected call to %s", request.URL)
			return nil, nil
		}
	}

	outcomes, errClaim := claimDaily(testHost(), sampleCredential(), settings())
	if errClaim != nil {
		t.Fatalf("claimDaily: %v", errClaim)
	}
	// Highest priority first.
	if strings.Join(claimed, ",") != "first,second" {
		t.Fatalf("claimed order = %v, want first,second", claimed)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcome count = %d", len(outcomes))
	}
	for _, outcome := range outcomes {
		if !outcome.OK {
			t.Errorf("plan %s was not claimed: %s", outcome.PlanID, outcome.Message)
		}
	}

	// ⚠ The claim was sent WITHOUT a captcha header. The official client's bundle
	// attaches one here, but this plugin mints none: inference was measured to need
	// none, and the claim endpoint works without it.
	claims := fake.callsFor(ClaimPath)
	if len(claims) != 2 {
		t.Fatalf("claim calls = %d, want 2", len(claims))
	}
	for _, call := range claims {
		if call.Headers.Get("Authorization") == "" {
			t.Error("a claim was sent without a bearer token, which it requires")
		}
		if call.Headers.Get("X-Device-Mid") == "" {
			t.Error("a claim was sent without X-Device-Mid, which it requires")
		}
		for name := range call.Headers {
			if strings.Contains(strings.ToLower(name), "captcha") {
				t.Errorf("a claim carried the captcha header %s", name)
			}
		}
	}
}

// TestClaimAlreadyClaimedIsSuccess is the idempotency rule.
//
// ⚠ `code:1003` means "already claimed". Treating it as a failure makes a scheduled
// check-in report a false error every time it runs, which is worse than a real
// failure because it never resolves.
func TestClaimAlreadyClaimedIsSuccess(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, EventReportPath):
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[{"plan_id":"p1","priority":1}]}}`), nil
		default:
			return httpResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
		}
	}
	outcomes, errClaim := claimDaily(testHost(), sampleCredential(), settings())
	if errClaim != nil {
		t.Fatalf("claimDaily: %v", errClaim)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcome count = %d", len(outcomes))
	}
	if !outcomes[0].OK {
		t.Fatal("1003 was reported as a failure, but it is the idempotent success")
	}
	if !outcomes[0].AlreadyClaimed {
		t.Error("the already-claimed flag was not set")
	}
}

// TestClaimDailyWithNothingToClaim covers the empty-preview branch.
func TestClaimDailyWithNothingToClaim(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, PreviewPath) {
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}
	outcomes, errClaim := claimDaily(testHost(), sampleCredential(), settings())
	if errClaim != nil {
		t.Fatalf("claimDaily: %v", errClaim)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcome count = %d, want one explanatory entry", len(outcomes))
	}
	// Reported as an idempotent success, because the user's action in both the
	// "already claimed" and the "nothing granted today" cases is the same: come
	// back tomorrow.
	if !outcomes[0].OK || !outcomes[0].AlreadyClaimed {
		t.Fatalf("an empty preview produced %#v, want an idempotent success", outcomes[0])
	}
	if !strings.Contains(outcomes[0].Message, "自然日") {
		t.Errorf("the message does not explain the daily refresh: %q", outcomes[0].Message)
	}
	// ⚠ No claim request was issued.
	if claims := fake.callsFor(ClaimPath); len(claims) != 0 {
		t.Fatalf("%d claim requests were issued although nothing was claimable", len(claims))
	}
}

// TestClaimFailuresAreReportedWithTheirCause covers the failure mapping.
func TestClaimFailuresAreReportedWithTheirCause(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantWord string
	}{
		{"an unknown plan", `{"code":1001,"msg":"plan not found"}`, "活动不存在"},
		{"an ended campaign", `{"code":1002,"msg":"ended"}`, "活动已结束"},
		{"an ineligible account", `{"code":1004,"msg":"not eligible"}`, "不符合领取条件"},
		{"an exhausted pool", `{"code":1005,"msg":"sold out"}`, "名额已用完"},
		{"a captcha demand", `{"code":3007,"msg":"captcha verify failed"}`, "人机验证"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.install(t)
			fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				switch {
				case strings.HasSuffix(request.URL, EventReportPath):
					return httpResponse(http.StatusOK, `{"code":0}`), nil
				case strings.Contains(request.URL, PreviewPath):
					return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[{"plan_id":"p1","priority":1}]}}`), nil
				default:
					return httpResponse(http.StatusOK, tc.body), nil
				}
			}
			outcomes, errClaim := claimDaily(testHost(), sampleCredential(), settings())
			if errClaim != nil {
				t.Fatalf("claimDaily: %v", errClaim)
			}
			if len(outcomes) != 1 {
				t.Fatalf("outcome count = %d", len(outcomes))
			}
			if outcomes[0].OK {
				t.Fatal("a failed claim was reported as successful")
			}
			if !strings.Contains(outcomes[0].Message, tc.wantWord) {
				t.Fatalf("message = %q, want it to explain %q", outcomes[0].Message, tc.wantWord)
			}
		})
	}
}

// TestClaimPastATransportFailureIsReportedPerPlan covers partial failures: one
// plan's transport error must not abort the rest of the sweep.
func TestClaimPastATransportFailureIsReportedPerPlan(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, EventReportPath):
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		case strings.Contains(request.URL, PreviewPath):
			return httpResponse(http.StatusOK, `{"code":0,"data":{"plans":[
				{"plan_id":"a","priority":2},{"plan_id":"b","priority":1}]}}`), nil
		default:
			var payload map[string]any
			_ = json.Unmarshal(request.Body, &payload)
			if stringField(payload, "plan_id") == "a" {
				return nil, errFakeTransport
			}
			return httpResponse(http.StatusOK, `{"code":0}`), nil
		}
	}
	outcomes, errClaim := claimDaily(testHost(), sampleCredential(), settings())
	if errClaim != nil {
		t.Fatalf("claimDaily: %v", errClaim)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcome count = %d, want 2", len(outcomes))
	}
	if outcomes[0].OK {
		t.Error("the failed plan was reported as claimed")
	}
	if !outcomes[1].OK {
		t.Errorf("the second plan was not attempted after the first failed: %s", outcomes[1].Message)
	}
}

// TestQuotaDescribeDeclaresNoReset covers the quota capability declaration.
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

// TestClaimWithoutACaptchaHeaderIsDeliberate documents the decision in an
// executable form, so a future change has to argue with the comment.
func TestClaimWithoutACaptchaHeaderIsDeliberate(t *testing.T) {
	fake := newFakeHost()
	fake.install(t)
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(request.URL, ClaimPath) {
			for name := range request.Headers {
				if strings.Contains(strings.ToLower(name), "captcha") {
					t.Fatalf("the claim carried %s; this plugin mints no captcha on purpose", name)
				}
			}
		}
		return httpResponse(http.StatusOK, `{"code":0}`), nil
	}
	outcome := claimPlan(testHost(), sampleCredential(), settings(), "p1")
	if !outcome.OK {
		t.Fatalf("claim failed: %+v", outcome)
	}
}
