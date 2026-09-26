package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of CodeBuddy's status document and
// status page: a channel with several accounts reports EVERY account's own
// credits and check-in state.
//
// The two fixture accounts carry different access tokens, and the upstream stub
// routes by the Bearer token, so an implementation that fetched the balance once
// and reused it for both accounts fails these tests instead of rendering one
// account's numbers twice. The whole file is product-agnostic: it exercises the
// shared source tree that scripts/build.sh builds into codebuddy,
// codebuddy-intl, workbuddy-cn and workbuddy.

// codebuddyAccountFixture is one scripted account.
type codebuddyAccountFixture struct {
	name        string
	authIndex   string
	accessToken string
	userID      string
	nickname    string
	// credits is the cycle remainder this account's resource endpoint reports.
	credits float64
	// checkedIn/streakDays shape the check-in status answer.
	checkedIn  bool
	streakDays float64
	// balanceStatus replaces the resource endpoint status when non-zero, which
	// is how a failing balance read is exercised.
	balanceStatus int
}

// codebuddyQuotaHost installs a host transport holding the given accounts.
func codebuddyQuotaHost(t *testing.T, cfg Config, accounts ...codebuddyAccountFixture) *abiboot.Host {
	t.Helper()
	previous := settings()
	setSettings(cfg)
	t.Cleanup(func() { setSettings(previous) })

	byToken := map[string]codebuddyAccountFixture{}
	for _, account := range accounts {
		byToken[account.accessToken] = account
	}
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			files := make([]map[string]any, 0, len(accounts))
			for _, account := range accounts {
				files = append(files, map[string]any{
					"name":       account.name,
					"auth_index": account.authIndex,
					"provider":   ProviderKey,
					"type":       ProviderKey,
					"status":     "active",
				})
			}
			return abiboot.OK(map[string]any{"files": files})
		case pluginabi.MethodHostAuthGet:
			var payload struct {
				AuthIndex string `json:"auth_index"`
			}
			_ = json.Unmarshal(request, &payload)
			for _, account := range accounts {
				if account.authIndex != payload.AuthIndex {
					continue
				}
				raw, errMarshal := json.Marshal(&Credential{
					Type:         ProviderKey,
					AccessToken:  account.accessToken,
					UserID:       account.userID,
					Nickname:     account.nickname,
					Product:      string(cfg.Product),
					RefreshToken: "refresh-" + account.accessToken,
				})
				if errMarshal != nil {
					return nil, errMarshal
				}
				return abiboot.OK(map[string]any{
					"auth_index": account.authIndex,
					"name":       account.name,
					"json":       json.RawMessage(raw),
				})
			}
			return nil, fmt.Errorf("unknown auth_index %s", payload.AuthIndex)
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(request, &payload)
			authorization := payload.Headers.Get("Authorization")
			var account codebuddyAccountFixture
			found := false
			for token, candidate := range byToken {
				if strings.Contains(authorization, token) {
					account, found = candidate, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("no account matches the request signature: %s", authorization)
			}
			body, status := codebuddyUpstreamBody(account, payload.URL)
			return abiboot.OK(map[string]any{"StatusCode": status, "Body": []byte(body)})
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}

// codebuddyUpstreamBody answers one billing endpoint for one account.
func codebuddyUpstreamBody(account codebuddyAccountFixture, rawURL string) (string, int) {
	switch {
	case strings.HasSuffix(rawURL, UserResourcePath):
		if account.balanceStatus != 0 {
			return `{"code":500,"msg":"upstream said no"}`, account.balanceStatus
		}
		body, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"Accounts": []any{map[string]any{
							"PackageName":         "CodeBuddy个人体验版",
							"CapacityUnit":        "credit",
							"Status":              float64(0),
							"CycleCapacityRemain": account.credits,
							"CycleCapacitySize":   float64(1000),
							"CycleCapacityUsed":   float64(1000) - account.credits,
						}},
					},
				},
			},
		})
		return string(body), http.StatusOK
	case strings.HasSuffix(rawURL, CheckinActivityStatusPath):
		body, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{
				"active":           true,
				"today_checked_in": account.checkedIn,
				"streak_days":      account.streakDays,
				"daily_credit":     100,
				"activity_name":    "每日签到",
			},
		})
		return string(body), http.StatusOK
	}
	return `{}`, http.StatusNotFound
}

// twoCodebuddyAccounts is the canonical fixture: two accounts with distinct
// figures, both on the checked-in product.
func twoCodebuddyAccounts(t *testing.T) *abiboot.Host {
	t.Helper()
	return codebuddyQuotaHost(t, Config{Product: ProductCodeBuddy},
		codebuddyAccountFixture{
			name: "codebuddy-alice.json", authIndex: "idx-1", accessToken: "tok-alice",
			userID: "u-1", nickname: "alice", credits: 247.87, checkedIn: true, streakDays: 3,
		},
		codebuddyAccountFixture{
			name: "codebuddy-bob.json", authIndex: "idx-2", accessToken: "tok-bob",
			userID: "u-2", nickname: "bob", credits: 12.5, checkedIn: false,
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnFigures is the core contract.
func TestStatusJSONReportsEveryAccountWithItsOwnFigures(t *testing.T) {
	host := twoCodebuddyAccounts(t)
	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{}})
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
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
	if first["name"] != "codebuddy-alice.json" || second["name"] != "codebuddy-bob.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	firstCredits, _ := first["credits"].(map[string]any)
	secondCredits, _ := second["credits"].(map[string]any)
	if firstCredits["total"] != 247.87 || secondCredits["total"] != 12.5 {
		t.Errorf("per-account credits = %#v / %#v, want 247.87 / 12.5", firstCredits["total"], secondCredits["total"])
	}
	firstCheckin, _ := first["checkin"].(map[string]any)
	secondCheckin, _ := second["checkin"].(map[string]any)
	if firstCheckin["today_checked_in"] != true || secondCheckin["today_checked_in"] != false {
		t.Errorf("per-account check-in = %#v / %#v, want true / false", firstCheckin, secondCheckin)
	}
	if firstCheckin["streak_days"] != float64(3) {
		t.Errorf("first account streak = %#v, want 3", firstCheckin["streak_days"])
	}
	// The flat document keeps describing the selected (first) account.
	if document["name"] != "codebuddy-alice.json" {
		t.Errorf("top-level name = %#v, want the first account", document["name"])
	}
	if document["credits"].(map[string]any)["total"] != 247.87 {
		t.Errorf("top-level credits = %#v, want the first account's 247.87", document["credits"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selected := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"auth_index": {"idx-2"}}})
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selected.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status JSON: %v", errUnmarshal)
	}
	if selectedDocument["credits"].(map[string]any)["total"] != 12.5 {
		t.Errorf("selected credits = %#v, want the second account's 12.5", selectedDocument["credits"])
	}
	if len(selectedDocument["accounts"].([]any)) != 2 {
		t.Errorf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule: a failed
// balance read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	host := codebuddyQuotaHost(t, Config{Product: ProductCodeBuddy},
		codebuddyAccountFixture{
			name: "codebuddy-alice.json", authIndex: "idx-1", accessToken: "tok-alice",
			userID: "u-1", nickname: "alice", credits: 247.87, checkedIn: true,
		},
		codebuddyAccountFixture{
			name: "codebuddy-bob.json", authIndex: "idx-2", accessToken: "tok-bob",
			userID: "u-2", nickname: "bob", balanceStatus: http.StatusInternalServerError,
		},
	)
	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{"auth_index": {"idx-2"}}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a failing balance read must still serve the document, got HTTP %d", response.StatusCode)
	}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	// The selected account keeps the historical `credits: null` shape and gains
	// the reason; it never gains a number.
	if document["credits"] != nil {
		t.Errorf("a failed read must not publish a number: %#v", document["credits"])
	}
	if _, present := document["credit_error"]; !present {
		t.Errorf("a failed read must carry its reason: %#v", document)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want two entries", document["accounts"])
	}
	healthy, _ := accounts[0].(map[string]any)
	broken, _ := accounts[1].(map[string]any)
	if healthy["credits"].(map[string]any)["total"] != 247.87 {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["credits"]; present {
		t.Errorf("the broken account published a number: %#v", broken["credits"])
	}
	if _, present := broken["credit_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusJSONReportsAProductWithoutCheckinPerAccount pins the honest case:
// a product whose backend has no check-in endpoint reports that per account
// instead of an inactive activity or a repeated number.
func TestStatusJSONReportsAProductWithoutCheckinPerAccount(t *testing.T) {
	host := codebuddyQuotaHost(t, Config{Product: ProductWorkBuddy},
		codebuddyAccountFixture{
			name: "workbuddy-alice.json", authIndex: "idx-1", accessToken: "tok-alice",
			userID: "u-1", nickname: "alice", credits: 42,
		},
	)
	response := statusJSON(host, pluginapi.ManagementRequest{Query: map[string][]string{}})
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one entry", document["accounts"])
	}
	entry, _ := accounts[0].(map[string]any)
	if entry["checkin_supported"] != false {
		t.Errorf("checkin_supported = %#v, want false for a product without the endpoint", entry["checkin_supported"])
	}
	if _, present := entry["checkin"]; present {
		t.Errorf("a product without the endpoint must not report a check-in state: %#v", entry["checkin"])
	}
	if document["checkin_supported"] != false {
		t.Errorf("top-level checkin_supported = %#v, want false", document["checkin_supported"])
	}
}

// TestStatusPageRendersOneCreditCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own figures.
func TestStatusPageRendersOneCreditCardPerAccount(t *testing.T) {
	host := twoCodebuddyAccounts(t)
	body := string(renderStatusPage(host, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)

	if got := strings.Count(body, "账号与积分 · "); got != 2 {
		t.Fatalf("status page shows %d account cards, want one per account (2):\n%s", got, body)
	}
	for _, want := range []string{
		"codebuddy-alice.json",
		"codebuddy-bob.json",
		"247.87",
		"12.50",
		"今日签到</dt><dd>今日已签到",
		"今日签到</dt><dd>今日可签到",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	// The per-account actions name their own account, and adding an account stays
	// reachable without one.
	if !strings.Contains(body, "checkin?auth_index=idx-2") {
		t.Error("the second account's check-in link does not name the second account")
	}
	if !strings.Contains(body, "login?auth_index=idx-2") {
		t.Error("the second account's re-login link does not name the second account")
	}
	if !strings.Contains(body, "login?add=1") {
		t.Error("新建账号 is not reachable from the status page")
	}
	if strings.Contains(body, "<form") {
		t.Error("resource routes are dispatched as GET only, so no form may be rendered")
	}
}
