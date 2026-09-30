package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The claim cascade is the AtomCode equivalent of a daily check-in, and its
// stopping rules are what keep a lower tier from being claimed after a higher one
// already succeeded. These tests pin the reference's rules
// (crates/atomcode-codingplan/src/setup.rs:825-922).

// claimScript answers claim-v2 from a per-tier script and records the order the
// tiers were asked for, so stopping behaviour is observable.
func claimScript(t *testing.T, answers map[string]string) (*fakeHost, *[]string) {
	t.Helper()
	host := newFakeHost()
	order := &[]string{}
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, codingPlanClaimPath):
			var body struct {
				PlanType string `json:"plan_type"`
			}
			if errUnmarshal := json.Unmarshal(request.Body, &body); errUnmarshal != nil {
				t.Fatalf("decode claim body: %v", errUnmarshal)
			}
			*order = append(*order, body.PlanType)
			answer, ok := answers[body.PlanType]
			if !ok {
				t.Fatalf("unexpected tier %s", body.PlanType)
			}
			return httpResponse(http.StatusOK, answer), nil
		case strings.Contains(request.URL, codingPlanStatusPath):
			return httpResponse(http.StatusOK, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","expires_at":"2026-10-08"}}`), nil
		}
		t.Fatalf("unexpected URL %s", request.URL)
		return nil, nil
	}
	host.install(t)
	return host, order
}

func TestClaimCascadeStopsAtTheFirstGrantedTier(t *testing.T) {
	_, order := claimScript(t, map[string]string{
		planTypeMax:  `{"success":true,"duplicate":false,"message":"领取成功","plan_type":"Lite-beta","plan_name":"CodingPlan Lite-体验版"}`,
		planTypePro:  `{"success":true,"duplicate":false,"message":"never reached"}`,
		planTypeLite: `{"success":true,"duplicate":false,"message":"never reached"}`,
	})
	outcome := claimCascade(testHost(), settings(), sampleCredential(3600))
	if outcome.Status != claimClaimed {
		t.Fatalf("status = %q, want %q (%s)", outcome.Status, claimClaimed, outcome.Message)
	}
	if len(*order) != 1 || (*order)[0] != planTypeMax {
		t.Fatalf("tiers tried = %v, want only Max", *order)
	}
	if outcome.PlanName != "CodingPlan Lite-体验版" {
		t.Fatalf("plan name = %q", outcome.PlanName)
	}
}

// TestClaimCascadeStopsOnDuplicateTier is the rule that protects an account from
// being downgraded: `duplicate:true` means "you already hold this tier or a
// higher one", so asking for Pro next would either duplicate again or actively
// move the account down.
func TestClaimCascadeStopsOnDuplicateTier(t *testing.T) {
	_, order := claimScript(t, map[string]string{
		planTypeMax:  `{"success":false,"duplicate":true,"message":"已领取"}`,
		planTypePro:  `{"success":true,"duplicate":false,"message":"never reached"}`,
		planTypeLite: `{"success":true,"duplicate":false,"message":"never reached"}`,
	})
	outcome := claimCascade(testHost(), settings(), sampleCredential(3600))
	if outcome.Status != claimAlreadyHeld {
		t.Fatalf("status = %q, want %q", outcome.Status, claimAlreadyHeld)
	}
	if len(*order) != 1 {
		t.Fatalf("tiers tried = %v, want the cascade to stop at Max", *order)
	}
}

// TestClaimCascadeWalksDownOnRefusal covers the one case that must continue: a
// per-tier refusal is not a transport failure, so the next tier down is tried.
func TestClaimCascadeWalksDownOnRefusal(t *testing.T) {
	_, order := claimScript(t, map[string]string{
		planTypeMax:  `{"success":false,"duplicate":false,"message":"全平台日限额已满"}`,
		planTypePro:  `{"success":false,"duplicate":false,"message":"暂无开放"}`,
		planTypeLite: `{"success":true,"duplicate":false,"message":"领取成功"}`,
	})
	outcome := claimCascade(testHost(), settings(), sampleCredential(3600))
	if outcome.Status != claimClaimed {
		t.Fatalf("status = %q, want %q (%s)", outcome.Status, claimClaimed, outcome.Message)
	}
	if len(*order) != 3 {
		t.Fatalf("tiers tried = %v, want all three", *order)
	}
	if len(outcome.Attempts) != 3 {
		t.Fatalf("attempts = %d, want a full trail", len(outcome.Attempts))
	}
}

// TestClaimCascadeAbortsOnTransportFailure pins the other stopping rule: a
// broken connection cannot be fixed by asking a lower tier, and retrying would
// only add load.
func TestClaimCascadeAbortsOnTransportFailure(t *testing.T) {
	host := newFakeHost()
	host.do = func(abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		return nil, errFakeTransport
	}
	host.install(t)
	outcome := claimCascade(testHost(), settings(), sampleCredential(3600))
	if outcome.Status != claimFailed {
		t.Fatalf("status = %q, want %q", outcome.Status, claimFailed)
	}
	if len(outcome.Attempts) != 1 {
		t.Fatalf("attempts = %d, want the cascade to abort after the first failure", len(outcome.Attempts))
	}
}

// TestPlanTierMapsTheServerPlanName covers the mapping that decides which tier
// `models-v2` is queried with. Asking for a higher tier than the account holds
// marks models available that answer 403 on every request.
func TestPlanTierMapsTheServerPlanName(t *testing.T) {
	cases := map[string]string{
		"CodingPlan Lite-体验版": planTypeLite,
		"CodingPlan Pro":      planTypePro,
		"CodingPlan Max":      planTypeMax,
		"CodingPlan Free":     "",
		"":                    "",
	}
	for name, want := range cases {
		status := &statusResponse{CodingPlanFree: &planInfo{PlanName: name}}
		if got := status.planTier(); got != want {
			t.Fatalf("planTier(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestStatusActiveFollowsTheExpiryNotTheStatusEnum pins the authoritative rule.
// `PlanInfo.status` and `audit_status` are parsed by the reference and consumed
// nowhere, so this adapter must not invent meaning for them.
func TestStatusActiveFollowsTheExpiryNotTheStatusEnum(t *testing.T) {
	claimed := &statusResponse{CodingPlanFree: &planInfo{PlanName: "CodingPlan Lite", Status: 1, ExpiresAt: "2026-10-08"}}
	if !claimed.active() {
		t.Fatal("a plan with an expiry should be active")
	}
	pending := &statusResponse{CodingPlanFree: &planInfo{PlanName: "CodingPlan Lite", Status: 1}}
	if pending.active() {
		t.Fatal("a plan with no expiry is pending activation, not active")
	}
	// The pre-claim shape observed live: status -1, claimed_at null.
	preClaim := &statusResponse{CodingPlanFree: &planInfo{PlanName: "CodingPlan Lite", Status: -1}}
	if preClaim.active() {
		t.Fatal("an unclaimed plan must not report as active")
	}
}

// TestQuotaWindowsRespectShowEnable covers the display filter.
func TestQuotaWindowsRespectShowEnable(t *testing.T) {
	status := &statusResponse{RateLimitWindows: []rateLimitWindow{
		{RuleIndex: 0, ShowEnable: 1, CallLimit: 200, WindowHours: 5},
		{RuleIndex: 1, ShowEnable: 0, CallLimit: 9999},
	}}
	shown := status.quotaWindows()
	if len(shown) != 1 || shown[0].CallLimit != 200 {
		t.Fatalf("quotaWindows = %#v, want only the show_enable==1 window", shown)
	}
	if got := quotaSummary(shown[0]); !strings.Contains(got, "0/200") {
		t.Fatalf("quotaSummary = %q, want the call count", got)
	}
}

// TestCodingPlanErrorFormatting pins the three shapes the reference renders.
func TestCodingPlanErrorFormatting(t *testing.T) {
	if got := classifyCodingPlanError("claim", http.StatusOK, `{"message":"全平台日限额已满"}`); got != "全平台日限额已满" {
		t.Fatalf("product payload: %q", got)
	}
	if got := classifyCodingPlanError("models", http.StatusNotFound,
		`{"timestamp":"t","status":404,"path":"/api/v5/coding-plan/models"}`); !strings.Contains(got, "/api/v5/coding-plan/models") {
		t.Fatalf("spring payload: %q", got)
	}
	if got := classifyCodingPlanError("status", http.StatusBadGateway, "plain text"); !strings.Contains(got, "plain text") {
		t.Fatalf("raw fallback: %q", got)
	}
}

// TestCheckinJSONOnlyClaimsOnRequest is the guard against claiming on the user's
// behalf from a page load.
//
// The resource route is dispatched as GET and the manager renders it, so without
// this guard a bare `?format=json` poll would run the cascade every time it was
// read. The HTML page already required `action=claim`; the JSON path must agree
// with it, and the hub catalogue carries the parameter for exactly this reason.
func TestCheckinJSONOnlyClaimsOnRequest(t *testing.T) {
	host := newFakeHost()
	host.auths["idx-1"] = mustJSON(t, sampleCredential(7*24*3600))
	host.files = []pluginapi.HostAuthFileEntry{
		{AuthIndex: "idx-1", Name: "atomcode-qq_23240873.json", Provider: ProviderKey, Type: ProviderKey},
	}
	claimSeen := false
	host.do = func(request abiboot.HTTPDoRequest) (*pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(request.URL, codingPlanClaimPath):
			claimSeen = true
			return httpResponse(200, `{"success":true,"duplicate":false,"message":"领取成功"}`), nil
		case strings.Contains(request.URL, codingPlanStatusPath):
			return httpResponse(200, `{"codingplan_free":{"plan_name":"CodingPlan Lite-体验版","expires_at":"2026-10-08"}}`), nil
		}
		return httpResponse(404, `{"message":"unexpected"}`), nil
	}
	host.install(t)

	report := checkinJSON(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/checkin", Query: url.Values{"format": {"json"}},
	})
	if claimSeen {
		t.Fatal("a bare ?format=json load ran the claim cascade")
	}
	if !strings.Contains(string(report.Body), "needs-action") {
		t.Fatalf("report-only answer = %s", string(report.Body))
	}
	if !strings.Contains(string(report.Body), "CodingPlan Lite-体验版") {
		t.Fatalf("the report should still carry the plan state: %s", string(report.Body))
	}

	claimed := checkinJSON(testHost(), pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/atomcode/checkin",
		Query: url.Values{"format": {"json"}, "action": {"claim"}},
	})
	if !claimSeen {
		t.Fatal("action=claim did not run the cascade")
	}
	if !strings.Contains(string(claimed.Body), "claimed") {
		t.Fatalf("claim answer = %s", string(claimed.Body))
	}
}
