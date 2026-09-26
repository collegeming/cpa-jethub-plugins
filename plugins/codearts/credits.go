package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// claimedStatuses are the activity states that mean the daily benefit has
// already been taken today.
var claimedStatuses = map[string]bool{
	"CLAIMED":   true,
	"CONFIRMED": true,
	"CONSUMED":  true,
}

// creditMetricNames are the balance fields reported by the package endpoint.
var creditMetricNames = []string{
	"usageTotalPackageCredit",
	"usageBasicPackageCredit",
	"usageOnDemandPackageCredit",
	"usageBonusPackageCredit",
}

// snapRequest performs a signed Snap-Access call and attaches the unsigned
// agent headers the gateway expects.
func snapRequest(h *abiboot.Host, credential *Credential, method, path string, body []byte, query url.Values) (*pluginapi.HTTPResponse, error) {
	rawURL := SnapEngineBase + path
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return nil, abiboot.Errorf("invalid_url", "parse %s: %v", rawURL, errParse)
	}
	wire := &http.Request{Method: method, URL: parsed, Header: http.Header{}}
	SignRequest(wire, body, credential.AccessKeyID, credential.SecretAccessKey, credential.SecurityToken, time.Now(), nil)
	for key, value := range UnsignedHeaders() {
		wire.Header[key] = []string{value}
	}
	return h.HTTPDo(abiboot.HTTPDoRequest{Method: method, URL: rawURL, Headers: wire.Header, Body: body})
}

// creditBalance summarises the account's credit state.
type creditBalance struct {
	IsCreditPackage bool
	Remaining       float64
	Used            float64
	Total           float64
}

// dailyActivity is the USER_LOGIN campaign entry.
type dailyActivity struct {
	CampaignID    string
	Claimable     bool
	Status        string
	BenefitAmount float64
}

// checkinOutcome is the result of one daily check-in attempt.
type checkinOutcome struct {
	Status  string
	Message string
	Amount  float64
	Balance *creditBalance
}

// accountQuota is ONE account's own quota and check-in state.
//
// Every quota field is a pointer: it is nil when the provider did not return it,
// with the matching error set instead. A failed balance read must render as
// 未知, never as 0 — 0 would tell the user the credits are spent.
type accountQuota struct {
	// Entry is the host credential this quota belongs to.
	Entry pluginapi.HostAuthFileEntry
	// Credential is nil when the credential itself could not be read;
	// CredentialErr then explains why and no upstream call was made for it.
	Credential    *Credential
	CredentialErr error
	// Balance is the account's own credit state, or nil with BalanceErr set.
	Balance    *creditBalance
	BalanceErr error
	// Activity is the account's own daily-login campaign, or nil. ActivityErr
	// carries a failed read and a nil Activity with a nil error means the
	// provider listed no daily-login campaign for this account.
	Activity    *dailyActivity
	ActivityErr error
}

// collectAccountQuotas reads the quota of every account, in host order, one
// account at a time.
//
// Sequential on purpose: each account costs two upstream requests, and the
// reference panel queries balances one account at a time for the same reason
// (parallel balance reads are what the provider's risk control reacts to). One
// account's failure never stops the sweep and never hides another's figures.
func collectAccountQuotas(h *abiboot.Host, accounts []pluginapi.HostAuthFileEntry) []accountQuota {
	out := make([]accountQuota, 0, len(accounts))
	for _, entry := range accounts {
		quota := accountQuota{Entry: entry}
		credential, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			quota.CredentialErr = errCredential
			out = append(out, quota)
			continue
		}
		quota.Credential = credential
		balance, errBalance := fetchCreditBalance(h, credential)
		if errBalance != nil {
			quota.BalanceErr = errBalance
			out = append(out, quota)
			continue
		}
		quota.Balance = balance
		activity, errActivity := fetchDailyActivity(h, credential)
		if errActivity != nil {
			quota.ActivityErr = errActivity
		} else {
			quota.Activity = activity
		}
		out = append(out, quota)
	}
	return out
}

// quotaOf picks one account's quota out of a collected list.
func quotaOf(quotas []accountQuota, entry pluginapi.HostAuthFileEntry) (accountQuota, bool) {
	for _, quota := range quotas {
		if quota.Entry.AuthIndex == entry.AuthIndex && quota.Entry.Name == entry.Name {
			return quota, true
		}
	}
	return accountQuota{}, false
}

// quotaJSON renders one account's own figures.
//
// The keys are exactly the ones the selected account already publishes at the
// top level (credit_package / remaining / used / total / daily_checkin) so a
// consumer that reads the flat document can read one entry of `accounts` with
// the same code. A failed read emits the reason under `credit_error` (balance) or
// `activity_error` (check-in campaign) and NO number at all.
func quotaJSON(quota accountQuota) map[string]any {
	item := map[string]any{
		"auth_index": quota.Entry.AuthIndex,
		"name":       quota.Entry.Name,
		"status":     statusText(quota.Entry),
	}
	if label := strings.TrimSpace(quota.Entry.Label); label != "" {
		item["label"] = label
	}
	if quota.CredentialErr != nil {
		item["error"] = quota.CredentialErr.Error()
		return item
	}
	if quota.Balance != nil {
		item["credit_package"] = quota.Balance.IsCreditPackage
		item["remaining"] = quota.Balance.Remaining
		item["used"] = quota.Balance.Used
		item["total"] = quota.Balance.Total
	}
	if quota.BalanceErr != nil {
		item["credit_error"] = quota.BalanceErr.Error()
	}
	if quota.Activity != nil {
		item["daily_checkin"] = map[string]any{
			"campaign_id": quota.Activity.CampaignID,
			"claimable":   quota.Activity.Claimable,
			"status":      quota.Activity.Status,
		}
	}
	if quota.ActivityErr != nil {
		item["activity_error"] = quota.ActivityErr.Error()
	}
	return item
}

// quotaListJSON renders every account's figures, in host order.
func quotaListJSON(quotas []accountQuota) []map[string]any {
	out := make([]map[string]any, 0, len(quotas))
	for _, quota := range quotas {
		out = append(out, quotaJSON(quota))
	}
	return out
}

// fetchCreditBalance reads /snap-manager/v1/statistics/plugin. The response is a
// bare object, not a {code,data} envelope.
func fetchCreditBalance(h *abiboot.Host, credential *Credential) (*creditBalance, error) {
	response, errDo := snapRequest(h, credential, http.MethodGet, PackageInfoPath, nil, nil)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("credit_status", "package info returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 300))
	}

	var payload struct {
		Package struct {
			IsCreditPackage any `json:"is_credit_package"`
		} `json:"package"`
		Metrics []struct {
			Name   string  `json:"name"`
			Amount float64 `json:"package_credit_amount"`
			Used   float64 `json:"package_credit_used"`
			Remain float64 `json:"package_credit_remain"`
		} `json:"metrics"`
	}
	if errDecode := json.Unmarshal(response.Body, &payload); errDecode != nil {
		return nil, abiboot.Errorf("credit_decode", "decode package info: %v", errDecode)
	}

	balance := &creditBalance{IsCreditPackage: truthy(payload.Package.IsCreditPackage)}
	for _, metric := range payload.Metrics {
		if metric.Name == "usageTotalPackageCredit" {
			// The total is authoritative rather than the sum of the parts.
			balance.Remaining = metric.Remain
			balance.Used = metric.Used
			balance.Total = metric.Amount
			return balance, nil
		}
	}
	for _, metric := range payload.Metrics {
		for _, name := range creditMetricNames {
			if metric.Name == name {
				balance.Remaining += metric.Remain
				balance.Used += metric.Used
				balance.Total += metric.Amount
			}
		}
	}
	return balance, nil
}

// fetchDailyActivity reads /v1/ops/delivery and returns the USER_LOGIN campaign.
func fetchDailyActivity(h *abiboot.Host, credential *Credential) (*dailyActivity, error) {
	query := url.Values{"channel": {OpsChannel}}
	response, errDo := snapRequest(h, credential, http.MethodGet, OpsDeliveryPath, nil, query)
	if errDo != nil {
		return nil, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, abiboot.Errorf("ops_delivery", "activity list returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 300))
	}

	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Items []struct {
				CampaignID    any     `json:"campaignId"`
				Type          string  `json:"type"`
				Claimable     bool    `json:"claimable"`
				Status        *string `json:"status"`
				BenefitAmount float64 `json:"benefitAmount"`
			} `json:"items"`
		} `json:"data"`
	}
	if errDecode := json.Unmarshal(response.Body, &payload); errDecode != nil {
		return nil, abiboot.Errorf("ops_decode", "decode activity list: %v", errDecode)
	}
	if payload.Code != 0 {
		return nil, abiboot.Errorf("ops_rejected", "activity list rejected (code %d): %s", payload.Code, payload.Message)
	}

	for _, item := range payload.Data.Items {
		if item.Type != DailyLoginType {
			continue
		}
		activity := &dailyActivity{
			CampaignID:    stringifyCampaignID(item.CampaignID),
			Claimable:     item.Claimable,
			BenefitAmount: item.BenefitAmount,
		}
		if item.Status != nil {
			activity.Status = strings.ToUpper(strings.TrimSpace(*item.Status))
		}
		return activity, nil
	}
	return nil, nil
}

// claimDaily performs the four-step daily check-in.
func claimDaily(h *abiboot.Host, credential *Credential) (*checkinOutcome, error) {
	balance, errBalance := fetchCreditBalance(h, credential)
	if errBalance != nil {
		return nil, errBalance
	}
	if !balance.IsCreditPackage {
		return &checkinOutcome{Status: "inactive", Message: "该账号为 Token 计费，不适用每日签到", Balance: balance}, nil
	}

	activity, errActivity := fetchDailyActivity(h, credential)
	if errActivity != nil {
		return nil, errActivity
	}
	if activity == nil {
		return &checkinOutcome{Status: "inactive", Message: "未找到每日登录活动", Balance: balance}, nil
	}
	if activity.CampaignID == "" {
		return &checkinOutcome{Status: "failed", Message: "活动缺少 campaignId", Balance: balance}, nil
	}
	if !activity.Claimable {
		if claimedStatuses[activity.Status] {
			return &checkinOutcome{Status: "already-claimed", Message: "今日已签到", Balance: balance}, nil
		}
		return &checkinOutcome{Status: "inactive", Message: "当前不可领取", Balance: balance}, nil
	}

	claimBody, errMarshal := json.Marshal(map[string]string{
		"campaignId": activity.CampaignID,
		"channel":    OpsChannel,
	})
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_claim", "encode claim body: %v", errMarshal)
	}
	claimResponse, errClaim := snapRequest(h, credential, http.MethodPost, OpsClaimPath, claimBody, nil)
	if errClaim != nil {
		return nil, errClaim
	}
	if claimResponse.StatusCode < 200 || claimResponse.StatusCode >= 300 {
		return nil, abiboot.Errorf("claim_failed", "claim returned HTTP %d: %s", claimResponse.StatusCode, truncate(string(claimResponse.Body), 300))
	}

	var claim struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ID any `json:"id"`
		} `json:"data"`
	}
	if errDecode := json.Unmarshal(claimResponse.Body, &claim); errDecode != nil {
		return nil, abiboot.Errorf("claim_decode", "decode claim response: %v", errDecode)
	}
	if claim.Code != 0 {
		return nil, abiboot.Errorf("claim_rejected", "claim rejected (code %d): %s", claim.Code, claim.Message)
	}

	// Confirm only when the claim produced an identifier.
	if claim.Data.ID != nil {
		confirmBody, errConfirmMarshal := json.Marshal(map[string]string{
			"campaignId": activity.CampaignID,
			"channel":    OpsChannel,
		})
		if errConfirmMarshal == nil {
			if confirmResponse, errConfirm := snapRequest(h, credential, http.MethodPost, OpsConfirmPath, confirmBody, nil); errConfirm == nil {
				if confirmResponse.StatusCode < 200 || confirmResponse.StatusCode >= 300 {
					h.Log("warn", "CodeArts 签到确认失败", map[string]any{"status": confirmResponse.StatusCode})
				}
			}
		}
	}

	return &checkinOutcome{
		Status:  "claimed",
		Message: "签到成功",
		Amount:  activity.BenefitAmount,
		Balance: balance,
	}, nil
}

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the supported provider and reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "CodeArts 每日签到",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the current credit balance and check-in state.
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	credential, errCredential := ParseCredential(request.StorageJSON)
	if errCredential != nil {
		return nil, errCredential
	}
	balance, errBalance := fetchCreditBalance(h, credential)
	if errBalance != nil {
		return nil, errBalance
	}

	summary := []pluginapi.QuotaMetric{
		{Key: "credit_remaining", Label: "剩余额度", Value: balance.Remaining, Unit: "credit", Format: "number"},
		{Key: "credit_used", Label: "已用额度", Value: balance.Used, Unit: "credit", Format: "number"},
	}
	if activity, errActivity := fetchDailyActivity(h, credential); errActivity == nil && activity != nil {
		claimable := 0.0
		if activity.Claimable {
			claimable = 1
		}
		summary = append(summary, pluginapi.QuotaMetric{
			Key: "daily_checkin", Label: "今日可签到", Value: claimable, Format: "boolean",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "CodeArts 不支持重置额度"}, nil
}

// truthy accepts either a JSON boolean or the string "true".
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	case float64:
		return typed != 0
	default:
		return false
	}
}

// stringifyCampaignID renders a campaign id that may arrive as a number or a
// string. The claim body expects a string.
func stringifyCampaignID(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		encoded, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return ""
		}
		return strings.Trim(string(encoded), `"`)
	}
}
