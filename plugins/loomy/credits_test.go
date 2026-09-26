package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The read endpoint parses both pools, prefers `availableBalance` and never
// invents a number for a field the server did not send
// (`loomy-credits.ts:135-151`).
func TestFetchPointsParsesBothPools(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if got := request.Headers["token"]; len(got) != 1 || got[0] == "" {
			t.Errorf("token header = %#v, want the session under the lowercase name", got)
		}
		if got := request.Headers.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want no bearer header on /points/records", got)
		}
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	}
	fake.install(t)

	snapshot, errPoints := fetchPoints(testHost(), sampleCredential(t), DefaultConfig())
	if errPoints != nil {
		t.Fatalf("fetchPoints: %v", errPoints)
	}
	if snapshot.Balance != 15000 || snapshot.DailyBalance != 4992 || snapshot.Available != 19992 {
		t.Fatalf("snapshot = %#v, want the measured 15000/4992/19992 example", snapshot)
	}
	if snapshot.DailyQuota != nil {
		t.Fatal("dailyQuota only exists in the first-login response and must not be hard-coded (trap #13)")
	}
	calls := fake.callsFor(PointsRecordsPath)
	if len(calls) != 1 {
		t.Fatalf("issued %d records calls, want 1", len(calls))
	}
	if !strings.Contains(calls[0].URL, "pageNo=1") || !strings.Contains(calls[0].URL, "pageSize=1") ||
		!strings.Contains(calls[0].URL, "recordType=all") {
		t.Fatalf("url = %q, want the fixed pageNo/pageSize/recordType query", calls[0].URL)
	}
}

// `availableBalance` falls back to balance+daily, and a missing `balance` yields
// NO snapshot — the page must not render 0 (trap #11).
func TestFetchPointsNeverFabricatesZero(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"dailyBalance":10}}`), nil
	}
	fake.install(t)
	snapshot, errPoints := fetchPoints(testHost(), sampleCredential(t), DefaultConfig())
	if errPoints != nil {
		t.Fatalf("fetchPoints: %v", errPoints)
	}
	if snapshot != nil {
		t.Fatalf("snapshot = %#v, want nil when balance is absent", snapshot)
	}

	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":100,"dailyBalance":5}}`), nil
	}
	snapshot, errPoints = fetchPoints(testHost(), sampleCredential(t), DefaultConfig())
	if errPoints != nil || snapshot == nil {
		t.Fatalf("snapshot = %#v / %v, want a parsed snapshot", snapshot, errPoints)
	}
	if snapshot.Available != 105 {
		t.Fatalf("available = %v, want balance+daily = 105", snapshot.Available)
	}
}

// A dead session on the read path is terminal.
func TestFetchPointsClassifiesAuthExpired(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)
	_, errPoints := fetchPoints(testHost(), sampleCredential(t), DefaultConfig())
	if errPoints == nil {
		t.Fatal("100002 must fail the read")
	}
	if status := statusOf(errPoints, 0); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// The daily grant keys off `alreadyProcessed` in the RESPONSE body
// (`loomy-credits.ts:202-243`), never the HTTP status and never `code`.
func TestClaimDailyGrantIdempotencyMapping(t *testing.T) {
	// A repeat: alreadyProcessed=true must be `already-claimed`, never `claimed`.
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":true,"currentBalance":15000,"permanentBalance":15000,"dailyBalance":5000,"dailyQuota":5000,"dailyConsumed":0,"dailyCycleDate":"2026-09-26"}}`), nil
	}
	fake.install(t)
	repeat := claimDailyGrant(testHost(), sampleCredential(t), DefaultConfig())
	if repeat.Status != "already-claimed" {
		t.Fatalf("status = %q, want already-claimed", repeat.Status)
	}
	if !strings.Contains(repeat.Message, "今日额度已初始化") || !strings.Contains(repeat.Message, "5000/5000") {
		t.Fatalf("message = %q, want the documented wording with the pool numbers", repeat.Message)
	}
	if repeat.Credit != 0 {
		t.Fatalf("credit = %v, want 0 for a repeat", repeat.Credit)
	}

	// A first claim: credit = dailyQuota - dailyConsumed.
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":false,"permanentBalance":5000,"dailyBalance":5000,"dailyQuota":5000,"dailyConsumed":8}}`), nil
	}
	claimed := claimDailyGrant(testHost(), sampleCredential(t), DefaultConfig())
	if claimed.Status != "claimed" {
		t.Fatalf("status = %q, want claimed", claimed.Status)
	}
	if claimed.Credit != 4992 {
		t.Fatalf("credit = %v, want dailyQuota-dailyConsumed = 4992", claimed.Credit)
	}
	if claimed.Snapshot == nil || claimed.Snapshot.Balance != 5000 || claimed.Snapshot.DailyBalance != 5000 {
		t.Fatalf("snapshot = %#v, want the returned pools", claimed.Snapshot)
	}
	if claimed.Snapshot.DailyCycleDate != "" {
		t.Fatalf("cycle date = %q, want empty when the server omits it", claimed.Snapshot.DailyCycleDate)
	}
}

// Without both quota fields the credit is the quota (or 0), never a negative
// number.
func TestClaimDailyGrantCreditFallbacks(t *testing.T) {
	cases := map[string]struct {
		body   string
		credit float64
	}{
		"quota only":     {`{"code":"000000","data":{"alreadyProcessed":false,"dailyQuota":5000}}`, 5000},
		"no numbers":     {`{"code":"000000","data":{"alreadyProcessed":false}}`, 0},
		"consumed>quota": {`{"code":"000000","data":{"alreadyProcessed":false,"dailyQuota":10,"dailyConsumed":50}}`, 0},
	}
	for label, testCase := range cases {
		fake := newFakeHost()
		fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(200, testCase.body), nil
		}
		fake.install(t)
		outcome := claimDailyGrant(testHost(), sampleCredential(t), DefaultConfig())
		if outcome.Status != "claimed" || outcome.Credit != testCase.credit {
			t.Errorf("%s: status=%q credit=%v, want claimed/%v", label, outcome.Status, outcome.Credit, testCase.credit)
		}
	}
}

// Every failure mode maps to `failed` and never panics or returns an error, so a
// batch run is not interrupted by one bad account
// (`loomy-credits.ts:206,217-221`).
func TestClaimDailyGrantNeverThrows(t *testing.T) {
	cases := map[string]func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error){
		"transport": func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) { return nil, errFakeTransport },
		"http 500":  func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) { return httpResponse(500, "boom"), nil },
		"expired": func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(200, `{"code":"100002"}`), nil
		},
		"garbage": func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
			return httpResponse(200, "not json"), nil
		},
	}
	for label, route := range cases {
		fake := newFakeHost()
		fake.do = route
		fake.install(t)
		outcome := claimDailyGrant(testHost(), sampleCredential(t), DefaultConfig())
		if outcome.Status != "failed" {
			t.Errorf("%s: status = %q, want failed", label, outcome.Status)
		}
		if outcome.Message == "" {
			t.Errorf("%s: a failure must carry a message", label)
		}
	}
}

// The write endpoint's body is `{}` and the call carries a Content-Type.
func TestClaimDailyGrantBodyAndHeaders(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"alreadyProcessed":false}}`), nil
	}
	fake.install(t)
	if outcome := claimDailyGrant(testHost(), sampleCredential(t), DefaultConfig()); outcome.Status != "claimed" {
		t.Fatalf("status = %q, want claimed", outcome.Status)
	}
	calls := fake.callsFor(PointsFirstLoginPath)
	if len(calls) != 1 {
		t.Fatalf("issued %d first-login calls, want 1", len(calls))
	}
	if strings.TrimSpace(string(calls[0].Body)) != "{}" {
		t.Fatalf("body = %q, want {}", calls[0].Body)
	}
	if got := calls[0].Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json for a body-carrying call", got)
	}
	if got := calls[0].Headers["token"]; len(got) != 1 || got[0] == "" {
		t.Fatalf("token header = %#v, want the lowercase session header", got)
	}
}

// quota.* declares the provider, reports both pools and refuses to reset.
func TestQuotaHandlers(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992,"dailyQuota":5000,"dailyConsumed":8,"dailyCycleDate":"2026-09-26"}}`), nil
	}
	fake.install(t)

	describeValue, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("quota.describe: %v", errDescribe)
	}
	described := decodeResult[pluginapi.QuotaDescribeResponse](t, describeValue)
	if len(described.SupportedProviders) != 1 || described.SupportedProviders[0] != ProviderKey {
		t.Fatalf("describe = %#v, want the loomy provider", described)
	}
	if described.SupportsReset {
		t.Fatal("there is no reset endpoint, so SupportsReset must be false")
	}

	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	fetchValue, errFetch := handleQuotaFetch(testHost(), mustJSON(t, pluginapi.QuotaFetchRequest{
		AuthIndex: "loomy-1", Provider: ProviderKey, StorageJSON: storage,
	}))
	if errFetch != nil {
		t.Fatalf("quota.fetch: %v", errFetch)
	}
	fetched := decodeResult[pluginapi.QuotaFetchResponse](t, fetchValue)
	if len(fetched.Summary) != 4 {
		t.Fatalf("summary = %#v, want the two pools, the total and the quota", fetched.Summary)
	}
	byKey := map[string]pluginapi.QuotaMetric{}
	for _, metric := range fetched.Summary {
		byKey[metric.Key] = metric
	}
	if byKey["permanent_points"].Value != 15000 || byKey["daily_points"].Value != 4992 || byKey["available_points"].Value != 19992 {
		t.Fatalf("metrics = %#v, want the measured pools", byKey)
	}
	if byKey["daily_quota"].Label != dailyQuotaDescription {
		t.Fatalf("quota label = %q, want the mandated 每日赠送额度 wording", byKey["daily_quota"].Label)
	}

	resetValue, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("quota.reset: %v", errReset)
	}
	if decodeResult[pluginapi.QuotaResetResponse](t, resetValue).Success {
		t.Fatal("reset must report failure: the server owns the daily grant")
	}

	identifierValue, errIdentifier := handleQuotaIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("quota.identifier: %v", errIdentifier)
	}
	identifier := decodeResult[map[string]string](t, identifierValue)
	if identifier["identifier"] != ProviderKey {
		t.Fatalf("identifier = %q, want %q", identifier["identifier"], ProviderKey)
	}
}

// A quota read that fails is an error, never a zero balance.
func TestQuotaFetchFailsInsteadOfZeroing(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"000000","data":{"dailyBalance":10}}`), nil
	}
	fake.install(t)
	storage, errEncode := sampleCredential(t).Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	_, errFetch := handleQuotaFetch(testHost(), mustJSON(t, pluginapi.QuotaFetchRequest{StorageJSON: storage}))
	if errFetch == nil {
		t.Fatal("a response without `balance` must not be rendered as numbers")
	}
	if !strings.Contains(errFetch.Error(), "balance") {
		t.Fatalf("error = %v, want it to name the missing field", errFetch)
	}
}

// ensureDailyGrant logs a warning on failure instead of failing the login
// (`loomy-auth.ts:241-249`).
func TestEnsureDailyGrantIsWarnOnly(t *testing.T) {
	fake := newFakeHost()
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"code":"100002","desc":"缺少 token"}`), nil
	}
	fake.install(t)
	ensureDailyGrant(testHost(), sampleCredential(t), DefaultConfig())
	if len(fake.logs) != 1 {
		t.Fatalf("logs = %d, want one warning", len(fake.logs))
	}
	if !strings.Contains(fake.logs[0], "每日额度") {
		t.Fatalf("log = %q, want the daily-grant warning", fake.logs[0])
	}
}
