package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Balance, activity reporting and the daily claim.
//
// ## Why the activation events are not optional
//
// The server does NOT push entitlements. `billing/preview` answers
// `{"code":0,"data":{"plans":[]}}` until the client has reported activity; after
// `POST /api/v1/event/report` with `app_launch` and `app_daily_active` the very
// same call lists a plan:
//
//	before: {"code":0,"data":{"plans":[]}}
//	after:  {"code":0,"data":{"plans":[{"plan_id":"zcode-v3-start-plan-trust-…"}]}}
//
// ⇒ "daily random distribution" is not a push at all: the server decides whether
// to grant anything from the activity signal. Querying the preview without
// reporting first reports "nothing to claim" every single day, and the user
// concludes the check-in is broken.
//
// ## Why the balance is rendered in TOKENS
//
// The balance buckets publish `unit_type` (measured `"token"`) and `meter`
// (measured `"model_usage"`) alongside `total_units` / `used_units` /
// `remaining_units`. A real defect in the reference came from rendering
// `remaining_units` as a generic "credits" number: the UI showed `94539275`,
// which reads like 94 million credits rather than 94.54M tokens. Both fields are
// therefore carried through and the unit is rendered explicitly.

// balanceBucket is one entry of `data.balances`.
type balanceBucket struct {
	PlanID   string
	ShowName string
	// UnitType is upstream's `unit_type` (measured `"token"`).
	UnitType string
	// Meter is upstream's `meter` (measured `"model_usage"`).
	Meter     string
	Total     int64
	Used      int64
	Remaining int64
	Available int64
	ExpiresAt int64
}

// balanceResult is the parsed balance.
type balanceResult struct {
	// Enterprise marks the `displayMode: "enterprise"` shape, which publishes no
	// numbers at all. Rendering that as 0 would read as "used up" instead of
	// "not reported".
	Enterprise bool
	Buckets    []balanceBucket
	Remaining  int64
	Total      int64
	// ExpiresAt is the earliest bucket expiry (Unix seconds).
	ExpiresAt int64
	// PlanName is the first bucket's display name.
	PlanName string
}

func fetchBalance(h *abiboot.Host, credential *Credential, cfg Config) (*balanceResult, error) {
	headers := buildHeaders(credential, cfg, headerOptions{Authorization: true, Accept: "application/json"})
	response, errDo := hostRequest(h, http.MethodGet, Origin+BalancePath, headers, nil)
	if errDo != nil {
		return nil, transportError("balance_transport", "查询额度失败：%v", errDo)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	envelope := parseUpstreamEnvelope(response.Body, response.StatusCode)
	if !envelope.success() {
		return nil, classifyUpstreamError(response.StatusCode, response.Body)
	}
	data, errData := envelopeData(envelope, response.Body)
	if errData != nil {
		return nil, errData
	}
	return parseBalance(data), nil
}

// parseBalance reads the balance body.
func parseBalance(data map[string]any) *balanceResult {
	result := &balanceResult{}
	if strings.EqualFold(stringField(data, "displayMode"), "enterprise") {
		return &balanceResult{Enterprise: true}
	}
	rawBuckets, _ := data["balances"].([]any)
	for _, item := range rawBuckets {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bucket := balanceBucket{
			PlanID:   stringField(record, "plan_id"),
			ShowName: stringField(record, "show_name"),
			// ⚠ the unit fields MUST be carried: upstream says `unit_type: "token"`
			// and the page renders the M-magnitude from it.
			UnitType: stringField(record, "unit_type"),
			Meter:    stringField(record, "meter"),
		}
		bucket.Total = nonNegativeInt64(record["total_units"])
		bucket.Used = nonNegativeInt64(record["used_units"])
		bucket.Remaining = nonNegativeInt64(record["remaining_units"])
		bucket.Available = nonNegativeInt64(record["available_units"])
		bucket.ExpiresAt = nonNegativeInt64(record["expires_at"])
		result.Buckets = append(result.Buckets, bucket)
	}
	for _, bucket := range result.Buckets {
		// Prefer `available_units` when the server publishes it, else `remaining_units`.
		if bucket.Available > 0 {
			result.Remaining += bucket.Available
		} else {
			result.Remaining += bucket.Remaining
		}
		result.Total += bucket.Total
		if bucket.ExpiresAt > 0 && (result.ExpiresAt == 0 || bucket.ExpiresAt < result.ExpiresAt) {
			result.ExpiresAt = bucket.ExpiresAt
		}
	}
	if len(result.Buckets) > 0 {
		result.PlanName = result.Buckets[0].ShowName
	}
	return result
}

func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the provider and the reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        DisplayName,
		// The daily allowance resets on the server's own schedule; there is no
		// reset endpoint to call.
		SupportsReset: false,
	}, nil
}

// handleQuotaFetch reports the current balance as normalized quota.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := credentialForRequest(h, request.AuthIndex, request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	cfg := settings()
	balance, errBalance := fetchBalance(h, credential, cfg)
	if errBalance != nil {
		return nil, errBalance
	}

	response := pluginapi.QuotaFetchResponse{}
	if balance.Enterprise {
		response.Summary = []pluginapi.QuotaMetric{{
			Key:   "balance",
			Label: "额度",
			// Enterprise accounts publish no numbers; saying 0 would read as
			// "used up", so the metric reports the shape instead.
			Value:  0,
			Unit:   "企业版",
			Format: "text",
		}}
		return response, nil
	}

	// ⚠ The unit is rendered explicitly. Upstream's `unit_type` is `token`, and
	// the M-magnitude is what makes the number readable: `94.54M tokens` rather
	// than a bare `94539275` that looks like 94 million credits.
	unit := "tokens"
	if len(balance.Buckets) > 0 && balance.Buckets[0].UnitType != "" {
		unit = balance.Buckets[0].UnitType + "s"
	}
	response.Summary = append(response.Summary,
		pluginapi.QuotaMetric{
			Key:    "remaining",
			Label:  "剩余额度",
			Value:  float64(balance.Remaining),
			Unit:   formatTokenMagnitude(balance.Remaining) + " " + unit,
			Format: "number",
		},
		pluginapi.QuotaMetric{
			Key:    "total",
			Label:  "总额度",
			Value:  float64(balance.Total),
			Unit:   formatTokenMagnitude(balance.Total) + " " + unit,
			Format: "number",
		},
	)

	if balance.PlanName != "" || balance.ExpiresAt > 0 {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: balance.PlanName}
	}
	// One bucket per model, so the management UI can show where the quota went.
	for _, bucket := range balance.Buckets {
		label := bucket.ShowName
		if label == "" {
			label = bucket.PlanID
		}
		fraction := 0.0
		switch {
		case bucket.Total > 0:
			remaining := bucket.Remaining
			if bucket.Available > 0 {
				remaining = bucket.Available
			}
			fraction = float64(remaining) / float64(bucket.Total)
		case bucket.Remaining > 0:
			fraction = 1
		}
		bucketLabel := label
		if bucket.UnitType != "" {
			bucketLabel += "（" + bucket.UnitType + "）"
		}
		response.Groups = append(response.Groups, pluginapi.QuotaGroup{
			DisplayName: "ZCode 额度",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            bucketLabel,
				RemainingFraction: clampFraction(fraction),
				ResetTime:         unixSecondsToRFC3339(bucket.ExpiresAt),
				Description: "已用 " + formatTokenMagnitude(bucket.Used) + " / " +
					formatTokenMagnitude(bucket.Total) + " " + unit,
			}},
		})
	}
	return response, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "ZCode 的每日额度由服务端按自然日重置，没有重置接口",
	}, nil
}

// credentialForRequest resolves the credential a quota call acts on.
//
// The host may hand the storage inline or only an auth index; both spellings are
// accepted so the same handler serves the management page and the host.
func credentialForRequest(h *abiboot.Host, authIndex string, storage []byte) (*Credential, error) {
	if len(strings.TrimSpace(string(storage))) > 0 {
		return ParseCredential(storage)
	}
	if h == nil || strings.TrimSpace(authIndex) == "" {
		return nil, credentialError("missing_credential", "没有可用的 ZCode 凭据：请先登录一个账号")
	}
	auth, errGet := h.GetAuth(authIndex)
	if errGet != nil {
		return nil, errGet
	}
	return ParseCredential(auth.JSON)
}

// clampFraction keeps a fraction inside [0,1]; a negative remaining balance
// happens after a metering rollback and would otherwise render as a negative bar.
func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// formatTokenMagnitude renders a token count in the M/B magnitude the reference
// settled on (`94.54M`), which is what makes a nine-digit number readable.
func formatTokenMagnitude(value int64) string {
	switch {
	case value >= 1_000_000_000:
		return trimTrailingZeros(fmt.Sprintf("%.2f", float64(value)/1_000_000_000)) + "B"
	case value >= 1_000_000:
		return trimTrailingZeros(fmt.Sprintf("%.2f", float64(value)/1_000_000)) + "M"
	case value >= 1_000:
		return trimTrailingZeros(fmt.Sprintf("%.1f", float64(value)/1_000)) + "K"
	default:
		return strconv.FormatInt(value, 10)
	}
}

// trimTrailingZeros removes a trailing `.00` / `.50` tail so `94.50M` reads as
// `94.5M` and `100.00M` as `100M`.
func trimTrailingZeros(value string) string {
	if !strings.Contains(value, ".") {
		return value
	}
	value = strings.TrimRight(value, "0")
	return strings.TrimRight(value, ".")
}
