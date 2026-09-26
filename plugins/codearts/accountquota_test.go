package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of the status document and the status
// page: a channel with several accounts reports EVERY account's own figures.
//
// The fixtures mirror the live shape: two credentials of the same provider with
// different access keys (that is how the real instance looks), each answering
// `statistics/plugin` with its own numbers. The upstream stub routes by the
// signed Authorization header, so an implementation that fetched the balance
// once and reused it for both accounts fails these tests instead of rendering
// one account's numbers twice.

// quotaAccount is one account of the fixture.
type quotaAccount struct {
	name      string
	authIndex string
	accessKey string
	// remaining/used/total are what statistics/plugin reports for this account.
	remaining float64
	used      float64
	total     float64
	// claimable/status are what /v1/ops/delivery reports for this account.
	claimable bool
	status    string
	// balanceStatus replaces the package-info HTTP status when non-zero, which is
	// how a failing balance read is exercised.
	balanceStatus int
}

// codeartsQuotaHost installs a host transport holding the given accounts.
func codeartsQuotaHost(t *testing.T, accounts ...quotaAccount) *abiboot.Host {
	t.Helper()
	byAccessKey := map[string]quotaAccount{}
	for _, account := range accounts {
		byAccessKey[account.accessKey] = account
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
					Type:            ProviderKey,
					AccessKeyID:     account.accessKey,
					SecretAccessKey: "sk-" + account.accessKey,
					ExpiresAt:       "2027-01-02T03:04:05Z",
					UserName:        "user-" + account.accessKey,
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
			var account quotaAccount
			found := false
			for key, candidate := range byAccessKey {
				if strings.Contains(payload.Headers.Get("Authorization"), key) {
					account, found = candidate, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("no account matches the request signature: %s", payload.Headers.Get("Authorization"))
			}
			body, status := quotaUpstreamBody(account, payload.URL)
			return abiboot.OK(map[string]any{
				"StatusCode": status,
				"Body":       base64.StdEncoding.EncodeToString([]byte(body)),
			})
		case pluginabi.MethodHostLog:
			return abiboot.OK(map[string]any{})
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	})
	t.Cleanup(abiboot.ClearHostCaller)
	return abiboot.NewHost(json.RawMessage(`{"host_callback_id":"test-callback"}`))
}

// quotaUpstreamBody answers one CodeArts endpoint for one account.
func quotaUpstreamBody(account quotaAccount, rawURL string) (string, int) {
	switch {
	case strings.Contains(rawURL, PackageInfoPath):
		if account.balanceStatus != 0 {
			return `{"error":"upstream said no"}`, account.balanceStatus
		}
		return fmt.Sprintf(
			`{"package":{"is_credit_package":true},"metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":%v,"package_credit_used":%v,"package_credit_remain":%v}]}`,
			account.total, account.used, account.remaining), http.StatusOK
	case strings.Contains(rawURL, OpsDeliveryPath):
		status := "null"
		if account.status != "" {
			encoded, _ := json.Marshal(account.status)
			status = string(encoded)
		}
		return fmt.Sprintf(
			`{"code":0,"message":"ok","data":{"items":[{"campaignId":"1","type":"%s","claimable":%t,"status":%s,"benefitAmount":10}]}}`,
			DailyLoginType, account.claimable, status), http.StatusOK
	}
	return `{}`, http.StatusNotFound
}

// twoAccountHost is the canonical fixture: two accounts with figures that must
// never be confused with each other.
func twoAccountHost(t *testing.T) *abiboot.Host {
	t.Helper()
	return codeartsQuotaHost(t,
		quotaAccount{
			name: "codearts-HSTAANV6KVKBXL9FKENJ.json", authIndex: "idx-1", accessKey: "HSTAANV6KVKBXL9FKENJ",
			remaining: 1739.5, used: 760.5, total: 2500, claimable: false, status: "CONFIRMED",
		},
		quotaAccount{
			name: "codearts-HSTAVFVPPZRCVTMS4TVV.json", authIndex: "idx-2", accessKey: "HSTAVFVPPZRCVTMS4TVV",
			remaining: 7, used: 93, total: 100, claimable: true,
		},
	)
}

// decodeDocument reads a JSON management response back into a map.
func decodeDocument(t *testing.T, response pluginapi.ManagementResponse) map[string]any {
	t.Helper()
	var document map[string]any
	if errDecode := json.Unmarshal(response.Body, &document); errDecode != nil {
		t.Fatalf("decode status document: %v\n%s", errDecode, response.Body)
	}
	return document
}

// accountsOf returns the document's accounts array, failing when it is absent.
func accountsOf(t *testing.T, document map[string]any) []map[string]any {
	t.Helper()
	raw, ok := document["accounts"].([]any)
	if !ok {
		t.Fatalf("status document has no accounts array: %#v", document["accounts"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		object, okObject := item.(map[string]any)
		if !okObject {
			t.Fatalf("accounts entry is %T, want an object", item)
		}
		out = append(out, object)
	}
	return out
}

// TestStatusJSONReportsEveryAccountWithItsOwnFigures is the core contract: the
// document lists all accounts, each carrying its own numbers, while the selected
// account's fields stay at the top level for the consumers that read them there.
func TestStatusJSONReportsEveryAccountWithItsOwnFigures(t *testing.T) {
	host := twoAccountHost(t)
	request := pluginapi.ManagementRequest{Query: map[string][]string{"format": {"json"}}}
	document := decodeDocument(t, statusJSON(host, request))

	if got := document["account_count"]; got != float64(2) {
		t.Errorf("account_count = %#v, want 2", got)
	}
	if got := document["name"]; got != "codearts-HSTAANV6KVKBXL9FKENJ.json" {
		t.Errorf("top-level name = %#v, want the first account", got)
	}
	if got := document["remaining"]; got != 1739.5 {
		t.Errorf("top-level remaining = %#v, want 1739.5", got)
	}

	accounts := accountsOf(t, document)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d entries, want 2", len(accounts))
	}
	first, second := accounts[0], accounts[1]
	if first["name"] != "codearts-HSTAANV6KVKBXL9FKENJ.json" || second["name"] != "codearts-HSTAVFVPPZRCVTMS4TVV.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	if first["remaining"] != 1739.5 || second["remaining"] != 7.0 {
		t.Errorf("per-account remaining = %#v / %#v, want 1739.5 / 7", first["remaining"], second["remaining"])
	}
	if first["used"] != 760.5 || second["used"] != 93.0 {
		t.Errorf("per-account used = %#v / %#v, want 760.5 / 93", first["used"], second["used"])
	}
	if first["total"] != 2500.0 || second["total"] != 100.0 {
		t.Errorf("per-account total = %#v / %#v, want 2500 / 100", first["total"], second["total"])
	}
	if first["credit_package"] != true || second["credit_package"] != true {
		t.Errorf("per-account credit_package = %#v / %#v, want true / true", first["credit_package"], second["credit_package"])
	}

	// Each account carries its OWN check-in state: one already claimed today,
	// the other still claimable.
	firstCheckin, okFirst := first["daily_checkin"].(map[string]any)
	secondCheckin, okSecond := second["daily_checkin"].(map[string]any)
	if !okFirst || !okSecond {
		t.Fatalf("daily_checkin missing from an account entry: %#v", accounts)
	}
	if firstCheckin["status"] != "CONFIRMED" || firstCheckin["claimable"] != false {
		t.Errorf("first account check-in = %#v, want CONFIRMED/not claimable", firstCheckin)
	}
	if secondCheckin["claimable"] != true {
		t.Errorf("second account check-in = %#v, want claimable", secondCheckin)
	}
}

// TestStatusJSONSelectorMovesTheTopLevelFields pins that the selector still
// decides which account's fields sit at the top level, while the accounts array
// never loses an account.
func TestStatusJSONSelectorMovesTheTopLevelFields(t *testing.T) {
	host := twoAccountHost(t)
	request := pluginapi.ManagementRequest{Query: map[string][]string{
		"format": {"json"}, "auth_index": {"idx-2"},
	}}
	document := decodeDocument(t, statusJSON(host, request))

	if document["name"] != "codearts-HSTAVFVPPZRCVTMS4TVV.json" {
		t.Errorf("top-level name = %#v, want the selected second account", document["name"])
	}
	if document["remaining"] != 7.0 {
		t.Errorf("top-level remaining = %#v, want the second account's 7", document["remaining"])
	}
	if got := len(accountsOf(t, document)); got != 2 {
		t.Errorf("accounts = %d entries, want 2 regardless of the selection", got)
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the repo's oldest honesty rule:
// a failed balance read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	host := codeartsQuotaHost(t,
		quotaAccount{
			name: "codearts-good.json", authIndex: "idx-1", accessKey: "AKGOOD",
			remaining: 12, used: 1, total: 13, claimable: true,
		},
		quotaAccount{
			name: "codearts-broken.json", authIndex: "idx-2", accessKey: "AKBROKEN",
			balanceStatus: http.StatusInternalServerError,
		},
	)
	document := decodeDocument(t, statusJSON(host, pluginapi.ManagementRequest{
		Query: map[string][]string{"format": {"json"}, "auth_index": {"idx-2"}},
	}))

	if response := statusJSON(host, pluginapi.ManagementRequest{
		Query: map[string][]string{"format": {"json"}, "auth_index": {"idx-2"}},
	}); response.StatusCode != http.StatusOK {
		t.Fatalf("a failing balance read must still serve the document, got HTTP %d", response.StatusCode)
	}
	if _, present := document["remaining"]; present {
		t.Errorf("a failed read must not publish a number: %#v", document["remaining"])
	}
	if _, present := document["credit_error"]; !present {
		t.Errorf("a failed read must carry its reason: %#v", document)
	}

	accounts := accountsOf(t, document)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d entries, want 2", len(accounts))
	}
	if accounts[0]["remaining"] != 12.0 {
		t.Errorf("the healthy account lost its figures: %#v", accounts[0])
	}
	if _, present := accounts[1]["credit_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", accounts[1])
	}
	if _, present := accounts[1]["remaining"]; present {
		t.Errorf("the broken account published a number: %#v", accounts[1]["remaining"])
	}
}

// TestStatusPageRendersOneQuotaCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own numbers.
func TestStatusPageRendersOneQuotaCardPerAccount(t *testing.T) {
	host := twoAccountHost(t)
	body := string(renderStatusPage(host, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)

	if got := strings.Count(body, "额度 · "); got != 2 {
		t.Fatalf("status page shows %d quota cards, want one per account (2):\n%s", got, body)
	}
	for _, want := range []string{
		"codearts-HSTAANV6KVKBXL9FKENJ.json",
		"codearts-HSTAVFVPPZRCVTMS4TVV.json",
		"1739.50",
		"7.00",
		"今日已领取",
		"可领取",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page is missing %q", want)
		}
	}
	// The per-account actions name their own account, and adding an account stays
	// reachable without one.
	if !strings.Contains(body, "action=checkin&amp;auth_index=idx-2") {
		t.Error("the second account's 签到 link does not name the second account")
	}
	if !strings.Contains(body, "login?add=1") {
		t.Error("新建账号 is not reachable from the status page")
	}
	if strings.Contains(body, "<form") {
		t.Error("resource routes are dispatched as GET only, so no form may be rendered")
	}
}

// TestStatusPageKeepsNewAccountWithASingleAccount guards the case that used to
// hide 新建账号: the switcher card was only rendered when there were two or more
// accounts, so a one-account instance had no way to add a second one.
func TestStatusPageKeepsNewAccountWithASingleAccount(t *testing.T) {
	host := codeartsQuotaHost(t, quotaAccount{
		name: "codearts-only.json", authIndex: "idx-1", accessKey: "AKONLY",
		remaining: 5, used: 5, total: 10,
	})
	body := string(renderStatusPage(host, pluginapi.ManagementRequest{Query: map[string][]string{}}).Body)
	if !strings.Contains(body, "login?add=1") {
		t.Fatalf("a one-account instance offers no way to add another:\n%s", body)
	}
	if got := strings.Count(body, "额度 · "); got != 1 {
		t.Errorf("status page shows %d quota cards, want 1", got)
	}
}
