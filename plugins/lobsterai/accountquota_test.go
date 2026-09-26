package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file pins the per-account contract of LobsterAI's status document and
// status page: a channel with several accounts reports EVERY account's own
// credits and activity state.
//
// The two fixture accounts carry different access tokens, and the upstream stub
// binds each route to one account through the Authorization header, so an
// implementation that fetched the balance once and reused it for both accounts
// fails these tests instead of rendering one account's numbers twice.

// lobsterAccountFixture is one scripted account.
type lobsterAccountFixture struct {
	name        string
	authIndex   string
	accessToken string
	uid         string
	nickname    string
	// credits is the profile-summary total, or `balanceStatus` a failing status.
	credits       float64
	slotState     string
	balanceStatus int
}

// lobsterQuotaHost installs a fake host holding the given accounts.
func lobsterQuotaHost(t *testing.T, accounts ...lobsterAccountFixture) *fakeHost {
	t.Helper()
	fake := newFakeHost()
	// withFiles REPLACES the listing, so every account goes in one call.
	entries := make([]pluginapi.HostAuthFileEntry, 0, len(accounts))
	for _, account := range accounts {
		entries = append(entries, pluginapi.HostAuthFileEntry{
			AuthIndex: account.authIndex, ID: account.authIndex, Name: account.name,
			Provider: ProviderKey, Status: "active",
		})
		fake.withAuthJSON(account.authIndex, string(mustJSON(t, &Credential{
			Type: ProviderKey, AccessToken: account.accessToken, RefreshToken: "refresh-" + account.accessToken,
			UID: account.uid, Nickname: account.nickname, UUID: "uuid-" + account.uid,
		})))
		if account.balanceStatus != 0 {
			fake.on(httpRoute{
				Method: http.MethodGet, Match: ProfileSummaryPath, Status: account.balanceStatus,
				HeaderName: "Authorization", HeaderValue: account.accessToken,
			})
			continue
		}
		fake.on(httpRoute{
			Method: http.MethodGet, Match: ProfileSummaryPath, HeaderName: "Authorization", HeaderValue: account.accessToken,
			Body: `{"code":0,"data":{"totalCreditsRemaining":` + formatCredits(account.credits) +
				`,"creditItems":[{"type":"activity","creditsRemaining":` + formatCredits(account.credits) + `}]}}`,
		})
		fake.on(httpRoute{
			Method: http.MethodGet, Match: ActivitySlotPath, HeaderName: "Authorization", HeaderValue: account.accessToken,
			Body: `{"code":0,"data":{"slotState":"` + account.slotState + `","activity":{"activityCode":"daily-1","configRevision":3}}}`,
		})
	}
	fake.on(httpRoute{Method: http.MethodGet, Match: "api-overmind", Body: `{"data":{"value":{"version":"2026.9.4"}},"code":0}`})
	fake.withFiles(entries...)
	return fake
}

// formatCredits renders a credit amount without an exponent, so the JSON fixture
// is a plain number.
func formatCredits(value float64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// twoLobsterAccounts is the canonical fixture: two accounts with distinct figures.
func twoLobsterAccounts(t *testing.T) *fakeHost {
	t.Helper()
	return lobsterQuotaHost(t,
		lobsterAccountFixture{
			name: "lobsterai-uid-1.json", authIndex: "idx-1", accessToken: "tok-alice",
			uid: "uid-1", nickname: "alice", credits: 42.5, slotState: "available",
		},
		lobsterAccountFixture{
			name: "lobsterai-uid-2.json", authIndex: "idx-2", accessToken: "tok-bob",
			uid: "uid-2", nickname: "bob", credits: 7, slotState: "claimed",
		},
	)
}

// TestStatusJSONReportsEveryAccountWithItsOwnFigures is the core contract.
func TestStatusJSONReportsEveryAccountWithItsOwnFigures(t *testing.T) {
	fake := twoLobsterAccounts(t)
	host := installFakeHost(t, fake)

	response := statusJSON(host, pluginapi.ManagementRequest{})
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
	if first["name"] != "lobsterai-uid-1.json" || second["name"] != "lobsterai-uid-2.json" {
		t.Fatalf("accounts are not in host order: %#v", accounts)
	}
	firstCredit, _ := first["credit"].(map[string]any)
	secondCredit, _ := second["credit"].(map[string]any)
	if firstCredit["total"] != 42.5 || secondCredit["total"] != 7.0 {
		t.Errorf("per-account credits = %#v / %#v, want 42.5 / 7", firstCredit["total"], secondCredit["total"])
	}
	firstActivity, _ := first["activity"].(map[string]any)
	secondActivity, _ := second["activity"].(map[string]any)
	if firstActivity["slot_state"] != "available" || secondActivity["slot_state"] != "claimed" {
		t.Errorf("per-account activity = %#v / %#v, want available / claimed", firstActivity, secondActivity)
	}
	// The flat document keeps describing the selected (first) account.
	selected, _ := document["selected"].(map[string]any)
	if selected["name"] != "lobsterai-uid-1.json" {
		t.Errorf("top-level selected = %#v, want the first account", document["selected"])
	}
	if selected["credit"].(map[string]any)["total"] != 42.5 {
		t.Errorf("top-level credit = %#v, want the first account's 42.5", selected["credit"])
	}

	// Selecting the second account moves the flat fields without losing a row.
	selectedResponse := statusJSON(host, pluginapi.ManagementRequest{
		Query: url.Values{"auth_index": {"idx-2"}},
	})
	var selectedDocument map[string]any
	if errUnmarshal := json.Unmarshal(selectedResponse.Body, &selectedDocument); errUnmarshal != nil {
		t.Fatalf("decode selected status JSON: %v", errUnmarshal)
	}
	selectedAgain, _ := selectedDocument["selected"].(map[string]any)
	if selectedAgain["name"] != "lobsterai-uid-2.json" {
		t.Errorf("selected account = %#v, want the second one", selectedDocument["selected"])
	}
	if len(selectedDocument["accounts"].([]any)) != 2 {
		t.Errorf("accounts lost a row when a selector was used: %#v", selectedDocument["accounts"])
	}
}

// TestStatusJSONReportsAFailedReadAsUnknown pins the honesty rule: a failed
// credit read is an error field, never a 0 that reads like "spent".
func TestStatusJSONReportsAFailedReadAsUnknown(t *testing.T) {
	fake := lobsterQuotaHost(t,
		lobsterAccountFixture{
			name: "lobsterai-uid-1.json", authIndex: "idx-1", accessToken: "tok-alice",
			uid: "uid-1", nickname: "alice", credits: 42.5, slotState: "available",
		},
		lobsterAccountFixture{
			name: "lobsterai-uid-2.json", authIndex: "idx-2", accessToken: "tok-bob",
			uid: "uid-2", nickname: "bob", balanceStatus: http.StatusInternalServerError,
		},
	)
	host := installFakeHost(t, fake)

	response := statusJSON(host, pluginapi.ManagementRequest{Query: url.Values{"auth_index": {"idx-2"}}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("a failing credit read must still serve the document, got HTTP %d", response.StatusCode)
	}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status JSON: %v", errUnmarshal)
	}
	selected, _ := document["selected"].(map[string]any)
	if _, present := selected["credit"]; present {
		t.Errorf("a failed read must not publish a number: %#v", selected["credit"])
	}
	if _, present := selected["credit_error"]; !present {
		t.Errorf("a failed read must carry its reason: %#v", selected)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want two entries", document["accounts"])
	}
	healthy, _ := accounts[0].(map[string]any)
	broken, _ := accounts[1].(map[string]any)
	if healthy["credit"].(map[string]any)["total"] != 42.5 {
		t.Errorf("the healthy account lost its figures: %#v", healthy)
	}
	if _, present := broken["credit"]; present {
		t.Errorf("the broken account published a number: %#v", broken["credit"])
	}
	if _, present := broken["credit_error"]; !present {
		t.Errorf("the broken account carries no reason: %#v", broken)
	}
}

// TestStatusPageRendersOneCreditCardPerAccount is the display half of the
// contract: two accounts, two blocks, each with its own figures.
func TestStatusPageRendersOneCreditCardPerAccount(t *testing.T) {
	fake := twoLobsterAccounts(t)
	host := installFakeHost(t, fake)

	response := renderStatusPage(host, pluginapi.ManagementRequest{})
	body := string(response.Body)

	if got := strings.Count(body, "积分 · "); got != 2 {
		t.Fatalf("status page shows %d credit cards, want one per account (2):\n%s", got, firstLines(body, 40))
	}
	for _, want := range []string{
		"lobsterai-uid-1.json",
		"lobsterai-uid-2.json",
		"剩余积分</dt><dd>42.50",
		"剩余积分</dt><dd>7.00",
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
}
