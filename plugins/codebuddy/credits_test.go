package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestReadJSONHelpers(t *testing.T) {
	source := map[string]any{
		"yes":    true,
		"no":     false,
		"count":  float64(3),
		"text":   "value",
		"list":   []any{"a", "b", float64(1)},
		"status": float64(PackageStatusExpired),
	}
	if !readBool(source, "yes") || readBool(source, "no") || readBool(source, "missing") {
		t.Error("readBool")
	}
	if readNumber(source, "count") != 3 || readNumber(source, "missing") != 0 {
		t.Error("readNumber")
	}
	if readString(source, "text") != "value" || readString(source, "missing") != "" {
		t.Error("readString")
	}
	list := readStringArray(source, "list")
	if len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Errorf("readStringArray = %v, want only the strings", list)
	}
	if readStringArray(source, "missing") != nil {
		t.Error("a missing array must be nil")
	}
	if !truthy(true) || !truthy("true") || !truthy(float64(1)) || truthy(nil) || truthy("no") {
		t.Error("truthy")
	}
}

func TestReadPreciseNumber(t *testing.T) {
	source := map[string]any{
		"CapacityRemain":        float64(247),
		"CapacityRemainPrecise": "247.87",
		"Clean":                 float64(12),
		"Bad":                   float64(5),
		"BadPrecise":            "not-a-number",
	}
	if got := readPreciseNumber(source, "CapacityRemain"); got != 247.87 {
		t.Errorf("precise value = %v, want 247.87", got)
	}
	if got := readPreciseNumber(source, "Clean"); got != 12 {
		t.Errorf("integer fallback = %v", got)
	}
	if got := readPreciseNumber(source, "Bad"); got != 5 {
		t.Errorf("unparseable precise value must fall back to the integer, got %v", got)
	}
}

func TestParseCreditPackage(t *testing.T) {
	active := parseCreditPackage(map[string]any{
		"PackageName":                "CodeBuddy个人体验版",
		"CapacityUnit":               "credit",
		"Status":                     float64(0),
		"CycleCapacityRemain":        float64(155),
		"CycleCapacityRemainPrecise": "155.67",
		"CycleCapacitySize":          float64(1000),
		"CycleCapacityUsed":          float64(844.33),
		"CycleStartTime":             "2026-09-01 00:00:00",
		"CycleEndTime":               "2026-10-01 00:00:00",
	})
	if !active.Active || active.Name != "CodeBuddy个人体验版" || active.Unit != "credit" {
		t.Fatalf("active package = %+v", active)
	}
	if active.Remaining != 155.67 || active.Total != 1000 {
		t.Fatalf("precise remainder = %+v", active)
	}

	// Status 3 means expired even when the balance is non-zero.
	expired := parseCreditPackage(map[string]any{
		"PackageCode":         "Bonus",
		"Status":              float64(PackageStatusExpired),
		"CycleCapacityRemain": float64(500),
	})
	if expired.Active {
		t.Error("status 3 must mark the package inactive")
	}
	if expired.Name != "Bonus" {
		t.Errorf("name fallback chain = %q, want the PackageCode", expired.Name)
	}

	// A past ExpiredTime also marks it inactive.
	past := parseCreditPackage(map[string]any{
		"PackageName": "Old",
		"ExpiredTime": "2020-06-02 00:00:00",
	})
	if past.Active {
		t.Error("a past ExpiredTime must mark the package inactive")
	}
	// An unknown status stays active: better to show a usable balance than to
	// hide it (credits.ts:390-391).
	unknown := parseCreditPackage(map[string]any{"PackageName": "X", "Status": float64(9)})
	if !unknown.Active {
		t.Error("an unknown status must be treated as active")
	}
}

func TestRoundCredits(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{55.67000031, 55.67},
		{0.004, 0.0},
		{0.005, 0.01},
		{155.67, 155.67},
	}
	for _, test := range tests {
		if got := roundCredits(test.in); got != test.want {
			t.Errorf("roundCredits(%v) = %v, want %v", test.in, got, test.want)
		}
	}
}

func TestDescribeNonJSONResponse(t *testing.T) {
	// An expired credential produces an HTML error page; the message must point
	// at the credential rather than leaking a JSON parse error.
	got := describeNonJSONResponse(401, "<html> <h1>unauthorized</h1>")
	if got != "凭据已失效（HTTP 401），请重新登录该账号" {
		t.Errorf("401 message = %q", got)
	}
	got = describeNonJSONResponse(502, "<html>\n  <body>bad gateway</body>\n</html>")
	if got == "" || got == "<html>" {
		t.Errorf("non-JSON message = %q", got)
	}
}

func TestSupportsCheckin(t *testing.T) {
	tests := map[string]bool{
		ProductCodeBuddy:     true,
		ProductCodeBuddyIntl: false,
		ProductWorkBuddyCN:   false,
		ProductWorkBuddy:     false,
	}
	for value, want := range tests {
		product, _ := productByConfigValue(value)
		if got := supportsCheckin(product); got != want {
			t.Errorf("supportsCheckin(%s) = %v, want %v", value, got, want)
		}
	}
}

func TestCheckinHeaders(t *testing.T) {
	product, _ := productByConfigValue(ProductCodeBuddy)
	credential := &Credential{
		AccessToken:  "tok",
		UserID:       "uid",
		EnterpriseID: "ent",
		Domain:       "stale.example.com",
	}
	headers := checkinHeaders(credential, product)
	expectations := map[string]string{
		"Authorization":    "Bearer tok",
		HeaderDomain:       "copilot.tencent.com",
		HeaderProduct:      DeploymentType,
		HeaderProductCode:  "codebuddy",
		"X-User-Id":        "uid",
		HeaderEnterpriseID: "ent",
		HeaderTenantID:     "ent",
		"User-Agent":       BuddyUserAgent,
	}
	for key, want := range expectations {
		if got := headers.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
	// No user id: the header must be absent rather than empty.
	lean := checkinHeaders(&Credential{AccessToken: "t"}, product)
	if got := lean.Get("X-User-Id"); got != "" {
		t.Errorf("X-User-Id = %q, want it omitted", got)
	}
}

func TestQuotaDescribeMentionsCheckinOnlyWhenSupported(t *testing.T) {
	previous := settings()
	defer setSettings(previous)

	setSettings(Config{Product: ProductCodeBuddy})
	value, errDescribe := handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("describe: %v", errDescribe)
	}
	withCheckin, ok := value.(pluginapi.QuotaDescribeResponse)
	if !ok {
		t.Fatalf("describe returned %T", value)
	}

	setSettings(Config{Product: ProductWorkBuddy})
	value, errDescribe = handleQuotaDescribe(nil, nil)
	if errDescribe != nil {
		t.Fatalf("describe: %v", errDescribe)
	}
	withoutCheckin, ok := value.(pluginapi.QuotaDescribeResponse)
	if !ok {
		t.Fatalf("describe returned %T", value)
	}

	if withCheckin.SupportsReset || withoutCheckin.SupportsReset {
		t.Error("CodeBuddy has no quota reset")
	}
	if len(withoutCheckin.SupportedProviders) != 1 || withoutCheckin.SupportedProviders[0] != ProviderKey {
		t.Errorf("supported providers = %v", withoutCheckin.SupportedProviders)
	}
	if withCheckin.DisplayName == withoutCheckin.DisplayName {
		t.Errorf("the display name should mention check-in only where it exists: %q", withCheckin.DisplayName)
	}
}

func TestQuotaResetIsUnsupported(t *testing.T) {
	value, errReset := handleQuotaReset(nil, nil)
	if errReset != nil {
		t.Fatalf("reset: %v", errReset)
	}
	response, ok := value.(pluginapi.QuotaResetResponse)
	if !ok {
		t.Fatalf("reset returned %T", value)
	}
	if response.Success {
		t.Error("reset must report failure")
	}
}
