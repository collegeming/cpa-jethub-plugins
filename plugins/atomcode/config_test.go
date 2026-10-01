package main

import (
	"strings"
	"testing"
	"time"
)

// TestAdapterIdentityIsNotTheOfficialOne guards the constant itself, so the trap
// is documented by a failing test rather than only by a comment. The gateway
// demands the closed-source request signature for any `atomcode/<version>`
// caller; presenting that identity would take the whole channel down with a
// `403 ATOMCODE_SIG_MISSING` that looks like a credential fault.
func TestAdapterIdentityIsNotTheOfficialOne(t *testing.T) {
	if strings.HasPrefix(strings.ToLower(AdapterUserAgent), "atomcode/") {
		t.Fatalf("AdapterUserAgent = %q must not claim to be the official client", AdapterUserAgent)
	}
	if !strings.HasPrefix(OfficialUserAgent, "atomcode/") {
		t.Fatalf("OfficialUserAgent = %q should record what the vendor client sends", OfficialUserAgent)
	}
	if AdapterUserAgent == OfficialUserAgent {
		t.Fatal("the adapter identity and the vendor identity must not be the same string")
	}
}

// TestDefaultGatewayIsNotTheSignedHost pins the endpoint choice: the reference's
// own default requires the closed-source signature.
func TestDefaultGatewayIsNotTheSignedHost(t *testing.T) {
	cfg := DefaultConfig()
	if strings.Contains(cfg.GatewayBase, "llm-api.atomgit.com") {
		t.Fatalf("GatewayBase = %q, which requires the closed-source signature", cfg.GatewayBase)
	}
	if cfg.GatewayBase != DefaultGatewayBase {
		t.Fatalf("GatewayBase = %q, want %q", cfg.GatewayBase, DefaultGatewayBase)
	}
	if got := cfg.gatewayChatURL(); got != DefaultGatewayBase+"/chat/completions" {
		t.Fatalf("gatewayChatURL = %q", got)
	}
}

// TestConfigFromYAML covers the coercion rules the sibling plugins document:
// block and flow style, numbers arriving as strings, and one bad key not
// discarding the rest.
func TestConfigFromYAML(t *testing.T) {
	cfg := ConfigFromYAML([]byte(`
gateway_base: "https://example.test/v1/"
plan_type: lite
discover_models: "no"
model_prefix: 0
login_timeout_ms: "120000"
request_timeout_ms: 5000
`))
	if cfg.GatewayBase != "https://example.test/v1" {
		t.Fatalf("trailing slash not trimmed: %q", cfg.GatewayBase)
	}
	if cfg.PlanType != planTypeLite {
		t.Fatalf("PlanType = %q, want %q", cfg.PlanType, planTypeLite)
	}
	if cfg.DiscoverModels {
		t.Fatal("discover_models: \"no\" should disable discovery")
	}
	if cfg.ModelPrefix {
		t.Fatal("model_prefix: 0 should disable the prefix")
	}
	if cfg.LoginTimeoutMS != 120000 || cfg.RequestTimeoutMS != 5000 {
		t.Fatalf("timeouts = %d/%d", cfg.LoginTimeoutMS, cfg.RequestTimeoutMS)
	}
	// Keys that were not mentioned keep their defaults.
	if cfg.CatalogueTimeoutMS != CatalogueTimeoutMS {
		t.Fatalf("unmentioned key lost its default: %d", cfg.CatalogueTimeoutMS)
	}
}

// TestCoercePlanTypeRejectsUnknownTiers pins the guard that stops a typo from
// being forwarded: `models-v2` computes `plan_available` relative to the
// requested tier, so asking for a higher tier than the account holds marks
// models available that answer 403 on every request.
func TestCoercePlanTypeRejectsUnknownTiers(t *testing.T) {
	if got := coercePlanType("banana", PlanTypeAuto); got != PlanTypeAuto {
		t.Fatalf("unknown tier produced %q, want the auto default", got)
	}
	if got := coercePlanType("MAX", PlanTypeAuto); got != planTypeMax {
		t.Fatalf("case-insensitive match failed: %q", got)
	}
	if got := coercePlanType(nil, PlanTypeAuto); got != PlanTypeAuto {
		t.Fatalf("absent tier produced %q", got)
	}
}

// TestCredentialExpiryComesFromTheIssuanceRecord pins the only expiry source
// available: an AtomGit access token is opaque, so there is no `exp` claim to
// fall back to.
func TestCredentialExpiryComesFromTheIssuanceRecord(t *testing.T) {
	now := time.Now().Unix()
	credential := sampleCredential(7200)
	credential.CreatedAt = now
	millis, ok := credential.ExpiresAtMS()
	if !ok {
		t.Fatal("a complete record must yield an expiry")
	}
	if want := (now + 7200) * 1000; millis != want {
		t.Fatalf("expiry = %d, want %d", millis, want)
	}
	if credential.Expired() {
		t.Fatal("a token two hours out must not read as expired")
	}

	// An incomplete record must NOT be treated as expired: reporting 1970 would
	// start a refresh loop against a token that may well be valid.
	incomplete := &Credential{AccessToken: "t"}
	if _, ok := incomplete.ExpiresAtMS(); ok {
		t.Fatal("a record with no expires_in must not invent an expiry")
	}
	if incomplete.Expired() {
		t.Fatal("a credential with an unknown expiry must not read as expired")
	}
}

// TestApplyRefreshRotatesAndPreserves covers the rotation contract. A successful
// refresh replaces BOTH tokens and invalidates the old access token immediately,
// so dropping the new refresh token costs the user a full re-login.
func TestApplyRefreshRotatesAndPreserves(t *testing.T) {
	existing := sampleCredential(3600)
	existing.CreatedAt = 1_700_000_000
	refreshed := applyRefresh(existing, brokerTokenResponse{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		TokenType:    "Bearer",
		ExpiresIn:    604800,
	}, time.Unix(1_800_000_000, 0))

	if refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" {
		t.Fatalf("rotation did not take: %#v", refreshed)
	}
	if refreshed.CreatedAt != 1_800_000_000 {
		t.Fatalf("created_at = %d, want the refresh instant", refreshed.CreatedAt)
	}
	if refreshed.User.ID != existing.User.ID {
		t.Fatal("the account identity must survive a refresh")
	}

	// A stripped-down reply keeps the previous values rather than blanking them.
	sparse := applyRefresh(existing, brokerTokenResponse{AccessToken: "only-access"}, time.Unix(1_800_000_000, 0))
	if sparse.RefreshToken != existing.RefreshToken {
		t.Fatalf("an omitted refresh_token blanked the stored one: %q", sparse.RefreshToken)
	}
	if sparse.ExpiresIn != existing.ExpiresIn {
		t.Fatalf("an omitted expires_in changed the lifetime: %d", sparse.ExpiresIn)
	}
	if sparse.User.ID != existing.User.ID {
		t.Fatal("an omitted user member lost the account identity")
	}
}

// TestCredentialFileNameLadder pins the naming, which is what keeps one account
// in one file. The AtomGit display name is frequently Chinese and sanitises to
// nothing, so the login handle is the value that actually carries identity.
func TestCredentialFileNameLadder(t *testing.T) {
	credential := sampleCredential(3600)
	if got := defaultAuthFileName(credential); got != "atomcode-qq_23240873.json" {
		t.Fatalf("file name = %q, want the ASCII login handle", got)
	}
	credential.User.Username = ""
	if got := defaultAuthFileName(credential); got != "atomcode-6746ebd581efe24face84197.json" {
		t.Fatalf("file name = %q, want the account id when the handle is gone", got)
	}
	credential.User.ID = ""
	if got := defaultAuthFileName(credential); got != "atomcode-account.json" {
		t.Fatalf("file name = %q, want the constant fallback", got)
	}
}

// TestCredentialLabelPrefersTheDisplayName covers the panel label.
func TestCredentialLabelPrefersTheDisplayName(t *testing.T) {
	credential := sampleCredential(3600)
	if got := credential.Label(); got != "黎明文铮" {
		t.Fatalf("Label = %q", got)
	}
	credential.User.Name = ""
	if got := credential.Label(); got != "qq_23240873" {
		t.Fatalf("Label = %q", got)
	}
}

// TestParseCredentialRejectsEmptyTokens covers the validation that decides
// whether an auth file belongs to this provider at all.
func TestParseCredentialRejectsEmptyTokens(t *testing.T) {
	if _, errParse := ParseCredential(nil); errParse == nil {
		t.Fatal("an empty payload must be rejected")
	}
	if _, errParse := ParseCredential([]byte(`{"refresh_token":"x"}`)); errParse == nil {
		t.Fatal("a payload without access_token must be rejected")
	}
	credential, errParse := ParseCredential([]byte(`{"access_token":"a","user":{"id":"u"}}`))
	if errParse != nil {
		t.Fatalf("valid payload rejected: %v", errParse)
	}
	if credential.Refreshable() {
		t.Fatal("a credential with no refresh_token must not report as refreshable")
	}
}

// TestExtraModelsCoercion covers the three spellings a user reaches for. A
// single string is the first thing anyone writes, and silently ignoring it
// would look like the setting does not work at all.
func TestExtraModelsCoercion(t *testing.T) {
	if got := ConfigFromYAML([]byte("extra_models: deepseek-flash")).ExtraModels; len(got) != 1 || got[0] != "deepseek-flash" {
		t.Fatalf("scalar spelling = %#v", got)
	}
	if got := ConfigFromYAML([]byte("extra_models: a, b ,,c")).ExtraModels; len(got) != 3 || got[2] != "c" {
		t.Fatalf("comma spelling = %#v", got)
	}
	if got := ConfigFromYAML([]byte("extra_models:\n  - a\n  - b\n")).ExtraModels; len(got) != 2 || got[0] != "a" {
		t.Fatalf("list spelling = %#v", got)
	}
	if got := DefaultConfig().ExtraModels; len(got) != 0 {
		t.Fatalf("the default must be empty: offering an unadvertised model is the operator's call, got %#v", got)
	}
}
