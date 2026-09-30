package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The hub's channel overview renders one provider row from that provider's own
// `status?format=json` document, and it reads a fixed vocabulary of field paths
// (`plugins/hub/overview.go`, the fact builders). A provider that publishes none
// of them renders as "provider 只返回了本页不展示的配置字段" — a channel that
// looks installed but inert.
//
// These tests pin the paths this provider is responsible for, so a future edit to
// the status document cannot silently blank the row.
func TestStatusJSONPublishesTheFieldsTheHubRenders(t *testing.T) {
	browseHost(t)
	response := statusJSON(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/status", Query: url.Values{"format": {"json"}},
	})

	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode status document: %v", errUnmarshal)
	}

	// `modelCountFacts` reads `model_count` and renders "模型 N".
	count, ok := document["model_count"].(float64)
	if !ok {
		t.Fatalf("model_count is missing or not a number: %#v", document["model_count"])
	}
	if int(count) != 1 {
		t.Fatalf("model_count = %v, want the one model the scripted catalogue returns", count)
	}

	// `expiryFacts` reads `expires_at_ms` (and `expired`).
	if millis, okMillis := document["expires_at_ms"].(float64); !okMillis || millis <= 0 {
		t.Fatalf("expires_at_ms is missing or non-positive: %#v", document["expires_at_ms"])
	}
	if _, okExpired := document["expired"].(bool); !okExpired {
		t.Fatalf("expired is missing or not a boolean: %#v", document["expired"])
	}
	if text, _ := document["expires_at"].(string); !strings.Contains(text, "T") {
		t.Fatalf("expires_at should be an RFC3339 string the hub can reformat, got %#v", document["expires_at"])
	}

	// `reportedAccounts` reads `account_count`, and the per-account rows carry
	// their own expiry so the overview can render one line per account.
	accounts, okAccounts := document["accounts"].([]any)
	if !okAccounts || len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one entry", document["accounts"])
	}
	first, _ := accounts[0].(map[string]any)
	if millis, okMillis := first["expires_at_ms"].(float64); !okMillis || millis <= 0 {
		t.Fatalf("accounts[0].expires_at_ms is missing: %#v", first)
	}
	if _, okExpired := first["expired"].(bool); !okExpired {
		t.Fatalf("accounts[0].expired is missing: %#v", first)
	}
}

// TestPerAccountRowsCarryTheQuotaFigures covers the account's own line in the
// hub overview. `creditFacts` renders `remaining` / `total` as "剩余 R / T", and
// `accountFigures` reads them from the ACCOUNT entry, not from the document — so
// publishing them only at the top level leaves every account line blank.
func TestPerAccountRowsCarryTheQuotaFigures(t *testing.T) {
	browseHost(t)
	response := statusJSON(testHost(), pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/atomcode/status", Query: url.Values{"format": {"json"}},
	})
	var document map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	accounts, _ := document["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", document["accounts"])
	}
	entry, _ := accounts[0].(map[string]any)
	// The scripted window is limit 200 / used 3.
	remaining, okRemaining := entry["remaining"].(float64)
	total, okTotal := entry["total"].(float64)
	if !okRemaining || !okTotal {
		t.Fatalf("account entry lacks the quota figures the hub renders: %#v", entry)
	}
	if remaining != 197 || total != 200 {
		t.Fatalf("quota figures = %v/%v, want 197/200", remaining, total)
	}
}
