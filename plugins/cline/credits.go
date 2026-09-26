package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Cline account balance, ported from `src/cline-credits.ts`.
//
//	GET {APIBase}/api/v1/users/{account_id}/balance
//
// ⚠️ The `{id}` segment is the Cline account id (`usr-…`), NEVER the JWT `sub`
// (`user_…`): passing the subject answers `400 {"error":"Invalid request
// format"}` (`cline-credits.ts:15-19`). Credentials minted by an older tool may
// carry no account id at all, in which case the balance is unavailable — that is
// reported as such, never as 0.
//
// ⚠️ The unit is a guess. `balance: 500000` is assumed to be micro-USD, with, in
// the source's own words, "no source evidence" (`cline-credits.ts:26-33`). The
// divisor is one named constant (balanceDivisorRaw) and one configuration field.
//
// There is NO check-in and no quota reset for Cline: the credits RPC answers a
// hard error because the backend has no such endpoint (`jet-hub-rpc.ts:1487-1502`,
// `credits-capabilities.js:86`). The plugin therefore declares
// `SupportsReset: false` instead of inventing one.

// balanceSnapshot is the parsed balance answer.
type balanceSnapshot struct {
	// Raw is the untouched number from the response. Keeping it is deliberate:
	// it is the only way to re-derive the scale once a real account confirms it
	// (`cline-credits.ts:85-86`).
	Raw float64
	// Total is Raw converted by the divisor and rounded to 2 decimals.
	Total float64
	// Unit is the display unit of Total.
	Unit string
	// AccountID is the id the endpoint echoed (or the requested one).
	AccountID string
}

// parseBalance implements the parsing order of `cline-credits.ts:142-162`.
//
// The `error` field is read FIRST. The measured HTTP 401 body is
// `{"error":"Unauthorized: Please make sure you're using the latest version of
// Cline and re-authenticate your Cline account."}` and carries NO `success`
// field at all, so a check that only looks at `success` would read it as a
// successful answer with a missing `data` object — a real fixed defect in the
// source (`cline-credits.ts:133-140`).
func parseBalance(body []byte) (float64, string, error) {
	payload := decodeObject(body)
	errorText := readStringField(payload, "error", "message")
	success, hasSuccess := payload["success"].(bool)
	if hasSuccess && !success {
		detail := errorText
		if detail == "" {
			detail = "success=false"
		}
		return 0, "", fmt.Errorf("%s", detail)
	}
	if errorText != "" && !(hasSuccess && success) {
		return 0, "", fmt.Errorf("%s", errorText)
	}
	data, okData := payload["data"].(map[string]any)
	if !okData {
		return 0, "", fmt.Errorf("余额响应缺少 data 对象")
	}
	raw, okRaw := readNumberField(data, "balance")
	if !okRaw {
		return 0, "", fmt.Errorf("余额响应的 data.balance 不是数字")
	}
	return raw, readStringField(data, "userId", "user_id"), nil
}

// fetchBalance reads the account balance.
//
// A failure is a failure with a reason, never a 0 balance
// (`cline-credits.ts:130-131`): reporting "0 remaining" for an unreachable
// endpoint is indistinguishable from an exhausted account.
func fetchBalance(d doer, credential *Credential, cfg Config) (*balanceSnapshot, error) {
	accountID := strings.TrimSpace(credential.AccountID)
	if accountID == "" {
		return nil, statusError(false, "missing_account_id", http.StatusBadRequest,
			"Cline 凭据缺少 account_id，无法查询余额（该接口的路径参数是 Cline 账号 id，不是 JWT 的 sub）")
	}
	response, errDo := d(http.MethodGet, APIBase+balancePath(accountID), clineHeaders(credential), nil)
	if errDo != nil {
		return nil, transportError("balance_transport", "查询余额失败：%v", errDo)
	}
	raw, echoed, errParse := parseBalance(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// A non-2xx body is parsed with the SAME function, so its `error` text
		// becomes the detail rather than a raw HTML dump
		// (`cline-credits.ts:200-208`).
		detail := upstreamErrorDetail(response.Body)
		if errParse != nil {
			detail = errParse.Error()
		}
		return nil, upstreamStatusError("balance_status", response.StatusCode,
			"余额查询失败（HTTP %d）：%s%s", response.StatusCode, detail, credentialAdvice(response.StatusCode))
	}
	if errParse != nil {
		return nil, transportError("balance_shape", "解析余额响应失败：%v", errParse)
	}
	divisor := cfg.BalanceDivisor
	if divisor <= 0 {
		divisor = balanceDivisorRaw
	}
	if echoed == "" {
		echoed = accountID
	}
	return &balanceSnapshot{
		Raw:       raw,
		Total:     math.Round(raw/divisor*100) / 100,
		Unit:      "USD",
		AccountID: echoed,
	}, nil
}

// ---------------------------------------------------------------------------
// quota provider
// ---------------------------------------------------------------------------

// handleQuotaIdentifier advertises the provider this quota source covers.
func handleQuotaIdentifier(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return abiboot.IdentifierReply(ProviderKey), nil
}

// handleQuotaDescribe declares the provider and the reset capability.
func handleQuotaDescribe(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderKey},
		DisplayName:        "Cline 账户余额",
		SupportsReset:      false,
	}, nil
}

// handleQuotaFetch reports the account balance as a single metric, mirroring the
// single pseudo-package Jet-Hub builds (`cline-credits.ts:110-124`: an active
// package named `Cline 账户余额`, unit USD, remaining = total, used = 0).
func handleQuotaFetch(h *abiboot.Host, raw json.RawMessage) (any, error) {
	request, errDecode := abiboot.Decode[pluginapi.QuotaFetchRequest](raw)
	if errDecode != nil {
		return nil, errDecode
	}
	// The host asks for a quota read before the page is ever opened, so the
	// credential is renewed here as well.
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
	balance, errBalance := fetchBalance(transportFor(h), credential, settings())
	if errBalance != nil {
		return nil, errBalance
	}
	summary := []pluginapi.QuotaMetric{{
		Key:      "balance",
		Label:    "Cline 账户余额",
		Value:    balance.Total,
		Unit:     balance.Unit,
		Format:   "currency",
		Currency: "USD",
	}}
	return pluginapi.QuotaFetchResponse{Summary: summary}, nil
}

// handleQuotaReset is declared unsupported; the host should not call it.
func handleQuotaReset(_ *abiboot.Host, _ json.RawMessage) (any, error) {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "Cline 不支持重置额度（其后端只有余额查询接口，没有签到或额度重置接口）",
	}, nil
}

// balanceJSON renders a balance for the machine-readable status payload.
func balanceJSON(balance *balanceSnapshot) map[string]any {
	return map[string]any{
		"total":      balance.Total,
		"raw":        balance.Raw,
		"unit":       balance.Unit,
		"account_id": balance.AccountID,
	}
}
