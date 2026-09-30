package main

import (
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host's quota surface, fed from `status-v2`.
//
// The plan's real quota is a CALL COUNT per rolling window, not a token budget:
// the free tier measured 2026-10-01 allows 200 calls per 5 hours, which the
// server publishes through `rate_limit_windows`. Reporting a token metric here
// would show a number the account is not actually limited by.

// handleQuotaIdentifier advertises the provider key this quota source serves.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and that reset is not
// available: the window resets on the server's own clock and nothing a client
// sends can bring it forward.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "AtomCode 套餐额度",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the plan state and the rolling call window.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	// The host asks for a quota read before any page is opened, so the credential
	// is renewed here as well.
	fresh, errFresh := ensureCredentialFresh(h, authrefresh.Request{
		Name:        request.AuthID,
		StorageJSON: request.StorageJSON,
		Attributes:  request.Attributes,
	})
	if errFresh != nil {
		return nil, errFresh
	}
	credential, errCredential := ParseCredential(fresh.Storage)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	status, errStatus := fetchStatus(h, cfg, credential)
	if errStatus != nil {
		return nil, errStatus
	}

	summary := make([]pluginapi.QuotaMetric, 0, 6)
	active := 0.0
	if status.active() {
		active = 1
	}
	summary = append(summary,
		pluginapi.QuotaMetric{Key: "plan_active", Label: "套餐生效", Value: active, Format: "boolean"},
	)
	if status.CodingPlanFree != nil {
		remaining := 0.0
		if status.CodingPlanFree.Remaining != nil {
			remaining = float64(*status.CodingPlanFree.Remaining)
		}
		summary = append(summary,
			pluginapi.QuotaMetric{Key: "plan_remaining_days", Label: "套餐剩余天数", Value: remaining, Unit: "天", Format: "number"},
		)
	}
	for _, window := range status.quotaWindows() {
		summary = append(summary,
			pluginapi.QuotaMetric{
				Key:    "window_calls_used",
				Label:  "本窗口已用调用",
				Value:  float64(window.CallsUsed),
				Unit:   "次 / " + itoa64(window.CallLimit),
				Format: "number",
			},
			pluginapi.QuotaMetric{
				Key:    "window_calls_limit",
				Label:  "本窗口调用上限",
				Value:  float64(window.CallLimit),
				Unit:   "次",
				Format: "number",
			},
		)
		break
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "AtomCode 额度按服务端窗口重置，不支持手动重置"}, nil
}
