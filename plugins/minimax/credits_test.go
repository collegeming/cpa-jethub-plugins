package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credits"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Credits tests: timezone_id in the QUERY (never a header), business status in
// base_resp, claim_result idempotency, points vs bonus_points, flat balance
// response, and the sum of string remaining_amount values.

func TestTimezoneQueryUsesQueryParameter(t *testing.T) {
	tests := []struct {
		name string
		zone string
	}{
		{"UTC", "UTC"},
		{"China", "Asia/Shanghai"},
		{"spaces are encoded", "America/Argentina/Buenos_Aires"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw, errURL := timezoneQuery(APIHost, SigninStatusPath, testCase.zone)
			if errURL != nil {
				t.Fatalf("timezoneQuery: %v", errURL)
			}
			parsed, errParse := url.Parse(raw)
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := parsed.Query().Get("timezone_id"); got != testCase.zone {
				t.Errorf("timezone_id = %q, want %q (url=%s)", got, testCase.zone, raw)
			}
			if parsed.Path != SigninStatusPath {
				t.Errorf("path = %q, want %q", parsed.Path, SigninStatusPath)
			}
		})
	}
}

// TestSigninRequestsPutTimezoneInQueryNotHeader locks the measured trap:
// putting timezone_id in a header returns business code 1406010011 WITH HTTP
// 200. A status-code-only test would still pass, so the request itself is
// inspected here.
func TestSigninRequestsPutTimezoneInQueryNotHeader(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, signinStatusFixture(false, 800, 400)), nil
	}
	host.install(t)
	cfg := DefaultConfig()
	cfg.TimezoneID = "Asia/Shanghai"
	if _, errStatus := fetchSigninStatus(testHost(), &Credential{AccessToken: "token"}, cfg); errStatus != nil {
		t.Fatalf("fetchSigninStatus: %v", errStatus)
	}
	request := host.requests[0]
	parsed, errParse := url.Parse(request.URL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if got := parsed.Query().Get("timezone_id"); got != "Asia/Shanghai" {
		t.Errorf("timezone_id query = %q, want Asia/Shanghai", got)
	}
	if got := request.Headers.Get("timezone_id"); got != "" {
		t.Errorf("timezone_id header = %q, want ABSENT", got)
	}
	if got := request.Headers.Get("Timezone-Id"); got != "" {
		t.Errorf("Timezone-Id header = %q, want ABSENT", got)
	}
}

func TestRequestEnvelopeReadsBusinessCodeFromBaseResp(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantOK      bool
		wantCode    int
		wantMessage string
	}{
		{
			name:   "base_resp zero is success even with a top-level code",
			body:   `{"code":999,"data":{"x":1},"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantOK: true,
		},
		{
			name:   "base_resp non-zero is failure despite HTTP 200",
			body:   `{"code":0,"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`,
			wantOK: false, wantCode: 1406010011, wantMessage: "invalid timezone_id",
		},
		{
			name:   "a top-level code is not the business code",
			body:   `{"code":1406010011,"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantOK: true,
		},
		{
			name:   "missing base_resp is tolerated by the flat response reader",
			body:   `{"total_count":0}`,
			wantOK: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakeHost()
			host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(http.StatusOK, testCase.body), nil
			}
			host.install(t)
			result, errRequest := requestEnvelope(testHost(), http.MethodGet, APIHost+CreditDetailsPath,
				&Credential{AccessToken: "token"}, nil, DefaultConfig())
			if errRequest != nil {
				t.Fatalf("requestEnvelope: %v", errRequest)
			}
			if result.OK != testCase.wantOK || result.Code != testCase.wantCode || result.Message != testCase.wantMessage {
				t.Errorf("result = %+v, want OK=%v Code=%d Message=%q",
					result, testCase.wantOK, testCase.wantCode, testCase.wantMessage)
			}
		})
	}
}

func TestInvalidTimezoneBusinessCodeIsClassified(t *testing.T) {
	err := businessError("读取签到", envelopeResult{
		OK: false, Code: timezoneRejectedCode, Message: "invalid timezone_id",
	})
	var envelope *abiboot.EnvelopeError
	if !errors.As(err, &envelope) {
		t.Fatalf("businessError returned %T, want EnvelopeError", err)
	}
	if envelope.Code != "invalid_timezone" || envelope.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("error = %#v, want invalid_timezone / HTTP 500", envelope)
	}
	if !strings.Contains(envelope.Message, "query 参数") {
		t.Errorf("error does not explain the measured query requirement: %q", envelope.Message)
	}
}

func TestParseSigninPanelStrictValidation(t *testing.T) {
	valid := signinPanelFixtureMap(t, false, 800, 400)
	tests := []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
	}{
		{name: "valid", mutate: func(map[string]any) {}, wantOK: true},
		{name: "must have exactly seven days", mutate: func(panel map[string]any) {
			panel["days"] = panel["days"].([]any)[:6]
		}},
		{name: "day numbers are unique", mutate: func(panel map[string]any) {
			days := panel["days"].([]any)
			days[1].(map[string]any)["day_no"] = float64(1)
		}},
		{name: "only one day may be today", mutate: func(panel map[string]any) {
			days := panel["days"].([]any)
			days[1].(map[string]any)["is_today"] = true
		}},
		{name: "only one day may be claimable", mutate: func(panel map[string]any) {
			days := panel["days"].([]any)
			days[1].(map[string]any)["status"] = float64(signinStatusClaimable)
		}},
		{name: "negative points rejected", mutate: func(panel map[string]any) {
			panel["days"].([]any)[0].(map[string]any)["points"] = float64(-1)
		}},
		{name: "negative bonus rejected", mutate: func(panel map[string]any) {
			panel["days"].([]any)[0].(map[string]any)["bonus_points"] = float64(-1)
		}},
		{name: "unknown status rejected", mutate: func(panel map[string]any) {
			panel["days"].([]any)[0].(map[string]any)["status"] = float64(9)
		}},
		{name: "is_today must be boolean", mutate: func(panel map[string]any) {
			panel["days"].([]any)[0].(map[string]any)["is_today"] = "yes"
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			panel := deepCopyMap(t, valid)
			testCase.mutate(panel)
			got := parseSigninPanel(panel)
			if (got != nil) != testCase.wantOK {
				t.Errorf("parseSigninPanel() = %#v, wantOK=%v", got, testCase.wantOK)
			}
		})
	}
}

func TestPanelToSigninStatusUsesPointsNotBonus(t *testing.T) {
	panel := parseSigninPanel(signinPanelFixtureMap(t, false, 800, 400))
	if panel == nil {
		t.Fatal("fixture panel is invalid")
	}
	status := panelToSigninStatus(panel)
	if !status.Active {
		t.Error("active = false, want true once a panel was read")
	}
	// ⚠️ points=800, bonus_points=400. The real daily amount is 800, NOT 1200.
	if status.DailyCredit != 800 {
		t.Errorf("dailyCredit = %v, want points=800 (bonus_points is already included)", status.DailyCredit)
	}
	if status.TodayCheckedIn {
		t.Error("today_checked_in = true for a claimable day")
	}
	if !status.ClaimableExists {
		t.Error("claimable = false, want true")
	}
	// We deliberately do NOT infer isStreakDay from bonus_points; measured
	// panels have bonus_points on all seven days, so `bonus>0` is meaningless.
	if status.IsStreakDay {
		t.Error("is_streak_day = true, but bonus_points does not carry that meaning")
	}
}

func TestClaimDailyCheckinUsesClaimResultAndPoints(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
		wantAmount float64
	}{
		{
			name:       "claim_result 1 means granted",
			body:       `{"data":{"claim_result":1,"points":800,"bonus_points":400},"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantStatus: credits.StatusClaimed, wantAmount: 800,
		},
		{
			name:       "claim_result 2 is idempotent already-claimed",
			body:       `{"data":{"claim_result":2,"points":800,"bonus_points":400},"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantStatus: credits.StatusAlreadyClaimed, wantAmount: 0,
		},
		{
			name:       "missing claim_result is a failed outcome despite HTTP 200",
			body:       `{"data":{"points":800},"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantStatus: credits.StatusFailed,
		},
		{
			name:       "unknown claim_result is failed",
			body:       `{"data":{"claim_result":3,"points":800},"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantStatus: credits.StatusFailed,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakeHost()
			host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(http.StatusOK, testCase.body), nil
			}
			host.install(t)
			cfg := DefaultConfig()
			cfg.TimezoneID = "Asia/Shanghai"
			outcome, errClaim := claimDailyCheckin(testHost(), &Credential{AccessToken: "token"}, cfg)
			if errClaim != nil {
				t.Fatalf("claimDailyCheckin: %v", errClaim)
			}
			if outcome.Status != testCase.wantStatus || outcome.Amount != testCase.wantAmount {
				t.Errorf("outcome = %+v, want status=%q amount=%v", outcome, testCase.wantStatus, testCase.wantAmount)
			}
			request := host.requests[0]
			if request.Method != http.MethodPost || string(request.Body) != "{}" {
				t.Errorf("request = %s body=%q, want POST {}", request.Method, request.Body)
			}
			parsed, _ := url.Parse(request.URL)
			if parsed.Query().Get("timezone_id") != "Asia/Shanghai" {
				t.Errorf("claim URL lacks timezone_id query: %s", request.URL)
			}
			if request.Headers.Get("timezone_id") != "" {
				t.Errorf("claim request put timezone_id in a header: %#v", request.Headers)
			}
		})
	}
}

func TestClaimDailyCheckinBusinessFailureIsAnOutcome(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`), nil
	}
	host.install(t)
	outcome, errClaim := claimDailyCheckin(testHost(), &Credential{AccessToken: "token"}, DefaultConfig())
	if errClaim != nil {
		t.Fatalf("claimDailyCheckin: %v", errClaim)
	}
	if outcome.Status != credits.StatusFailed || !strings.Contains(outcome.Message, "1406010011") {
		t.Errorf("outcome = %+v, want failed with business code", outcome)
	}
}

func TestFetchCreditBalanceSumsStringRemainingAmounts(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantTotal float64
		wantRows  int
		wantErr   bool
	}{
		{
			name: "flat details response sums string amounts",
			body: `{"details":[
				{"remaining_amount":"800.00","consumed_amount":"0.00"},
				{"remaining_amount":"25.50"},
				{"remaining_amount":4.5}
			],"total_count":3,"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantTotal: 830, wantRows: 3,
		},
		{
			// ⚠️ This is the fixture that defeats the shipped bug: the record
			// count is 1, the real balance is 800.
			name: "total_count is record count, never the balance",
			body: `{"details":[{"remaining_amount":"800.00"}],"total_count":1,
				"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantTotal: 800, wantRows: 1,
		},
		{
			name:      "missing details with total_count zero is a valid zero balance",
			body:      `{"total_count":0,"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantTotal: 0, wantRows: 0,
		},
		{
			name: "invalid amounts are skipped, not treated as the whole query failing",
			body: `{"details":[{"remaining_amount":""},{"remaining_amount":"oops"},{"remaining_amount":"2.5"}],
				"total_count":3,"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantTotal: 2.5, wantRows: 3,
		},
		{
			name: "a nested data envelope is accepted defensively",
			body: `{"data":{"details":[{"remaining_amount":"7"}],"total_count":1},
				"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantTotal: 7, wantRows: 1,
		},
		{
			name:    "missing both details and total_count is not invented as zero",
			body:    `{"base_resp":{"status_code":0,"status_msg":"ok"}}`,
			wantErr: true,
		},
		{
			name:    "non-zero business code is a real failure",
			body:    `{"details":[],"total_count":0,"base_resp":{"status_code":123,"status_msg":"bad"}}`,
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakeHost()
			host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(http.StatusOK, testCase.body), nil
			}
			host.install(t)
			balance, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "token"}, DefaultConfig())
			if testCase.wantErr {
				if errBalance == nil {
					t.Fatalf("fetchCreditBalance accepted %s: %+v", testCase.body, balance)
				}
				return
			}
			if errBalance != nil {
				t.Fatalf("fetchCreditBalance: %v", errBalance)
			}
			if balance.Total != testCase.wantTotal || balance.Rows != testCase.wantRows {
				t.Errorf("balance = %+v, want total=%v rows=%d", balance, testCase.wantTotal, testCase.wantRows)
			}
			if host.requests[0].URL != APIHost+CreditDetailsPath {
				t.Errorf("credit URL = %q, want flat endpoint %q", host.requests[0].URL, APIHost+CreditDetailsPath)
			}
		})
	}
}

func TestLooseAmountAcceptsNumbersAndStrings(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  float64
		ok    bool
	}{
		{"string", "800.00", 800, true},
		{"decimal string", "25.50", 25.5, true},
		{"trimmed string", " 7 ", 7, true},
		{"number", float64(4.5), 4.5, true},
		{"empty string rejected", "", 0, false},
		{"spaces rejected", "  ", 0, false},
		{"junk rejected", "800 points", 0, false},
		{"nil rejected", nil, 0, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := looseAmount(testCase.value)
			if ok != testCase.ok || got != testCase.want {
				t.Errorf("looseAmount(%#v) = %v, %v; want %v, %v", testCase.value, got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

func TestSharedCreditsOutcomeIsUsed(t *testing.T) {
	// The plugin intentionally reuses internal/jethub/credits. This test pins
	// the actual type returned by the signer path, so a future local copy of the
	// Outcome union cannot silently drift from the shared scheduler.
	var outcome credits.Outcome
	outcome.Status = credits.StatusClaimed
	outcome.Amount = 800
	if outcome.Status != "claimed" || outcome.Amount != 800 {
		t.Fatalf("shared credits outcome = %+v", outcome)
	}
	var signer credits.Signer = credits.SignerFunc(func(_ context.Context) (credits.Outcome, error) {
		return outcome, nil
	})
	got, errSign := signer.Sign(context.Background())
	if errSign != nil || !reflect.DeepEqual(got, outcome) {
		t.Fatalf("shared Signer = %+v, %v; want %+v", got, errSign, outcome)
	}
}

// signinStatusFixture builds a complete enveloped 7-day panel.
func signinStatusFixture(claimed bool, points, bonus float64) string {
	status := signinStatusClaimable
	if claimed {
		status = signinStatusClaimed
	}
	days := make([]map[string]any, 0, 7)
	for day := 1; day <= 7; day++ {
		dayStatus := signinStatusUpcoming
		isToday := day == 4
		if day < 4 {
			dayStatus = signinStatusClaimed
		}
		if isToday {
			dayStatus = status
		}
		days = append(days, map[string]any{
			"day_no": day, "points": points, "bonus_points": bonus,
			"status": dayStatus, "is_today": isToday,
		})
	}
	raw, _ := json.Marshal(map[string]any{
		"data":      map[string]any{"scene": 2, "days": days},
		"base_resp": map[string]any{"status_code": 0, "status_msg": "ok"},
	})
	return string(raw)
}

// signinPanelFixtureMap extracts the panel from signinStatusFixture.
func signinPanelFixtureMap(t *testing.T, claimed bool, points, bonus float64) map[string]any {
	t.Helper()
	var envelope map[string]any
	if errUnmarshal := json.Unmarshal([]byte(signinStatusFixture(claimed, points, bonus)), &envelope); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return envelope["data"].(map[string]any)
}

// deepCopyMap clones a generic JSON object through encoding/json.
func deepCopyMap(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	raw, errMarshal := json.Marshal(source)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return out
}
