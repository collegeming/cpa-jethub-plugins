package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of Qoder's status document and status
// page: a channel with several accounts reports EVERY account's own figures.
//
// The two fixture accounts carry different access tokens, and the upstream stub
// routes by the Bearer token, so an implementation that fetched the balance once
// and reused it for both accounts fails these tests instead of rendering one
// account's numbers twice.

// qoderAccountFixture is one scripted account.
type qoderAccountFixture struct {
	name        string
	authIndex   string
	accessToken string
	nickname    string
	// quotaBody is what the usage endpoint answers for this account.
	quotaBody string
	// quotaStatus replaces the usage endpoint status when non-zero, which is how
	// a failing balance read is exercised.
	quotaStatus int
	// claimable drives the campaigns answer for this account.
	claimable bool
}

// qoderQuotaHost installs a fake host holding the given accounts.
func qoderQuotaHost(t *testing.T, accounts ...qoderAccountFixture) *fakeHost {
	t.Helper()
	host := newFakeHost()
	for _, account := range accounts {
		host.files = append(host.files, pluginapi.HostAuthFileEntry{
			AuthIndex: account.authIndex, ID: account.authIndex, Name: account.name,
			Provider: ProviderKey, Status: "active",
		})
		credential := &Credential{
			AccessToken: account.accessToken, SecurityOAuthToken: account.accessToken,
			RefreshToken: "ref-" + account.accessToken, MachineID: "machine-1",
			UID: "uid-" + account.accessToken, Nickname: account.nickname,
			Region: string(RegionGlobal), ExpireTime: timeNowPlusHour(),
		}
		stored, errEncode := credential.Encode()
		if errEncode != nil {
			t.Fatalf("encode credential: %v", errEncode)
		}
		host.auths[account.authIndex] = stored
	}
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		for _, account := range accounts {
			if !strings.Contains(request.Headers.Get("Authorization"), account.accessToken) {
				continue
			}
			switch {
			case strings.HasSuffix(request.URL, UsagePath):
				if account.quotaStatus != 0 {
					return httpResponse(account.quotaStatus, `{"message":"upstream said no"}`), nil
				}
				return httpResponse(http.StatusOK, account.quotaBody), nil
			case strings.HasSuffix(request.URL, CampaignsPath):
				body, _ := json.Marshal(map[string]any{
					"showCampaign": true,
					"claimable":    account.claimable,
					"campaigns": []any{map[string]any{
						"campaignId": "c-1", "campaignKey": "daily", "actionType": "CLAIM_BENEFIT",
						"claimStatus": map[bool]string{true: "CLAIMABLE", false: "CLAIMED"}[account.claimable],
						"benefit":     map[string]any{"kind": "CREDITS", "amount": 100},
					}},
				})
				return httpResponse(http.StatusOK, string(body)), nil
			}
		}
		return httpResponse(http.StatusNotFound, "{}"), nil
	}
	host.install(t)
	return host
}

// qoderUsageBody renders a usage answer with one credit package.
func qoderUsageBody(remaining, total float64) string {
	body, _ := json.Marshal(map[string]any{
		"displayMode": "qoder",
		"qoderUsage": map[string]any{
			"userType":   "personal_standard",
			"userQuota":  map[string]any{"total": 0, "used": 0, "remaining": 0, "unit": "credits"},
			"addOnQuota": map[string]any{"total": total, "used": total - remaining, "remaining": remaining, "unit": "credits"},
			"expiresAt":  253402214400000,
		},
	})
	return string(body)
}

// twoQoderAccounts is the canonical fixture: two accounts with distinct figures.
func twoQoderAccounts(t *testing.T) *fakeHost {
	t.Helper()
	return qoderQuotaHost(t,
		qoderAccountFixture{
			name: "qoder-alice.json", authIndex: "idx-1", accessToken: "tok-alice", nickname: "alice",
			quotaBody: qoderUsageBody(100, 100), claimable: true,
		},
		qoderAccountFixture{
			name: "qoder-bob.json", authIndex: "idx-2", accessToken: "tok-bob", nickname: "bob",
			quotaBody: qoderUsageBody(7, 50), claimable: false,
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnFigures is the core contract.
func TestStatusJSONReportsEveryAccountWithItsOwnFigures(t *testing.T) {
	twoQoderAccounts(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status", map[string]string{"format": "json"}, ""))
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
	if first["name"] != "qoder-alice.json" || second["name"] != "qoder-bob.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	firstCredits, _ := first["credits"].(map[string]any)
	secondCredits, _ := second["credits"].(map[string]any)
	if firstCredits["total"] != float64(100) || secondCredits["total"] != float64(7) {
		t.Errorf("per-account totals = %#v / %#v, want 100 / 7", firstCredits["total"], secondCredits["total"])
	}
	// Each account carries its OWN check-in state.
	firstCheckin, _ := first["daily_checkin"].(map[string]any)
	secondCheckin, _ := second["daily_checkin"].(map[string]any)
	if firstCheckin["claimable"] != true || secondCheckin["claimable"] != false {
		t.Errorf("per-account check-in = %#v / %#v, want true / false", firstCheckin, secondCheckin)
	}
	// The flat document keeps describing the selected (first) account.
	if document["account"].(map[string]any)["name"] != "qoder-alice.json" {
		t.Errorf("top-level account = %#v, want the first one", document["account"])
	}
	if document["credits"].(map[string]any)["total"] != float64(100) {
		t.Errorf("top-level credits = %#v, want the first account's 100", document["credits"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selected := managementCall(t, testHost(), managementRequest("/status",
		map[string]string{"format": "json", "auth_index": "idx-2"}, ""))
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selected.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status JSON: %v", errUnmarshal)
	}
	if selectedDocument["account"].(map[string]any)["name"] != "qoder-bob.json" {
		t.Errorf("selected account = %#v, want the second one", selectedDocument["account"])
	}
	if len(selectedDocument["accounts"].([]any)) != 2 {
		t.Errorf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule: a failed
// balance read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	qoderQuotaHost(t,
		qoderAccountFixture{
			name: "qoder-alice.json", authIndex: "idx-1", accessToken: "tok-alice", nickname: "alice",
			quotaBody: qoderUsageBody(100, 100), claimable: true,
		},
		qoderAccountFixture{
			name: "qoder-bob.json", authIndex: "idx-2", accessToken: "tok-bob", nickname: "bob",
			quotaStatus: http.StatusInternalServerError,
		},
	)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest("/status",
		map[string]string{"format": "json", "auth_index": "idx-2"}, ""))
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
	if healthy["credits"].(map[string]any)["total"] != float64(100) {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["credits"]; present {
		t.Errorf("the broken account published a number: %#v", broken["credits"])
	}
	if _, present := broken["credit_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusPageRendersOneQuotaCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own figures.
func TestStatusPageRendersOneQuotaCardPerAccount(t *testing.T) {
	twoQoderAccounts(t)
	withSettings(t, DefaultConfig())

	response := managementCall(t, testHost(), managementRequest(
		"/v0/resource/plugins/qoder/status", nil, "text/html,application/xhtml+xml"))
	page := string(response.Body)

	if got := strings.Count(page, "额度 · "); got != 2 {
		t.Fatalf("status page shows %d quota cards, want one per account (2):\n%s", got, page)
	}
	for _, want := range []string{
		"qoder-alice.json",
		"qoder-bob.json",
		"剩余合计",
		"100 credits",
		"7 credits",
		"可领取",
		"今日已领取或不可领取",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	// The per-account actions name their own account, and adding an account stays
	// reachable without one.
	if !strings.Contains(page, "checkin?auth_index=idx-2") {
		t.Error("the second account's check-in link does not name the second account")
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
