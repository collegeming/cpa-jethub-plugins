package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of TRAE's status document and status
// page: a channel with several accounts reports EVERY account's own credits and
// check-in state.
//
// The two fixture accounts carry different access tokens, and the upstream stub
// routes the credit/check-in endpoints by the Authorization header, so an
// implementation that fetched the balance once and reused it for both accounts
// fails these tests instead of rendering one account's numbers twice.

// traeAccountFixture is one scripted account.
type traeAccountFixture struct {
	name        string
	authIndex   string
	accessToken string
	uid         string
	nickname    string
	// limit/used shape the EntUsagePath answer: remaining = limit - used.
	limit float64
	used  float64
	// checkedIn/streakDays shape the CheckinStatusPath answer.
	checkedIn  bool
	streakDays int
	// balanceStatus replaces the entitlement endpoint status when non-zero,
	// which is how a failing balance read is exercised.
	balanceStatus int
}

// traeQuotaHost installs a host transport holding the given accounts.
func traeQuotaHost(t *testing.T, accounts ...traeAccountFixture) {
	t.Helper()
	encoded := make(map[string]json.RawMessage, len(accounts))
	for _, account := range accounts {
		raw, errMarshal := json.Marshal(&Credential{
			Type:         ProviderKey,
			AccessToken:  account.accessToken,
			RefreshToken: "refresh-" + account.accessToken,
			ExpiresAt:    "99999999999999",
			UID:          account.uid,
			Nickname:     account.nickname,
			MachineID:    "0123456789abcdef0123456789abcdef",
			DeviceID:     "fedcba9876543210fedcba9876543210",
			Region:       RegionCN,
		})
		if errMarshal != nil {
			t.Fatalf("marshal credential: %v", errMarshal)
		}
		encoded[account.authIndex] = raw
	}
	fakeHost(t, func(method string, request []byte) (any, error) {
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
			return map[string]any{"files": files}, nil
		case pluginabi.MethodHostAuthGet:
			var payload struct {
				AuthIndex string `json:"auth_index"`
			}
			_ = json.Unmarshal(request, &payload)
			raw, okRaw := encoded[payload.AuthIndex]
			if !okRaw {
				return nil, abiboot.Errorf("auth_not_found", "no auth %s", payload.AuthIndex)
			}
			return map[string]any{"auth_index": payload.AuthIndex, "json": raw}, nil
		case pluginabi.MethodHostHTTPDo:
			var payload struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(request, &payload)
			authorization := payload.Headers.Get("Authorization")
			for _, account := range accounts {
				if !strings.Contains(authorization, account.accessToken) {
					continue
				}
				switch {
				case strings.Contains(payload.URL, BatchModelsPath):
					return map[string]any{
						"StatusCode": 200,
						"Body":       base64.StdEncoding.EncodeToString(batchBody()),
					}, nil
				case strings.Contains(payload.URL, EntUsagePath):
					if account.balanceStatus != 0 {
						return map[string]any{"StatusCode": account.balanceStatus, "Body": ""}, nil
					}
					body, _ := json.Marshal(map[string]any{
						"user_entitlement_pack_list": []any{map[string]any{
							"entitlement_base_info": map[string]any{
								"name":  "签到奖励",
								"quota": map[string]any{"credits_limit": account.limit},
							},
							"usage": map[string]any{"credits_amount": account.used},
						}},
					})
					return map[string]any{
						"StatusCode": 200,
						"Body":       base64.StdEncoding.EncodeToString(body),
					}, nil
				case strings.Contains(payload.URL, CheckinStatusPath):
					body, _ := json.Marshal(map[string]any{
						"code": 0, "checked_in": account.checkedIn,
						"credits": account.limit, "streak_days": account.streakDays, "enable": true,
					})
					return map[string]any{
						"StatusCode": 200,
						"Body":       base64.StdEncoding.EncodeToString(body),
					}, nil
				}
				return map[string]any{"StatusCode": 404, "Body": ""}, nil
			}
			return nil, abiboot.Errorf("unknown_account", "no account matches %s", authorization)
		case pluginabi.MethodHostLog:
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})
}

// twoTraeAccounts is the canonical fixture: two accounts with distinct figures.
func twoTraeAccounts(t *testing.T) {
	t.Helper()
	traeQuotaHost(t,
		traeAccountFixture{
			name: "trae-alice.json", authIndex: "idx-1", accessToken: "tok-alice",
			uid: "uid-1", nickname: "alice", limit: 200, used: 50, checkedIn: true, streakDays: 3,
		},
		traeAccountFixture{
			name: "trae-bob.json", authIndex: "idx-2", accessToken: "tok-bob",
			uid: "uid-2", nickname: "bob", limit: 100, used: 90, checkedIn: false,
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnFigures is the core contract.
func TestStatusJSONReportsEveryAccountWithItsOwnFigures(t *testing.T) {
	twoTraeAccounts(t)
	response := statusJSON(abiboot.NewHost(nil), pluginapi.ManagementRequest{})
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
	if first["name"] != "trae-alice.json" || second["name"] != "trae-bob.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	if first["credits"] != float64(150) || second["credits"] != float64(10) {
		t.Errorf("per-account credits = %#v / %#v, want 150 / 10", first["credits"], second["credits"])
	}
	firstCheckin, _ := first["daily_checkin"].(map[string]any)
	secondCheckin, _ := second["daily_checkin"].(map[string]any)
	if firstCheckin["checked_in"] != true || secondCheckin["checked_in"] != false {
		t.Errorf("per-account check-in = %#v / %#v, want true / false", firstCheckin, secondCheckin)
	}
	if firstCheckin["streak_days"] != float64(3) {
		t.Errorf("first account streak = %#v, want 3", firstCheckin["streak_days"])
	}
	// The catalog stays on the selected account's entry (one extra request per
	// account is not worth it), and the flat document keeps describing it.
	if first["models"] == nil || second["models"] != nil {
		t.Errorf("catalog fields = %#v / %#v, want them on the selected entry only", first["models"], second["models"])
	}
	if document["credits"] != float64(150) {
		t.Errorf("top-level credits = %#v, want the selected account's 150", document["credits"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selected := statusJSON(abiboot.NewHost(nil), pluginapi.ManagementRequest{
		Query: map[string][]string{"auth_index": {"idx-2"}},
	})
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selected.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status JSON: %v", errUnmarshal)
	}
	if selectedDocument["credits"] != float64(10) {
		t.Errorf("selected credits = %#v, want the second account's 10", selectedDocument["credits"])
	}
	selectedAccounts, _ := selectedDocument["accounts"].([]any)
	if len(selectedAccounts) != 2 {
		t.Fatalf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
	if selectedAccounts[1].(map[string]any)["models"] == nil {
		t.Errorf("the catalog did not follow the selection: %#v", selectedAccounts[1])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule: a failed
// balance read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	traeQuotaHost(t,
		traeAccountFixture{
			name: "trae-alice.json", authIndex: "idx-1", accessToken: "tok-alice",
			uid: "uid-1", nickname: "alice", limit: 200, used: 50, checkedIn: true,
		},
		traeAccountFixture{
			name: "trae-bob.json", authIndex: "idx-2", accessToken: "tok-bob",
			uid: "uid-2", nickname: "bob", balanceStatus: http.StatusInternalServerError,
		},
	)
	response := statusJSON(abiboot.NewHost(nil), pluginapi.ManagementRequest{
		Query: map[string][]string{"auth_index": {"idx-2"}},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a failing balance read must still serve the document, got HTTP %d", response.StatusCode)
	}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	if _, present := document["credits"]; present {
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
	if healthy["credits"] != float64(150) {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["credits"]; present {
		t.Errorf("the broken account published a number: %#v", broken["credits"])
	}
	if _, present := broken["credit_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusPageRendersOneCreditCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own figures.
func TestStatusPageRendersOneCreditCardPerAccount(t *testing.T) {
	twoTraeAccounts(t)
	body := string(renderStatusPage(abiboot.NewHost(nil), pluginapi.ManagementRequest{}).Body)

	if got := strings.Count(body, "积分与签到 · "); got != 2 {
		t.Fatalf("status page shows %d credit cards, want one per account (2):\n%s", got, body)
	}
	for _, want := range []string{
		"trae-alice.json",
		"trae-bob.json",
		"剩余积分</dt><dd>150",
		"剩余积分</dt><dd>10",
		"今日已签到</dt><dd>是",
		"今日已签到</dt><dd>否",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	// The per-account actions name their own account, and adding an account stays
	// reachable without one.
	if !strings.Contains(body, "checkin&amp;auth_index=idx-2") {
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
