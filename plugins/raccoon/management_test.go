package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The per-account status document, the quota provider and the ONE-OFF reward
// page. The reward is the item most likely to be mis-sold as a daily check-in, so
// its separation is asserted from the document, the page and the write path.

// accountFixture is one scripted account.
type accountFixture struct {
	name      string
	authIndex string
	token     string
	userID    string
	phone     string
	// balanceBody is what the balance endpoint answers for this account.
	balanceBody string
	// balanceStatus replaces the balance status when non-zero.
	balanceStatus int
}

// raccoonQuotaHost installs a fake host holding the given accounts.
func raccoonQuotaHost(t *testing.T, accounts ...accountFixture) *fakeHost {
	t.Helper()
	fake := newFakeHost()
	for _, account := range accounts {
		credential := &Credential{
			AccessToken:  account.token,
			RefreshToken: "refresh-" + account.name,
			UserID:       account.userID,
			Nickname:     "Raccoon" + account.userID,
			Phone:        account.phone,
			Type:         ProviderKey,
		}
		storedCredential(t, fake, account.authIndex, account.name, credential)
	}
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		token := strings.TrimPrefix(request.Headers.Get("Authorization"), "Bearer ")
		for _, account := range accounts {
			if token != account.token {
				continue
			}
			if account.balanceStatus != 0 {
				return httpResponse(account.balanceStatus, "gateway exploded"), nil
			}
			return httpResponse(http.StatusOK, account.balanceBody), nil
		}
		return httpResponse(http.StatusNotFound, `{"code":100002,"message":"unknown token"}`), nil
	}
	fake.install(t)
	return fake
}

// balanceBody renders a balance answer.
func balanceBody(available, reward, daily, topup, monthly float64) string {
	return jsonBodyRaw(map[string]any{
		"code": 0,
		"data": map[string]any{
			"available_points": available,
			"reward_points":    reward,
			"daily_points":     daily,
			"topup_points":     topup,
			"monthly_points":   monthly,
		},
	})
}

// jsonBodyRaw marshals a value without needing a *testing.T.
func jsonBodyRaw(value any) string {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return "{}"
	}
	return string(encoded)
}

// twoRaccoonAccounts is the canonical fixture: two accounts with distinct pools.
func twoRaccoonAccounts(t *testing.T) *fakeHost {
	t.Helper()
	return raccoonQuotaHost(t,
		accountFixture{
			name: "raccoon-user-alice.json", authIndex: "idx-1", token: "token-alice",
			userID: "user-alice", phone: "13800138000",
			balanceBody: balanceBody(15000, 3000, 300, 0, 0),
		},
		accountFixture{
			name: "raccoon-user-bob.json", authIndex: "idx-2", token: "token-bob",
			userID: "user-bob", phone: "13900139000",
			balanceBody: balanceBody(7, 0, 3, 100, 0),
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnPoints is the core per-account
// contract this repository just moved to.
func TestStatusJSONReportsEveryAccountWithItsOwnPoints(t *testing.T) {
	twoRaccoonAccounts(t)
	withSettings(t, DefaultConfig())

	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status", nil))
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status json: %v", errUnmarshal)
	}
	if document["account_count"] != float64(2) {
		t.Errorf("account_count = %#v, want 2", document["account_count"])
	}
	accounts, okAccounts := document["accounts"].([]any)
	if !okAccounts || len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want an array of two entries", document["accounts"])
	}
	first, _ := accounts[0].(map[string]any)
	second, _ := accounts[1].(map[string]any)
	if first["name"] != "raccoon-user-alice.json" || second["name"] != "raccoon-user-bob.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	firstPoints, _ := first["points"].(map[string]any)
	secondPoints, _ := second["points"].(map[string]any)
	if firstPoints["available"] != float64(15000) || secondPoints["available"] != float64(7) {
		t.Errorf("per-account balances = %#v / %#v, want 15000 / 7", firstPoints["available"], secondPoints["available"])
	}
	if firstPoints["daily"] != float64(300) || secondPoints["topup"] != float64(100) {
		t.Errorf("per-account pools are wrong: %#v / %#v", firstPoints, secondPoints)
	}
	if first["userid"] != "user-alice" || second["userid"] != "user-bob" {
		t.Errorf("per-account identities = %#v / %#v", first["userid"], second["userid"])
	}
	// The flat document keeps describing the selected (first) account.
	if document["account"].(map[string]any)["name"] != "raccoon-user-alice.json" {
		t.Errorf("top-level account = %#v, want the first one", document["account"])
	}
	if document["points"].(map[string]any)["available"] != float64(15000) {
		t.Errorf("top-level points = %#v, want the first account's 15000", document["points"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selected := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status",
		map[string][]string{"auth_index": {"idx-2"}}))
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selected.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status json: %v", errUnmarshal)
	}
	if selectedDocument["account"].(map[string]any)["name"] != "raccoon-user-bob.json" {
		t.Errorf("selected account = %#v, want the second one", selectedDocument["account"])
	}
	if selectedDocument["points"].(map[string]any)["available"] != float64(7) {
		t.Errorf("selected points = %#v, want the second account's 7", selectedDocument["points"])
	}
	if len(selectedDocument["accounts"].([]any)) != 2 {
		t.Errorf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule: a failed read
// is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	raccoonQuotaHost(t,
		accountFixture{
			name: "raccoon-user-alice.json", authIndex: "idx-1", token: "token-alice",
			userID: "user-alice", phone: "13800138000", balanceBody: balanceBody(15000, 3000, 300, 0, 0),
		},
		accountFixture{
			name: "raccoon-user-bob.json", authIndex: "idx-2", token: "token-bob",
			userID: "user-bob", phone: "13900139000", balanceStatus: http.StatusInternalServerError,
		},
	)
	withSettings(t, DefaultConfig())

	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status",
		map[string][]string{"auth_index": {"idx-2"}}))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a failing points read must still serve the document, got HTTP %d", response.StatusCode)
	}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status json: %v", errUnmarshal)
	}
	if _, present := document["points"]; present {
		t.Errorf("a failed read must not publish a number: %#v", document["points"])
	}
	if _, present := document["points_error"]; !present {
		t.Errorf("a failed read must carry its reason: %#v", document)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want two entries", document["accounts"])
	}
	healthy, _ := accounts[0].(map[string]any)
	broken, _ := accounts[1].(map[string]any)
	if healthy["points"].(map[string]any)["available"] != float64(15000) {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["points"]; present {
		t.Errorf("the broken account published a number: %#v", broken["points"])
	}
	if _, present := broken["points_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusDocumentDeclaresNoDailyCheckin pins the capability statement: this
// provider has no check-in endpoint and ships no route that implies one.
func TestStatusDocumentDeclaresNoDailyCheckin(t *testing.T) {
	twoRaccoonAccounts(t)
	withSettings(t, DefaultConfig())

	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status", nil))
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status json: %v", errUnmarshal)
	}
	if document["daily_checkin"] != false {
		t.Errorf("daily_checkin = %#v, want false", document["daily_checkin"])
	}
	if document["one_off_reward"] != LoginPointsGrantPath {
		t.Errorf("one_off_reward = %#v, want %q", document["one_off_reward"], LoginPointsGrantPath)
	}
	if document["sms_login"] != false {
		t.Errorf("sms_login = %#v, want false: the SMS path is not shipped", document["sms_login"])
	}
	if document["refreshable"] != true {
		t.Errorf("refreshable = %#v, want true: this provider has a refresh endpoint", document["refreshable"])
	}
}

// TestStatusPageRendersOneQuotaCardPerAccount is the display half of the
// contract.
func TestStatusPageRendersOneQuotaCardPerAccount(t *testing.T) {
	twoRaccoonAccounts(t)
	withSettings(t, DefaultConfig())

	page := renderPage(t, testHost(), "/status", nil)
	if got := strings.Count(page, "积分 · "); got != 2 {
		t.Fatalf("status page shows %d quota cards, want one per account (2):\n%s", got, truncate(page, 2500))
	}
	for _, want := range []string{
		"raccoon-user-alice.json",
		"raccoon-user-bob.json",
		"15000",
		"300",
		"100",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	if !strings.Contains(page, "<dd>7</dd>") {
		t.Errorf("the second account's own balance is not rendered:\n%s", truncate(page, 2500))
	}
	for _, forbidden := range []string{"<form", "<script"} {
		if strings.Contains(strings.ToLower(page), forbidden) {
			t.Errorf("the status page contains %q; resource routes are GET only", forbidden)
		}
	}
	// The page names the one-off reward and states plainly that there is no
	// daily check-in.
	if !strings.Contains(page, "没有：每日积分由服务端自动发放") {
		t.Error("the status page does not state that there is no daily check-in")
	}
	if !strings.Contains(page, "reward?auth_index=idx-2") {
		t.Error("the second account's reward link does not name the second account")
	}
	if !strings.Contains(page, "login?add=1") {
		t.Error("新建账号 is not reachable from the status page")
	}
	// The catalogue shows the price in the model name, including x1.
	if !strings.Contains(page, "Kimi-K3 · x1") {
		t.Error("the catalogue does not show the x1 multiplier; that is a regression the reference calls out")
	}
}

// TestRewardPageIsExplicitlyOneOffAndNeverADailyAction pins that a bare page
// load performs NO write, and that the wording never calls this a check-in.
func TestRewardPageIsExplicitlyOneOffAndNeverADailyAction(t *testing.T) {
	fake := raccoonQuotaHost(t, accountFixture{
		name: "raccoon-user-alice.json", authIndex: "idx-1", token: "token-alice",
		userID: "user-alice", phone: "13800138000", balanceBody: balanceBody(15000, 3000, 300, 0, 0),
	})
	// The reward status read is a bills call; answer it with a registration gift
	// only, which must NOT look like the login reward.
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, PointsBillsPath) {
			return httpResponse(http.StatusOK, jsonBodyRaw(map[string]any{
				"code": 0,
				"data": map[string]any{"items": []map[string]any{
					{"biz_type": LoginRewardBizType, "event_name": "新用户注册赠送", "points": 3000},
				}},
			})), nil
		}
		return httpResponse(http.StatusOK, balanceBody(15000, 3000, 300, 0, 0)), nil
	}
	withSettings(t, DefaultConfig())

	page := renderPage(t, testHost(), "/reward", nil)
	if calls := fake.callsFor(LoginPointsGrantPath); len(calls) != 0 {
		t.Fatalf("a bare page load performed %d write calls; the write must need ?action=grant", len(calls))
	}
	for _, want := range []string{"一次性", "没有每日签到", "未领取"} {
		if !strings.Contains(page, want) {
			t.Errorf("the reward page is missing %q:\n%s", want, truncate(page, 2000))
		}
	}
	// The page may MENTION that it is not part of a sweep, but it must not offer
	// one: no check-in route, no claim-all link.
	for _, forbidden := range []string{"action=checkin", "action=claimall", "action=claim", "签到成功"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the reward page exposes %q; the reward is one-off and never part of a sweep", forbidden)
		}
	}
	if !strings.Contains(page, "action=grant") {
		t.Error("the page does not offer the explicit grant link")
	}

	// The explicit action performs exactly one POST to the grant endpoint.
	page = renderPage(t, testHost(), "/reward", map[string][]string{"action": {"grant"}, "auth_index": {"idx-1"}})
	calls := fake.callsFor(LoginPointsGrantPath)
	if len(calls) != 1 {
		t.Fatalf("grant action made %d calls, want exactly 1", len(calls))
	}
	if calls[0].Method != http.MethodPost {
		t.Errorf("grant method = %q, want POST", calls[0].Method)
	}
	// The platform header is mandatory for this endpoint.
	if got := calls[0].Headers.Get("X-Client-Platform"); got != ClientPlatform {
		t.Errorf("X-Client-Platform = %q, want %q: the endpoint rejects the call without it", got, ClientPlatform)
	}
	if got := calls[0].Headers.Get("X-Client-Version"); got != ClientVersion {
		t.Errorf("X-Client-Version = %q, want %q", got, ClientVersion)
	}
	if len(calls[0].Body) != 0 {
		t.Errorf("the grant call must carry no body, got %q", calls[0].Body)
	}
	_ = page
}

// TestGrantLoginRewardMapsTheServerGrantedFlag pins trap #26: a repeat answers
// HTTP 200 with `granted:false` and must NOT be reported as a fresh claim.
func TestGrantLoginRewardMapsTheServerGrantedFlag(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantKind  string
		wantMoney int
	}{
		{name: "granted", status: 200, body: `{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`, wantKind: "claimed", wantMoney: 3000},
		{name: "granted with a custom amount", status: 200, body: `{"code":0,"data":{"granted":true,"popup":{"points":1500}}}`, wantKind: "claimed", wantMoney: 1500},
		{name: "granted without a popup", status: 200, body: `{"code":0,"data":{"granted":true}}`, wantKind: "claimed", wantMoney: LoginRewardPoints},
		{name: "already claimed", status: 200, body: `{"code":0,"data":{"granted":false}}`, wantKind: "already-claimed"},
		{name: "missing flag", status: 200, body: `{"code":0,"data":{}}`, wantKind: "already-claimed"},
		{name: "business failure", status: 200, body: `{"code":100002,"message":"params invalid"}`, wantKind: "failed"},
		{name: "gateway failure", status: 502, body: `{"message":"bad gateway"}`, wantKind: "failed"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				return httpResponse(testCase.status, testCase.body), nil
			}
			fake.install(t)
			outcome := grantLoginReward(testHost(), sampleCredential(t), DefaultConfig())
			if outcome.Kind != testCase.wantKind {
				t.Fatalf("kind = %q, want %q (message %q)", outcome.Kind, testCase.wantKind, outcome.Message)
			}
			if testCase.wantMoney != 0 && outcome.Credit != testCase.wantMoney {
				t.Errorf("credit = %d, want %d", outcome.Credit, testCase.wantMoney)
			}
			if testCase.wantKind == "already-claimed" && !strings.Contains(outcome.Message, "已领取") {
				t.Errorf("message = %q, want it to say the reward was already taken", outcome.Message)
			}
		})
	}
	// A transport failure is a failure, never a panic and never a claim.
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) { return nil, errFakeTransport }
	fake.install(t)
	if outcome := grantLoginReward(testHost(), sampleCredential(t), DefaultConfig()); outcome.Kind != "failed" {
		t.Errorf("transport failure kind = %q, want failed", outcome.Kind)
	}
}

// TestRewardStatusDerivesFromBills pins trap #27: the registration gift shares
// the bill type, so the EVENT NAME is what identifies the login reward.
func TestRewardStatusDerivesFromBills(t *testing.T) {
	cases := []struct {
		name        string
		billsStatus int
		items       []map[string]any
		wantClaimed bool
		wantPoints  int
	}{
		{
			name: "login reward present",
			items: []map[string]any{
				{"biz_type": LoginRewardBizType, "event_name": "新用户注册赠送", "points": 3000},
				{"biz_type": LoginRewardBizType, "event_name": LoginRewardEventName, "points": 3000},
			},
			wantClaimed: true, wantPoints: 3000,
		},
		{
			name: "registration gift only",
			items: []map[string]any{
				{"biz_type": LoginRewardBizType, "event_name": "新用户注册赠送", "points": 3000},
			},
			wantClaimed: false, wantPoints: LoginRewardPoints,
		},
		{
			name: "reward with no positive amount",
			items: []map[string]any{
				{"biz_type": LoginRewardBizType, "event_name": LoginRewardEventName, "points": 0},
			},
			wantClaimed: true, wantPoints: LoginRewardPoints,
		},
		{name: "bills failure", billsStatus: http.StatusInternalServerError, wantClaimed: false, wantPoints: LoginRewardPoints},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeHost()
			fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
				if testCase.billsStatus != 0 {
					return httpResponse(testCase.billsStatus, "exploded"), nil
				}
				return httpResponse(http.StatusOK, jsonBodyRaw(map[string]any{
					"code": 0, "data": map[string]any{"items": testCase.items},
				})), nil
			}
			fake.install(t)
			status := fetchRewardStatus(testHost(), sampleCredential(t), DefaultConfig())
			if status.Claimed != testCase.wantClaimed {
				t.Fatalf("claimed = %v, want %v (note %q)", status.Claimed, testCase.wantClaimed, status.Note)
			}
			if status.Points != testCase.wantPoints {
				t.Errorf("points = %d, want %d", status.Points, testCase.wantPoints)
			}
		})
	}
	// The paging query the reference uses is part of the request.
	fake := newFakeHost()
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"items":[]}}`), nil
	}
	fake.install(t)
	fetchRewardStatus(testHost(), sampleCredential(t), DefaultConfig())
	calls := fake.callsFor(PointsBillsPath)
	if len(calls) != 1 || !strings.Contains(calls[0].URL, "paging.limit=50&paging.offset=0") {
		t.Fatalf("bills call = %+v, want the documented paging query", calls)
	}
}

// TestQuotaProviderReportsEveryPoolSeparately pins the quota surface.
func TestQuotaProviderReportsEveryPoolSeparately(t *testing.T) {
	fake := newFakeHost()
	credential := sampleCredential(t)
	fake.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, balanceBody(15000, 3000, 300, 50, 0)), nil
	}
	fake.install(t)

	value, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("describe: %v", errDescribe)
	}
	describe := value.(pluginapi.QuotaDescribeResponse)
	if len(describe.SupportedProviders) != 1 || describe.SupportedProviders[0] != ProviderKey {
		t.Errorf("supported providers = %v", describe.SupportedProviders)
	}
	if describe.SupportsReset {
		t.Error("nothing here can be reset; SupportsReset must be false")
	}

	value, errFetch := handleQuotaFetch(testHost(), mustJSON(t, pluginapi.QuotaFetchRequest{
		AuthIndex: "idx-1", Provider: ProviderKey, StorageJSON: mustStorage(t, credential),
	}))
	if errFetch != nil {
		t.Fatalf("fetch: %v", errFetch)
	}
	fetch := value.(pluginapi.QuotaFetchResponse)
	byKey := map[string]float64{}
	for _, metric := range fetch.Summary {
		byKey[metric.Key] = metric.Value
	}
	for key, want := range map[string]float64{
		"available_points": 15000,
		"reward_points":    3000,
		"daily_points":     300,
		"topup_points":     50,
	} {
		if byKey[key] != want {
			t.Errorf("metric %s = %v, want %v", key, byKey[key], want)
		}
	}
	// A zero monthly pool is filtered out; the other pools are not.
	if _, present := byKey["monthly_points"]; present {
		t.Error("a zero monthly pool must be filtered out (`> 0` is the only positivity filter)")
	}
	if _, present := byKey["reward_points"]; !present {
		t.Error("a zero-valued reward pool must still be reported when the server sends it")
	}

	// A failed read is an error, never a zero.
	failing := newFakeHost()
	failing.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusInternalServerError, "exploded"), nil
	}
	failing.install(t)
	if _, errFetch := handleQuotaFetch(testHost(), mustJSON(t, pluginapi.QuotaFetchRequest{
		StorageJSON: mustStorage(t, credential),
	})); errFetch == nil {
		t.Fatal("a failed balance read must be an error")
	}

	// A body without `available_points` is "unknown", not zero.
	missing := newFakeHost()
	missing.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(http.StatusOK, `{"code":0,"data":{"reward_points":10}}`), nil
	}
	missing.install(t)
	if _, errFetch := handleQuotaFetch(testHost(), mustJSON(t, pluginapi.QuotaFetchRequest{
		StorageJSON: mustStorage(t, credential),
	})); errFetch == nil {
		t.Fatal("a balance without available_points must be reported as unavailable, not as 0")
	}
}

// mustStorage encodes a credential for a request payload.
func mustStorage(t *testing.T, credential *Credential) []byte {
	t.Helper()
	raw, errEncode := credential.Encode()
	if errEncode != nil {
		t.Fatalf("encode: %v", errEncode)
	}
	return raw
}

// TestQuotaResetIsExplicitlyUnsupported pins the honest refusal.
func TestQuotaResetIsExplicitlyUnsupported(t *testing.T) {
	value, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("reset: %v", errReset)
	}
	reset := value.(pluginapi.QuotaResetResponse)
	if reset.Success {
		t.Error("reset must report failure: there is nothing to reset")
	}
	if !strings.Contains(reset.Message, "每日积分") {
		t.Errorf("message = %q, want it to explain the server-granted daily points", reset.Message)
	}
}

// TestUnknownManagementRouteIs404 keeps unrouted paths from looking like a page.
func TestUnknownManagementRouteIs404(t *testing.T) {
	response := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/nope", nil))
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
	// Each declared route answers, and the full incoming path is tolerated.
	for _, path := range []string{"/status", "/login", "/reward"} {
		response := callManagement(t, testHost(),
			managementRequest(http.MethodGet, "/v0/resource/plugins/raccoon"+path, url.Values{}, nil))
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned HTTP %d", path, response.StatusCode)
		}
	}
	_ = time.Now()
}
