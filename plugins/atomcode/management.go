package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/authrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The JSON half of the management surface: the same facts the HTML pages render,
// in a shape a script can consume (`?format=json`).

// atomcodeAccounts lists the auth files this plugin owns.
func atomcodeAccounts(h *abiboot.Host) []pluginapi.HostAuthFileEntry {
	if h == nil {
		return nil
	}
	entries, errList := h.ListAuth()
	if errList != nil {
		return nil
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Provider == ProviderKey || entry.Type == ProviderKey {
			out = append(out, entry)
		}
	}
	return out
}

// selectAccount resolves which credential a page acts on. Without an explicit
// selector the first account is used, so the page works with no parameters.
func selectAccount(h *abiboot.Host, request pluginapi.ManagementRequest) (pluginapi.HostAuthFileEntry, bool) {
	wanted := strings.TrimSpace(request.Query.Get("auth_index"))
	if wanted == "" {
		wanted = strings.TrimSpace(request.Query.Get("auth_id"))
	}
	for _, entry := range atomcodeAccounts(h) {
		if wanted == "" {
			return entry, true
		}
		if entry.AuthIndex == wanted || entry.ID == wanted || entry.Name == wanted {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// credentialOf loads one account's credential and makes sure it is still valid
// before a page uses it: a credential inside the renewal lead window is renewed
// through this provider's own `auth.refresh` handler and written back to the auth
// file the host knows.
//
// The returned error covers a credential that could not be READ. A renewal that
// failed comes back inside the result instead: it does not stop the caller from
// showing the credential's own fields, and — while the credential is still inside
// its validity — does not stop the upstream read either.
func credentialOf(h *abiboot.Host, entry pluginapi.HostAuthFileEntry) (*Credential, authrefresh.Result, error) {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return nil, authrefresh.Result{}, abiboot.Errorf("missing_auth", "账号 %s 缺少运行时索引", entry.Name)
	}
	auth, errGet := h.GetAuth(entry.AuthIndex)
	if errGet != nil {
		return nil, authrefresh.Result{}, errGet
	}
	credential, errParse := ParseCredential(auth.JSON)
	if errParse != nil {
		return nil, authrefresh.Result{}, errParse
	}
	fresh, _ := ensureCredentialFresh(h, authrefresh.Request{
		Name:        entry.Name,
		StorageJSON: auth.JSON,
	})
	if fresh.Refreshed {
		if refreshed, errRefreshed := ParseCredential(fresh.Storage); errRefreshed == nil {
			credential = refreshed
		}
	}
	return credential, fresh, nil
}

// accountSummary is one account's line in the JSON listing.
type accountSummary struct {
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	AccountID   string `json:"account_id"`
	Username    string `json:"username"`
	Refreshable bool   `json:"refreshable"`
	// ExpiresAt is the human-readable expiry; ExpiresAtMS and Expired are the
	// machine-readable pair the hub's channel overview reads
	// (`plugins/hub/overview.go`, expiryFacts: `accounts.0.expires_at_ms`,
	// `accounts.0.expired`).
	ExpiresAt   string `json:"expires_at,omitempty"`
	ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
	Expired     bool   `json:"expired"`
	Current     bool   `json:"current"`
	// Remaining and Total are the rolling call-quota figures for this account.
	// The hub renders them as "剩余 R / T" and puts them on the account's own
	// line (`plugins/hub/overview.go`, creditFacts + accountFigures). The free
	// tier's quota is a call count per window, so these are call counts, not
	// tokens.
	Remaining *int64 `json:"remaining,omitempty"`
	Total     *int64 `json:"total,omitempty"`
}

// summariseAccount renders one account for the listing.
func summariseAccount(entry pluginapi.HostAuthFileEntry, credential *Credential, selected bool) accountSummary {
	summary := accountSummary{
		AuthIndex: entry.AuthIndex,
		Name:      entry.Name,
		Label:     entry.Name,
		Current:   selected,
	}
	if credential == nil {
		return summary
	}
	summary.Label = credential.Label()
	summary.AccountID = credential.AccountID()
	summary.Username = credential.User.Username
	summary.Refreshable = credential.Refreshable()
	if expiry := credential.Expiry(); !expiry.IsZero() {
		summary.ExpiresAt = expiry.Format("2006-01-02 15:04")
		summary.ExpiresAtMS = expiry.UnixMilli()
		summary.Expired = credential.Expired()
	}
	return summary
}

// statusJSON reports the plugin state, the accounts and the selected account's
// plan, quota and models.
func statusJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	cfg := settings()
	accounts := atomcodeAccounts(h)
	body := map[string]any{
		"provider":        ProviderKey,
		"version":         PluginVersion,
		"broker":          cfg.BrokerBase,
		"codingplan_api":  cfg.CodingPlanAPIBase,
		"gateway":         cfg.GatewayBase,
		"user_agent":      AdapterUserAgent,
		"discover_models": cfg.DiscoverModels,
		"account_count":   len(accounts),
	}
	if len(accounts) == 0 {
		body["accounts"] = []any{}
		return jsonManagementResponse(http.StatusOK, body)
	}

	selected, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}

	// One credential read per account: the listing labels every account, and the
	// selected one additionally drives the plan, quota and catalogue reads.
	listing := make([]accountSummary, 0, len(accounts))
	var selectedCredential *Credential
	for _, entry := range accounts {
		isSelected := entry.AuthIndex == selected.AuthIndex && entry.Name == selected.Name
		credential, _, errCredential := credentialOf(h, entry)
		if errCredential != nil {
			listing = append(listing, summariseAccount(entry, nil, isSelected))
			continue
		}
		if isSelected {
			selectedCredential = credential
		}
		summary := summariseAccount(entry, credential, isSelected)
		// Best effort: a failed quota read must not hide the account itself.
		if status, errStatus := fetchStatus(h, cfg, credential); errStatus == nil {
			if windows := status.quotaWindows(); len(windows) > 0 {
				remaining := windows[0].CallLimit - windows[0].CallsUsed
				if remaining < 0 {
					remaining = 0
				}
				limit := windows[0].CallLimit
				summary.Remaining = &remaining
				summary.Total = &limit
			}
		}
		listing = append(listing, summary)
	}
	body["accounts"] = listing
	body["auth_index"] = selected.AuthIndex

	if selectedCredential == nil {
		body["error"] = "无法读取所选账号的凭据"
		return jsonManagementResponse(http.StatusOK, body)
	}

	if status, errStatus := fetchStatus(h, cfg, selectedCredential); errStatus == nil {
		body["plan"] = status
		body["plan_active"] = status.active()
		body["plan_tier"] = resolvePlanType(h, cfg, selectedCredential)
	} else {
		body["plan_error"] = errStatus.Error()
	}
	if usage, errUsage := fetchUsage(h, cfg, selectedCredential); errUsage == nil {
		body["usage"] = usage
	} else {
		body["usage_error"] = errUsage.Error()
	}
	entries := staticModelEntries(h, cfg, selectedCredential)
	body["models"] = modelInfos(entries, cfg, selectedCredential)
	body["model_source"] = modelSource(h, cfg, selectedCredential)
	// Fields the hub's channel overview renders (`plugins/hub/overview.go`):
	// `model_count` -> "模型 N", `expires_at`/`expires_at_ms`/`expired` -> the
	// credential's validity line. Without them the row falls back to
	// "provider 只返回了本页不展示的配置字段" and the channel looks inert.
	body["model_count"] = len(entries)
	extra := 0
	for _, entry := range entries {
		if entry.FromConfig {
			extra++
		}
	}
	body["extra_model_count"] = extra
	if expiry := selectedCredential.Expiry(); !expiry.IsZero() {
		body["expires_at"] = expiry.Format(time.RFC3339)
		body["expires_at_ms"] = expiry.UnixMilli()
		body["expired"] = selectedCredential.Expired()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// modelSource reports whether the model list came from the server or the
// bundled snapshot, so a caller can tell a fresh catalogue from a fallback.
func modelSource(h *abiboot.Host, cfg Config, credential *Credential) string {
	if !cfg.DiscoverModels {
		return "bundled"
	}
	entry, errCatalogue := catalogueFor(h, cfg, credential)
	if errCatalogue != nil || len(entry.models) == 0 {
		return "bundled"
	}
	return "server"
}

// checkinJSON runs the claim cascade and reports the plan afterwards.
//
// The claim is the AtomCode equivalent of a daily check-in: it grants the free
// tier for the account and is what makes the gateway answer at all — before the
// first claim the gateway reports `403 user has no codingplan` on every request.
//
// ⚠️ The cascade runs ONLY for `action=claim`, matching the HTML page. Without
// that guard a bare `GET /checkin?format=json` — a monitoring poll, or a manager
// rendering the route — would claim on the user's behalf. The hub catalogue
// carries the parameter for exactly this reason (`plugins/hub/targets.go`,
// the atomcode entry).
func checkinJSON(h *abiboot.Host, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(atomcodeAccounts(h)) == 0 {
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"status":  "no-account",
			"message": "尚未登录任何 AtomGit 账号，请先在登录页发起登录",
		})
	}
	entry, found := selectAccount(h, request)
	if !found {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "指定的 auth_index 不存在"})
	}
	credential, _, errCredential := credentialOf(h, entry)
	if errCredential != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errCredential.Error()})
	}
	cfg := settings()

	if !isClaimRequest(request) {
		// Report-only: never claim from a page load or a poll.
		body := map[string]any{
			"status":  "needs-action",
			"message": "未执行领取：该接口只在 action=claim 时领取，本次仅返回套餐状态",
			"account": entry.Name,
		}
		if status, errStatus := fetchStatus(h, cfg, credential); errStatus == nil {
			body["plan"] = status
			body["plan_active"] = status.active()
		} else {
			body["plan_error"] = errStatus.Error()
		}
		return jsonManagementResponse(http.StatusOK, body)
	}

	outcome := claimCascade(h, cfg, credential)
	// The tier decides which models `models-v2` reports as available, so a claim
	// that changed it must not leave a stale catalogue behind.
	invalidateCatalogue(credential)

	body := map[string]any{
		"status":   outcome.Status,
		"message":  outcome.Message,
		"account":  entry.Name,
		"attempts": outcome.Attempts,
	}
	if outcome.PlanName != "" {
		body["plan_name"] = outcome.PlanName
	}
	if status, errStatus := fetchStatus(h, cfg, credential); errStatus == nil {
		body["plan"] = status
		body["plan_active"] = status.active()
	} else {
		body["plan_error"] = errStatus.Error()
	}
	return jsonManagementResponse(http.StatusOK, body)
}

// isClaimRequest reports whether a request asked for the claim to actually run.
//
// Both spellings are accepted because the hub catalogue and the HTML page use
// `claim` while a script written against the sibling providers may send
// `checkin`; anything else — including no parameter at all — is a report.
func isClaimRequest(request pluginapi.ManagementRequest) bool {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("action"))) {
	case "claim", "checkin":
		return true
	default:
		return false
	}
}
