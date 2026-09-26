package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of Loomy's status document and status
// page: a channel with several accounts reports EVERY account's own points.
//
// The two fixture accounts carry different session tokens, and the upstream stub
// routes by the `token` header, so an implementation that fetched the points once
// and reused them for both accounts fails these tests instead of rendering one
// account's numbers twice.

// loomyAccountFixture is one scripted account.
type loomyAccountFixture struct {
	name      string
	authIndex string
	session   string
	userID    string
	phone     string
	// pointsBody is what the records endpoint answers for this account.
	pointsBody string
	// pointsStatus replaces the records endpoint status when non-zero, which is
	// how a failing read is exercised.
	pointsStatus int
}

// loomyQuotaHost installs a fake host holding the given accounts.
func loomyQuotaHost(t *testing.T, accounts ...loomyAccountFixture) *fakeHost {
	t.Helper()
	fake := newFakeHost()
	for _, account := range accounts {
		credential := buildCredential(account.session, account.userID, account.phone, "", DefaultConfig(), time.Now())
		storage, errEncode := credential.Encode()
		if errEncode != nil {
			t.Fatalf("encode credential: %v", errEncode)
		}
		fake.files = append(fake.files, pluginapi.HostAuthFileEntry{
			Provider: ProviderKey, AuthIndex: account.authIndex, Name: account.name, Status: "active",
		})
		fake.auths[account.authIndex] = storage
	}
	fake.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		for _, account := range accounts {
			if sessionHeader(request.Headers) != account.session {
				continue
			}
			if account.pointsStatus != 0 {
				return httpResponse(account.pointsStatus, "gateway exploded"), nil
			}
			return httpResponse(http.StatusOK, account.pointsBody), nil
		}
		return httpResponse(http.StatusNotFound, `{"code":"000000","data":{}}`), nil
	}
	fake.install(t)
	return fake
}

// sessionHeader reads the lowercase `token` header, which Header.Get would miss
// because it canonicalises the name to `Token`.
func sessionHeader(headers http.Header) string {
	if values := headers["token"]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// loomyPointsBody renders a records answer with a permanent and a daily pool.
func loomyPointsBody(balance, dailyBalance float64) string {
	body, _ := json.Marshal(map[string]any{
		"code": "000000",
		"data": map[string]any{
			"balance":          balance,
			"dailyBalance":     dailyBalance,
			"availableBalance": balance + dailyBalance,
		},
	})
	return string(body)
}

// twoLoomyAccounts is the canonical fixture: two accounts with distinct figures.
func twoLoomyAccounts(t *testing.T) *fakeHost {
	t.Helper()
	return loomyQuotaHost(t,
		loomyAccountFixture{
			name: "loomy-13800138000.json", authIndex: "idx-1", session: "session-alice",
			userID: "user-alice", phone: "13800138000", pointsBody: loomyPointsBody(15000, 4992),
		},
		loomyAccountFixture{
			name: "loomy-13900139000.json", authIndex: "idx-2", session: "session-bob",
			userID: "user-bob", phone: "13900139000", pointsBody: loomyPointsBody(7, 3),
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnPoints is the core contract.
func TestStatusJSONReportsEveryAccountWithItsOwnPoints(t *testing.T) {
	twoLoomyAccounts(t)
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
	if first["name"] != "loomy-13800138000.json" || second["name"] != "loomy-13900139000.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	firstPoints, _ := first["points"].(map[string]any)
	secondPoints, _ := second["points"].(map[string]any)
	if firstPoints["balance"] != float64(15000) || secondPoints["balance"] != float64(7) {
		t.Errorf("per-account balances = %#v / %#v, want 15000 / 7", firstPoints["balance"], secondPoints["balance"])
	}
	if firstPoints["daily_balance"] != float64(4992) || secondPoints["daily_balance"] != float64(3) {
		t.Errorf("per-account daily pools = %#v / %#v, want 4992 / 3", firstPoints["daily_balance"], secondPoints["daily_balance"])
	}
	if first["userid"] != "user-alice" || second["userid"] != "user-bob" {
		t.Errorf("per-account identities = %#v / %#v", first["userid"], second["userid"])
	}
	// The flat document keeps describing the selected (first) account.
	if document["account"].(map[string]any)["name"] != "loomy-13800138000.json" {
		t.Errorf("top-level account = %#v, want the first one", document["account"])
	}
	if document["points"].(map[string]any)["balance"] != float64(15000) {
		t.Errorf("top-level points = %#v, want the first account's 15000", document["points"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selected := callManagement(t, testHost(), jsonManagementRequest(http.MethodGet, "/status",
		map[string][]string{"auth_index": {"idx-2"}}))
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selected.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status json: %v", errUnmarshal)
	}
	if selectedDocument["account"].(map[string]any)["name"] != "loomy-13900139000.json" {
		t.Errorf("selected account = %#v, want the second one", selectedDocument["account"])
	}
	if selectedDocument["points"].(map[string]any)["balance"] != float64(7) {
		t.Errorf("selected points = %#v, want the second account's 7", selectedDocument["points"])
	}
	if len(selectedDocument["accounts"].([]any)) != 2 {
		t.Errorf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule (trap #11): a
// failed points read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	loomyQuotaHost(t,
		loomyAccountFixture{
			name: "loomy-13800138000.json", authIndex: "idx-1", session: "session-alice",
			userID: "user-alice", phone: "13800138000", pointsBody: loomyPointsBody(15000, 4992),
		},
		loomyAccountFixture{
			name: "loomy-13900139000.json", authIndex: "idx-2", session: "session-bob",
			userID: "user-bob", phone: "13900139000", pointsStatus: http.StatusInternalServerError,
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
	if healthy["points"].(map[string]any)["balance"] != float64(15000) {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["points"]; present {
		t.Errorf("the broken account published a number: %#v", broken["points"])
	}
	if _, present := broken["points_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusPageRendersOneQuotaCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own figures.
func TestStatusPageRendersOneQuotaCardPerAccount(t *testing.T) {
	twoLoomyAccounts(t)
	withSettings(t, DefaultConfig())

	page := renderPage(t, testHost(), "/status", nil)

	if got := strings.Count(page, "积分 · "); got != 2 {
		t.Fatalf("status page shows %d quota cards, want one per account (2):\n%s", got, truncate(page, 2000))
	}
	for _, want := range []string{
		"loomy-13800138000.json",
		"loomy-13900139000.json",
		"15000",
		"4992",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	if !strings.Contains(page, "<dd>7</dd>") || !strings.Contains(page, "<dd>3</dd>") {
		t.Errorf("the second account's own pools are not rendered:\n%s", truncate(page, 2000))
	}
	// The per-account actions name their own account, and adding an account stays
	// reachable without one.
	if !strings.Contains(page, "checkin?auth_index=idx-2") {
		t.Error("the second account's refresh link does not name the second account")
	}
	if !strings.Contains(page, "login?auth_index=idx-2") {
		t.Error("the second account's re-login link does not name the second account")
	}
	if !strings.Contains(page, "login?add=1") {
		t.Error("新建账号 is not reachable from the status page")
	}
	if strings.Contains(strings.ToLower(page), "<form") {
		t.Error("resource routes are dispatched as GET only, so no form may be rendered")
	}
}
