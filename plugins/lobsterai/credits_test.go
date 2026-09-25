package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSlotQuery(t *testing.T) {
	query := slotQuery("2026.9.4")
	parsed, errParse := url.ParseQuery(query)
	if errParse != nil {
		t.Fatalf("slotQuery is not a valid query: %v", errParse)
	}
	want := map[string]string{
		"placement":           SlotPlacement,
		"clientVersion":       "2026.9.4",
		"containerApiVersion": SlotContainerAPIVersion,
		"platform":            SlotPlatform,
	}
	for key, value := range want {
		if parsed.Get(key) != value {
			t.Fatalf("slot query %s = %q, want %q", key, parsed.Get(key), value)
		}
	}
	if len(parsed) != len(want) {
		t.Fatalf("slot query carries unexpected keys: %v", parsed)
	}
	// The platform is a fixed impersonation value, not the real host OS.
	if parsed.Get("platform") != "win32" {
		t.Fatalf("platform = %q, want win32", parsed.Get("platform"))
	}
}

func TestRandomUUID(t *testing.T) {
	first, errUUID := randomUUID()
	if errUUID != nil {
		t.Fatalf("randomUUID: %v", errUUID)
	}
	second, _ := randomUUID()
	if first == second {
		t.Fatal("randomUUID returned a duplicate")
	}
	if len(first) != 36 {
		t.Fatalf("randomUUID length = %d (%q), want 36", len(first), first)
	}
	if first[14] != '4' {
		t.Fatalf("randomUUID version nibble = %c (%q), want 4", first[14], first)
	}
	if !strings.Contains("89ab", string(first[19])) {
		t.Fatalf("randomUUID variant nibble = %c (%q)", first[19], first)
	}
}

func TestFetchActivitySlot(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodGet,
		Match:  ActivitySlotPath,
		Body:   `{"code":0,"msg":"OK","data":{"slotState":"available","activity":{"activityCode":"daily-1","configRevision":7}}}`,
	})
	host := installFakeHost(t, fake)
	slot, errSlot := fetchActivitySlot(host, &Credential{AccessToken: "a"}, "2026.9.4")
	if errSlot != nil {
		t.Fatalf("fetchActivitySlot: %v", errSlot)
	}
	if slot.SlotState != "available" || slot.ActivityCode != "daily-1" || slot.ConfigRevision != 7 {
		t.Fatalf("slot = %+v", slot)
	}
	request := fake.requestsFor(ActivitySlotPath)[0]
	if got := request.Headers.Get("Authorization"); got != "Bearer a" {
		t.Fatalf("Authorization = %q", got)
	}
	assertNoTencentHeaders(t, request.Headers)
}

func TestFetchActivityContext(t *testing.T) {
	fake := newFakeHost().on(httpRoute{
		Method: http.MethodGet,
		Match:  ActivityContextPath,
		Body:   `{"code":0,"msg":"OK","data":{"state":{"claimedToday":true},"actions":["check_in","other"]}}`,
	})
	host := installFakeHost(t, fake)
	context, errContext := fetchActivityContext(host, &Credential{AccessToken: "a"}, &activitySlot{
		SlotState: "available", ActivityCode: "daily/1", ConfigRevision: 7,
	})
	if errContext != nil {
		t.Fatalf("fetchActivityContext: %v", errContext)
	}
	if !context.ClaimedToday || len(context.Actions) != 2 {
		t.Fatalf("context = %+v", context)
	}
	// The activity code is path-escaped.
	request := fake.requestsFor(ActivityContextPath)[0]
	if !strings.Contains(request.URL, "daily%2F1") {
		t.Fatalf("activity code was not escaped: %s", request.URL)
	}
	if !strings.Contains(request.URL, "configRevision=7") {
		t.Fatalf("configRevision missing: %s", request.URL)
	}
}

func TestClaimDailyCheckinOutcomes(t *testing.T) {
	slotBody := `{"code":0,"msg":"OK","data":{"slotState":"available","activity":{"activityCode":"daily-1","configRevision":7}}}`

	tests := []struct {
		name         string
		slot         string
		context      string
		claimStatus  int
		claim        string
		wantKind     string
		wantCredit   float64
		wantGranted  bool
		wantClaimReq bool
	}{
		{
			name:         "granted via creditsGranted",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`,
			claim:        `{"code":0,"msg":"OK","data":{"result":{"creditsGranted":12.5,"message":"明天再来"}}}`,
			wantKind:     "claimed",
			wantCredit:   12.5,
			wantGranted:  true,
			wantClaimReq: true,
		},
		{
			name:         "granted via rewardCredits",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`,
			claim:        `{"code":0,"data":{"result":{"rewardCredits":3}}}`,
			wantKind:     "claimed",
			wantCredit:   3,
			wantGranted:  true,
			wantClaimReq: true,
		},
		{
			name:         "granted via credits",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`,
			claim:        `{"code":0,"data":{"result":{"credits":"2.5"}}}`,
			wantKind:     "claimed",
			wantCredit:   2.5,
			wantGranted:  true,
			wantClaimReq: true,
		},
		{
			name:         "success without a credit field is still honest",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`,
			claim:        `{"code":0,"data":{"result":{}}}`,
			wantKind:     "claimed",
			wantGranted:  false,
			wantClaimReq: true,
		},
		{
			name:         "already claimed short-circuits",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":true},"actions":["check_in"]}}`,
			wantKind:     "already-claimed",
			wantClaimReq: false,
		},
		{
			name:         "slot not available",
			slot:         `{"code":0,"data":{"slotState":"closed","activity":{"activityCode":"daily-1"}}}`,
			wantKind:     "inactive",
			wantClaimReq: false,
		},
		{
			name:         "slot without an activity code",
			slot:         `{"code":0,"data":{"slotState":"available","activity":{}}}`,
			wantKind:     "inactive",
			wantClaimReq: false,
		},
		{
			name:         "action not offered",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["other"]}}`,
			wantKind:     "inactive",
			wantClaimReq: false,
		},
		{
			name:         "claim rejected by the envelope",
			slot:         slotBody,
			context:      `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`,
			claim:        `{"code":500,"msg":"boom","data":{}}`,
			wantKind:     "failed",
			wantClaimReq: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: test.slot})
			if test.context != "" {
				fake.on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: test.context})
			}
			if test.claim != "" {
				fake.on(httpRoute{Method: http.MethodPost, Match: "actions/check_in", Status: test.claimStatus, Body: test.claim})
			}
			host := installFakeHost(t, fake)

			outcome := claimDailyCheckin(host, &Credential{AccessToken: "a"}, "2026.9.4", time.Now())
			if outcome.Kind != test.wantKind {
				t.Fatalf("kind = %q (%s), want %q", outcome.Kind, outcome.Message, test.wantKind)
			}
			if outcome.CreditGranted != test.wantGranted {
				t.Fatalf("CreditGranted = %v, want %v", outcome.CreditGranted, test.wantGranted)
			}
			if test.wantGranted && outcome.Credit != test.wantCredit {
				t.Fatalf("credit = %v, want %v", outcome.Credit, test.wantCredit)
			}

			claims := fake.requestsFor("actions/check_in")
			if test.wantClaimReq != (len(claims) > 0) {
				t.Fatalf("claim requests = %d, wantClaim=%v", len(claims), test.wantClaimReq)
			}
			if !test.wantClaimReq {
				return
			}
			// The claim body must carry the client idempotency key, the config
			// revision and an empty payload.
			body := map[string]any{}
			if errUnmarshal := decodeJSON(claims[0].Body, &body); errUnmarshal != nil {
				t.Fatalf("decode claim body: %v", errUnmarshal)
			}
			key, _ := body["idempotencyKey"].(string)
			if len(key) != 36 || !strings.Contains("89ab", string(key[19])) {
				t.Fatalf("idempotencyKey = %q, want a UUID v4", key)
			}
			if value, found := readNumberField(body, "configRevision"); !found || value != 7 {
				t.Fatalf("configRevision = %v", body["configRevision"])
			}
			if payload := asRecord(body["payload"]); payload == nil {
				t.Fatalf("payload = %v, want an object", body["payload"])
			}
			if got := claims[0].Headers.Get("Authorization"); got != "Bearer a" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}
}

func TestClaimDailyCheckinIdempotencyKeysDiffer(t *testing.T) {
	fake := newFakeHost().
		on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d","configRevision":1}}}`}).
		on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`}).
		on(httpRoute{Method: http.MethodPost, Match: "actions/check_in", Body: `{"code":0,"data":{"result":{"credits":1}}}`})
	host := installFakeHost(t, fake)

	for index := 0; index < 2; index++ {
		outcome := claimDailyCheckin(host, &Credential{AccessToken: "a"}, "v", time.Now())
		if outcome.Kind != "claimed" {
			t.Fatalf("outcome = %+v", outcome)
		}
	}
	claims := fake.requestsFor("actions/check_in")
	if len(claims) != 2 {
		t.Fatalf("claims = %d", len(claims))
	}
	first := map[string]any{}
	second := map[string]any{}
	_ = decodeJSON(claims[0].Body, &first)
	_ = decodeJSON(claims[1].Body, &second)
	if first["idempotencyKey"] == second["idempotencyKey"] {
		t.Fatal("each claim must use a fresh idempotency key")
	}
}

func TestClaimDailyCheckinFailurePaths(t *testing.T) {
	t.Run("slot transport failure", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Err: errFakeTransport})
		host := installFakeHost(t, fake)
		outcome := claimDailyCheckin(host, &Credential{AccessToken: "a"}, "v", time.Now())
		if outcome.Kind != "failed" || outcome.Code != -1 {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	t.Run("context envelope failure", func(t *testing.T) {
		fake := newFakeHost().
			on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d"}}}`}).
			on(httpRoute{Method: http.MethodGet, Match: ActivityContextPath + "/", Body: `{"code":9,"msg":"nope"}`})
		host := installFakeHost(t, fake)
		outcome := claimDailyCheckin(host, &Credential{AccessToken: "a"}, "v", time.Now())
		if outcome.Kind != "failed" {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

func TestFetchCreditBalance(t *testing.T) {
	t.Run("packages, clamp and expired total", func(t *testing.T) {
		body := `{"code":0,"msg":"OK","data":{
			"totalCreditsRemaining":55.67000031,
			"creditItems":[
				{"type":"activity","creditsRemaining":50.1,"expiresAt":"2999-01-01 00:00:00"},
				{"type":"free","creditsRemaining":5.57000031,"expiresAt":"2000-01-01 00:00:00"},
				{"type":"negative","creditsRemaining":-3}
			]}}`
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: body})
		host := installFakeHost(t, fake)
		balance, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"})
		if errBalance != nil {
			t.Fatalf("fetchCreditBalance: %v", errBalance)
		}
		if balance.Total != 55.67 {
			t.Fatalf("Total = %v, want 55.67 (rounded)", balance.Total)
		}
		if len(balance.Packages) != 3 {
			t.Fatalf("packages = %+v", balance.Packages)
		}
		if !balance.Packages[0].Active {
			t.Fatal("a future expiry must be active")
		}
		if balance.Packages[1].Active {
			t.Fatal("a past expiry must be inactive")
		}
		if !balance.Packages[2].Active {
			t.Fatal("a missing expiry must not be treated as expired")
		}
		// Negative remaining values are clamped, so the expired total cannot go
		// negative either.
		if balance.ExpiredTotal != 5.57 {
			t.Fatalf("ExpiredTotal = %v, want 5.57", balance.ExpiredTotal)
		}
		if balance.Packages[0].Name != "activity" || balance.Packages[0].Total != 0 || balance.Packages[0].Used != 0 {
			t.Fatalf("package = %+v, want remaining-only reporting", balance.Packages[0])
		}
	})

	t.Run("zero total without items means unknown", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{
			Method: http.MethodGet, Match: ProfileSummaryPath,
			Body: `{"code":0,"data":{"totalCreditsRemaining":0,"creditItems":[]}}`,
		})
		host := installFakeHost(t, fake)
		if _, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"}); errBalance == nil {
			t.Fatal("0 with no items must be reported as unknown, not as a zero balance")
		}
	})

	t.Run("a real zero balance is reported", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{
			Method: http.MethodGet, Match: ProfileSummaryPath,
			Body: `{"code":0,"data":{"totalCreditsRemaining":0,"creditItems":[{"type":"t","creditsRemaining":0}]}}`,
		})
		host := installFakeHost(t, fake)
		balance, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"})
		if errBalance != nil {
			t.Fatalf("fetchCreditBalance: %v", errBalance)
		}
		if balance.Total != 0 {
			t.Fatalf("Total = %v", balance.Total)
		}
	})

	t.Run("negative total is clamped to zero", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{
			Method: http.MethodGet, Match: ProfileSummaryPath,
			Body: `{"code":0,"data":{"totalCreditsRemaining":-12.5,"creditItems":[{"type":"t","creditsRemaining":-12.5}]}}`,
		})
		host := installFakeHost(t, fake)
		balance, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"})
		if errBalance != nil {
			t.Fatalf("fetchCreditBalance: %v", errBalance)
		}
		if balance.Total != 0 {
			t.Fatalf("Total = %v, want 0", balance.Total)
		}
	})

	t.Run("envelope failure", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: `{"code":40100,"msg":"token rejected"}`})
		host := installFakeHost(t, fake)
		if _, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"}); errBalance == nil {
			t.Fatal("expected a failure")
		}
	})

	t.Run("non json response names the status", func(t *testing.T) {
		fake := newFakeHost().on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Status: 401, Body: "<html>login</html>"})
		host := installFakeHost(t, fake)
		_, errBalance := fetchCreditBalance(host, &Credential{AccessToken: "a"})
		if errBalance == nil {
			t.Fatal("expected a failure")
		}
		if !strings.Contains(errBalance.Error(), "凭据已失效") {
			t.Fatalf("error = %q, want a readable credential-expiry reason", errBalance.Error())
		}
	})
}

func TestRoundCreditsAndHelpers(t *testing.T) {
	if got := roundCredits(55.67000031); got != 55.67 {
		t.Fatalf("roundCredits = %v", got)
	}
	if got := formatNumber(7); got != "7" {
		t.Fatalf("formatNumber(7) = %q", got)
	}
	if got := formatNumber(7.5); got != "7.5" {
		t.Fatalf("formatNumber(7.5) = %q", got)
	}
	if !containsString([]string{"check_in", "other"}, "check_in") {
		t.Fatal("containsString missed a value")
	}
	if containsString([]string{"other"}, "check_in") {
		t.Fatal("containsString invented a value")
	}
}

func TestIsExpiredTimestamp(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty", raw: "", want: false},
		{name: "garbage", raw: "not-a-date", want: false},
		{name: "past rfc3339", raw: "2029-01-01T00:00:00Z", want: true},
		{name: "future rfc3339", raw: "2031-01-01T00:00:00Z", want: false},
		{name: "space separated", raw: "2029-01-01 00:00:00", want: true},
		{name: "date only", raw: "2029-01-01", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isExpiredTimestamp(test.raw, now); got != test.want {
				t.Fatalf("isExpiredTimestamp(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestDescribeNonJSONResponse(t *testing.T) {
	if got := describeNonJSONResponse(401, "login page"); !strings.Contains(got, "凭据已失效") {
		t.Fatalf("describeNonJSONResponse(401) = %q", got)
	}
	long := strings.Repeat("x", 200)
	got := describeNonJSONResponse(500, long)
	if len(got) > 140 {
		t.Fatalf("describeNonJSONResponse did not shorten the body: %d chars", len(got))
	}
}

func TestQuotaHandlers(t *testing.T) {
	fake := newFakeHost().
		on(httpRoute{Method: http.MethodGet, Match: ProfileSummaryPath, Body: `{"code":0,"data":{"totalCreditsRemaining":10,"creditItems":[{"creditsRemaining":10}]}}`}).
		on(httpRoute{Method: http.MethodGet, Match: ActivitySlotPath, Body: `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"d"}}}`})
	host := installFakeHost(t, fake)

	value, errIdentifier := handleQuotaIdentifier(nil, nil)
	if errIdentifier != nil {
		t.Fatalf("handleQuotaIdentifier: %v", errIdentifier)
	}
	if encoded := mustJSON(t, value); string(encoded) != `{"identifier":"lobsterai"}` {
		t.Fatalf("quota.identifier = %s", encoded)
	}

	value, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("handleQuotaDescribe: %v", errDescribe)
	}
	describe := value.(pluginapi.QuotaDescribeResponse)
	if len(describe.SupportedProviders) != 1 || describe.SupportedProviders[0] != ProviderKey || describe.SupportsReset {
		t.Fatalf("describe = %+v", describe)
	}

	value, errFetch := handleQuotaFetch(host, mustJSON(t, pluginapi.QuotaFetchRequest{
		StorageJSON: mustJSON(t, &Credential{AccessToken: "a"}),
	}))
	if errFetch != nil {
		t.Fatalf("handleQuotaFetch: %v", errFetch)
	}
	metrics := value.(pluginapi.QuotaFetchResponse).Summary
	keys := map[string]float64{}
	for _, metric := range metrics {
		keys[metric.Key] = metric.Value
	}
	if keys["credit_remaining"] != 10 || keys["daily_checkin"] != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}

	value, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("handleQuotaReset: %v", errReset)
	}
	if response := value.(pluginapi.QuotaResetResponse); response.Success {
		t.Fatal("LobsterAI must not advertise a quota reset")
	}
}

func TestQuotaFetchWithoutNetwork(t *testing.T) {
	fake := newFakeHost()
	host := installFakeHost(t, fake)
	value, errFetch := handleQuotaFetch(host, mustJSON(t, pluginapi.QuotaFetchRequest{
		StorageJSON: mustJSON(t, &Credential{AccessToken: "a"}),
	}))
	if errFetch != nil {
		t.Fatalf("handleQuotaFetch: %v", errFetch)
	}
	// Unreachable upstream sources must degrade to an empty summary rather than
	// failing the quota route.
	if metrics := value.(pluginapi.QuotaFetchResponse).Summary; len(metrics) != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}
