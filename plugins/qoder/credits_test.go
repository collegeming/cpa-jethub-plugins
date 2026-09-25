package main

import (
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// usageBody is the response shape measured against a real account
// (`qoder-credits.ts:18-25`). The point of the fixture is that the interesting
// balance sits in `addOnQuota`, not `userQuota`.
const usageBody = `{
  "displayMode": "qoder",
  "qoderUsage": {
    "userType": "personal_standard",
    "userQuota":  {"total": 0,   "used": 0, "remaining": 0,   "unit": "credits"},
    "addOnQuota": {"total": 100, "used": 0, "remaining": 100, "unit": "credits"},
    "expiresAt": 253402214400000
  }
}`

// TestFetchCreditBalanceReadsEveryPackage guards the reported "shows 0 credits"
// defect: the balance is not only in `userQuota` (`qoder-credits.ts:27-29`).
func TestFetchCreditBalanceReadsEveryPackage(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(request.URL, UsagePath) {
			t.Fatalf("URL = %q, want the usage endpoint", request.URL)
		}
		if got := request.Headers.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("Authorization = %q, want the bearer token", got)
		}
		if got := request.Headers.Get("Cosy-ClientType"); got != "5" {
			t.Fatalf("Cosy-ClientType = %q, want 5", got)
		}
		return httpResponse(200, usageBody), nil
	}
	host.install(t)

	credential := &Credential{AccessToken: "tok", MachineID: "m"}
	balance, errBalance := fetchCreditBalance(testHost(), credential, DefaultConfig())
	if errBalance != nil {
		t.Fatalf("fetchCreditBalance: %v", errBalance)
	}
	if balance == nil {
		t.Fatal("balance is nil for a usable response")
	}
	if balance.Total != 100 {
		t.Fatalf("total = %v, want 100 from the add-on package", balance.Total)
	}
	if len(balance.Packages) != 2 || balance.Packages[1].Name != "资源包" {
		t.Fatalf("packages = %#v, want the plan and the add-on package", balance.Packages)
	}
}

// TestFetchCreditBalanceEnterpriseIsNotApplicable keeps an enterprise account
// from being reported as "0 credits" (`qoder-credits.ts:193-194`).
func TestFetchCreditBalanceEnterpriseIsNotApplicable(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"displayMode":"enterprise","enterpriseUsage":{"detailUrl":"https://x"}}`), nil
	}
	host.install(t)
	balance, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig())
	if errBalance != nil {
		t.Fatalf("enterprise accounts must not be an error: %v", errBalance)
	}
	if balance != nil {
		t.Fatalf("balance = %#v, want nil for enterprise", balance)
	}
}

// TestFetchCreditBalanceFailuresAreErrors keeps a network failure distinguishable
// from a zero balance.
func TestFetchCreditBalanceFailuresAreErrors(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(500, "boom"), nil
	}
	host.install(t)
	if _, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig()); errBalance == nil {
		t.Fatal("a 500 response must be an error")
	}
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"displayMode":"qoder","qoderUsage":{}}`), nil
	}
	if _, errBalance := fetchCreditBalance(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig()); errBalance == nil {
		t.Fatal("a response without any package must be an error, not a zero balance")
	}
}

// TestToPackageClampsNegatives covers the metering-rollback case that would
// otherwise render "-12.5 credits" (`qoder-credits.ts:117-124`).
func TestToPackageClampsNegatives(t *testing.T) {
	pkg, ok := toPackage("p", map[string]any{"total": float64(10), "used": float64(30), "remaining": float64(-5)}, "")
	if !ok {
		t.Fatal("a package with numbers was rejected")
	}
	if pkg.Remaining != 0 || pkg.Used != 30 || pkg.Total != 10 {
		t.Fatalf("package = %#v, want the negative remainder clamped to 0", pkg)
	}
	derived, ok := toPackage("p", map[string]any{"total": float64(10), "used": float64(4)}, "")
	if !ok || derived.Remaining != 6 {
		t.Fatalf("package = %#v (ok=%v), want remaining derived as total-used", derived, ok)
	}
	if _, ok := toPackage("p", map[string]any{}, ""); ok {
		t.Fatal("an empty quota object must be rejected")
	}
}

// TestReadNumberAcceptsStrings covers a JSON number delivered as text.
func TestReadNumberAcceptsStrings(t *testing.T) {
	if value, ok := readNumber(map[string]any{"remaining": "12.5"}, "remaining"); !ok || value != 12.5 {
		t.Fatalf("readNumber = %v (ok=%v), want 12.5", value, ok)
	}
	if _, ok := readNumber(map[string]any{"remaining": "abc"}, "remaining"); ok {
		t.Fatal("readNumber accepted a non-numeric string")
	}
}

// campaignsBody mirrors the measured campaigns response (`qoder-credits.ts:41-51`).
const campaignsBody = `{
  "showCampaign": true,
  "claimable": true,
  "campaigns": [
    {"campaignId":"01a0bf8d","campaignKey":"act-20260921-308","actionType":"CLAIM_BENEFIT",
     "claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},
    {"campaignId":"01a0bf8e","campaignKey":"pro-double","actionType":"VIEW_DETAILS",
     "claimStatus":"CLAIMABLE","benefit":{"amount":999}}
  ]
}`

// TestParseCampaignsKeepsOnlyClaimBenefits pins the rule that a link-only
// campaign must not be claimed (`qoder-credits.ts:239-243`, `:365-369`).
func TestParseCampaignsKeepsOnlyClaimBenefits(t *testing.T) {
	parsed, errParse := parseCampaigns([]byte(campaignsBody))
	if errParse != nil {
		t.Fatalf("parseCampaigns: %v", errParse)
	}
	if !parsed.ShowCampaign || !parsed.Claimable || len(parsed.Campaigns) != 2 {
		t.Fatalf("parsed = %#v", parsed)
	}
	claimable := claimableCampaigns(parsed)
	if len(claimable) != 1 || claimable[0].CampaignID != "01a0bf8d" {
		t.Fatalf("claimable = %#v, want only the CLAIM_BENEFIT campaign", claimable)
	}
	if claimable[0].Amount != 100 {
		t.Fatalf("amount = %v, want 100", claimable[0].Amount)
	}
}

// TestClaimCampaignDetectsReplayFromTheBody is the idempotency rule: a repeat
// claim also answers 200, so only `replayed:true` tells the truth
// (`qoder-credits.ts:389-396`).
func TestClaimCampaignDetectsReplayFromTheBody(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if request.Method != "POST" {
			t.Fatalf("method = %q, want POST", request.Method)
		}
		if !strings.HasSuffix(request.URL, "/01a0bf8d/claim") {
			t.Fatalf("URL = %q, want the claim endpoint", request.URL)
		}
		if len(request.Body) != 0 {
			t.Fatalf("body = %q, want an empty body (the capture showed content-length: 0)", request.Body)
		}
		return httpResponse(200, `{"grantId":"g1","status":"CLAIMED","replayed":true,"claimedAt":"2026-09-18T14:54:12Z"}`), nil
	}
	host.install(t)

	outcome := claimCampaign(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig(), "01a0bf8d")
	if outcome.Status != "already-claimed" {
		t.Fatalf("status = %q, want already-claimed (HTTP 200 with replayed:true)", outcome.Status)
	}
	if outcome.Amount != 0 {
		t.Fatalf("amount = %v, want 0 on a replay", outcome.Amount)
	}
}

// TestClaimCampaignSuccessAndFailures covers the remaining outcome branches.
func TestClaimCampaignSuccessAndFailures(t *testing.T) {
	host := newFakeHost()
	host.install(t)

	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`), nil
	}
	outcome := claimCampaign(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig(), "c1")
	if outcome.Status != "claimed" || outcome.Amount != 100 {
		t.Fatalf("outcome = %#v, want claimed +100", outcome)
	}

	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"status":"REJECTED"}`), nil
	}
	if outcome := claimCampaign(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig(), "c1"); outcome.Status != "failed" {
		t.Fatalf("outcome = %#v, want failed for a non-CLAIMED status", outcome)
	}

	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(401, "<html>unauthorized</html>"), nil
	}
	unauthorized := claimCampaign(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig(), "c1")
	if unauthorized.Status != "failed" || !strings.Contains(unauthorized.Message, "重新登录") {
		t.Fatalf("outcome = %#v, want a credential-expired message", unauthorized)
	}
}

// TestClaimDailyCheckinAggregatesEveryClaimableCampaign covers the multi-campaign
// sum (`qoder-credits.ts:456-481`).
func TestClaimDailyCheckinAggregatesEveryClaimableCampaign(t *testing.T) {
	host := newFakeHost()
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(request.URL, CampaignsPath) {
			return httpResponse(200, `{"showCampaign":true,"claimable":true,"campaigns":[
				{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}},
				{"campaignId":"c2","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":50}}]}`), nil
		}
		return httpResponse(200, `{"status":"CLAIMED","benefit":{"amount":100}}`), nil
	}
	host.install(t)

	outcome, errClaim := claimDailyCheckin(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig())
	if errClaim != nil {
		t.Fatalf("claimDailyCheckin: %v", errClaim)
	}
	if outcome.Status != "claimed" || outcome.Amount != 200 {
		t.Fatalf("outcome = %#v, want claimed with the summed 200", outcome)
	}
}

// TestClaimDailyCheckinWithNoClaimableCampaign is the conservative reading of an
// empty list: "already taken", not "no activity" (`qoder-credits.ts:250-256`).
func TestClaimDailyCheckinWithNoClaimableCampaign(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return httpResponse(200, `{"showCampaign":false,"claimable":false,"campaigns":[]}`), nil
	}
	host.install(t)
	outcome, errClaim := claimDailyCheckin(testHost(), &Credential{AccessToken: "tok"}, DefaultConfig())
	if errClaim != nil {
		t.Fatalf("claimDailyCheckin: %v", errClaim)
	}
	if outcome.Status != "already-claimed" {
		t.Fatalf("outcome = %#v, want already-claimed", outcome)
	}
}

// TestDescribeNonJSONExplainsDeadCredentials keeps the HTML-from-the-gateway case
// readable (`qoder-credits.ts:438-442`).
func TestDescribeNonJSONExplainsDeadCredentials(t *testing.T) {
	if got := describeNonJSON(401, "<html/>"); !strings.Contains(got, "重新登录") {
		t.Fatalf("message = %q, want a re-login hint", got)
	}
	if got := describeNonJSON(502, "<html>\n  bad gateway\n</html>"); !strings.Contains(got, "bad gateway") {
		t.Fatalf("message = %q, want a snippet of the body", got)
	}
}
