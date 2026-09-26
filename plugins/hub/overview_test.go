package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
)

// channelOrder is the order the panel must list its providers in. It is pinned
// separately from targetCatalogue() on purpose: the overview is a user-visible
// contract, and a reordering of the catalogue that silently reshuffles the page
// is exactly the regression this test exists to catch.
var channelOrder = []string{
	"codearts", "codebuddy", "codebuddy-intl", "workbuddy-cn", "workbuddy",
	"lobsterai", "qoder", "trae", "cline", "loomy",
}

// TestChannelOverviewOrderAndIcons pins the row order and the marks: every
// provider in the reference panel's order, each with the icon the page embeds.
func TestChannelOverviewOrderAndIcons(t *testing.T) {
	catalogue := targetCatalogue()
	if len(catalogue) != len(channelOrder) {
		t.Fatalf("catalogue has %d providers, want %d", len(catalogue), len(channelOrder))
	}
	wantIcons := map[string]string{
		"codearts":       brandicons.CodeArts,
		"codebuddy":      brandicons.CodeBuddy,
		"codebuddy-intl": brandicons.CodeBuddy,
		"workbuddy-cn":   brandicons.WorkBuddy,
		"workbuddy":      brandicons.WorkBuddy,
		"lobsterai":      brandicons.LobsterAI,
		"qoder":          brandicons.Qoder,
		"trae":           brandicons.Trae,
		"cline":          brandicons.Cline,
		"loomy":          brandicons.Loomy,
	}
	for index, id := range channelOrder {
		entry := catalogue[index]
		if entry.ID != id {
			t.Fatalf("catalogue[%d] = %s, want %s", index, entry.ID, id)
		}
		if entry.Icon != wantIcons[id] {
			t.Fatalf("%s carries the wrong icon", id)
		}
		if !strings.HasPrefix(entry.Icon, "data:image/") {
			t.Fatalf("%s icon is not an embedded data URL", id)
		}
	}

	// The overview rows inherit that order.
	reports := channelReports(nil, DefaultConfig())
	if len(reports) != len(channelOrder) {
		t.Fatalf("overview has %d rows, want %d", len(reports), len(channelOrder))
	}
	for index, report := range reports {
		if report.Target.ID != channelOrder[index] {
			t.Fatalf("overview row %d = %s, want %s", index, report.Target.ID, channelOrder[index])
		}
		if want := resourcePrefix + channelOrder[index] + "/status"; report.URL() != want {
			t.Fatalf("row %s links to %s, want %s", report.Target.ID, report.URL(), want)
		}
	}
}

// TestChannelOverviewReportsProviderStatePerShape drives the whole page against
// one scripted answer per provider, in the exact field shape that provider's own
// statusJSON emits, and checks the summary line of each row. The expected
// strings are the provider's OWN fields: this test fails if the page starts
// inventing a value the document did not carry.
func TestChannelOverviewReportsProviderStatePerShape(t *testing.T) {
	fake := newFakeHost(
		account("codearts", "ca-1", "codearts-1.json"),
		account("codebuddy", "cb-1", "codebuddy-1.json"),
		account("lobsterai", "lb-1", "lobster-1.json"),
		account("qoder", "qd-1", "qoder-1.json"),
		account("trae", "tr-1", "trae-1.json"),
		account("cline", "cl-1", "cline-1.json"),
		account("loomy", "lo-1", "loomy-1.json"),
	)
	fake.install(t)
	fake.serve(
		jsonRoute("/codearts/status?", `{"auth_index":"ca-1","name":"codearts-1.json","credit_package":true,`+
			`"remaining":10,"used":5,"total":15,"daily_checkin":{"campaign_id":"c-1","claimable":false,"status":"CLAIMED"}}`),
		jsonRoute("/codebuddy/status?", `{"product":"codebuddy","account_count":1,"accounts":[{"auth_index":"cb-1","name":"codebuddy-1.json"}],`+
			`"auth_index":"cb-1","status":"正常","expires_at":"2026-01-02T03:04:05Z","credits":{"total":120},`+
			`"checkin":{"today_checked_in":true,"streak_days":3}}`),
		jsonRoute("/lobsterai/status?", `{"provider":"lobsterai","accounts":[{"auth_index":"lb-1","name":"lobster-1.json"}],`+
			`"selected":{"auth_index":"lb-1","status":"正常","expires_at":"2026-02-03T04:05:06Z",`+
			`"credit":{"total":50},"activity":{"slot_state":"OPEN","activity_code":"daily"}}}`),
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":2,"account":{"auth_index":"qd-1","status":"正常","expires_at":"2026-03-04T05:06:07Z"},`+
			`"credits":{"total":9,"packages":[]},"daily_checkin":{"claimable":true,"show":true,"campaigns":1}}`),
		jsonRoute("/trae/status?", `{"region":"cn","account_count":1,"accounts":[{"auth_index":"tr-1","expires_at_ms":1767225845000}],`+
			`"credits":42,"daily_checkin":{"checked_in":true,"credits":10,"streak_days":2}}`),
		jsonRoute("/cline/status?", `{"provider":"cline","accounts":1,"account":{"auth_index":"cl-1","status":"正常","email":"a@b.c","expires_at":"2026-04-05T06:07:08Z"}}`),
		jsonRoute("/loomy/status?", `{"provider":"loomy","accounts":1,"account":{"auth_index":"lo-1","status":"正常","expires_at":"2026-05-06T07:08:09Z"},`+
			`"points":{"balance":300,"daily_balance":10,"available":300,"daily_quota":50}}`),
		catchAll(),
	)
	withSettings(t, testConfig())

	reports := channelReports(testHost(), settings())
	byID := map[string]channelReport{}
	for _, report := range reports {
		byID[report.Target.ID] = report
	}

	// codearts reports the claim state and a credit balance at the top level.
	codearts := byID["codearts"]
	if codearts.State != channelReady || codearts.Accounts != 1 || codearts.Reported != 1 {
		t.Fatalf("codearts row = %+v", codearts)
	}
	for _, want := range []string{"今日不可领取", "剩余 10 / 15"} {
		if !strings.Contains(codearts.Status, want) {
			t.Fatalf("codearts status = %q, want %q", codearts.Status, want)
		}
	}

	// codebuddy nests the claim state, the account state and the credit total.
	codebuddy := byID["codebuddy"]
	for _, want := range []string{"今日已签到", "连续签到 3 天", "账号状态 正常", "积分 120", "有效期至"} {
		if !strings.Contains(codebuddy.Status, want) {
			t.Fatalf("codebuddy status = %q, want %q", codebuddy.Status, want)
		}
	}

	// lobsterai reports the state under "selected", including the activity slot.
	lobsterai := byID["lobsterai"]
	for _, want := range []string{"签到时段 OPEN", "账号状态 正常", "积分 50", "有效期至"} {
		if !strings.Contains(lobsterai.Status, want) {
			t.Fatalf("lobsterai status = %q, want %q", lobsterai.Status, want)
		}
	}

	// qoder's claimable flag and package total; the document reports two
	// accounts where the host ledger holds one, and the row must show both.
	qoder := byID["qoder"]
	if qoder.Accounts != 1 || qoder.Reported != 2 {
		t.Fatalf("qoder counts = %d/%d, want 1/2", qoder.Accounts, qoder.Reported)
	}
	for _, want := range []string{"今日可领取", "账号状态 正常", "积分 9"} {
		if !strings.Contains(qoder.Status, want) {
			t.Fatalf("qoder status = %q, want %q", qoder.Status, want)
		}
	}
	if got := string(channelAccountsCell(qoder)); !strings.Contains(got, "1") || !strings.Contains(got, "provider 报告 2") {
		t.Fatalf("qoder account cell = %q, want the ledger count and the provider's", got)
	}

	// trae reports a bare number for credits and milliseconds for the expiry.
	trae := byID["trae"]
	for _, want := range []string{"今日已签到", "连续签到 2 天", "积分 42", "有效期至 2026-"} {
		if !strings.Contains(trae.Status, want) {
			t.Fatalf("trae status = %q, want %q", trae.Status, want)
		}
	}

	for _, id := range []string{"cline", "loomy"} {
		report := byID[id]
		if report.State != channelReady {
			t.Fatalf("%s state = %s, want ready", id, report.State)
		}
		for _, want := range []string{"账号状态 正常", "有效期至"} {
			if !strings.Contains(report.Status, want) {
				t.Fatalf("%s status = %q, want %q", id, report.Status, want)
			}
		}
	}
	if !strings.Contains(byID["loomy"].Status, "积分余额 300") || !strings.Contains(byID["loomy"].Status, "每日额度 50") {
		t.Fatalf("loomy status = %q, want the points the document carried", byID["loomy"].Status)
	}

	// The three codebuddy-family variants are not installed in this script.
	for _, id := range []string{"codebuddy-intl", "workbuddy-cn", "workbuddy"} {
		if report := byID[id]; report.State != channelMissing {
			t.Fatalf("%s state = %s, want missing (its resource route answers 404)", id, report.State)
		}
	}
}

// TestChannelStatusLineInventsNothing: a document whose fields this page does
// not know must say so instead of showing a made-up status, and a provider that
// reports zero accounts must say that rather than showing two empty cells.
func TestChannelStatusLineInventsNothing(t *testing.T) {
	line := channelStatusLine(map[string]any{"something_new": "42", "nested": map[string]any{"x": 1}})
	if line != "provider 只返回了本页不展示的配置字段" {
		t.Fatalf("line = %q, want an explicit no-field message", line)
	}
	if line := channelStatusLine(nil); line != "provider 未返回 JSON 文档" {
		t.Fatalf("nil document line = %q", line)
	}
	// An upstream error the provider wrote into its own document is reported
	// first, because it explains why the rest may be missing.
	line = channelStatusLine(map[string]any{"points_error": "上游 502", "points_note": "服务端未返回 balance 字段"})
	if !strings.HasPrefix(line, "积分读取失败：上游 502") || !strings.Contains(line, "服务端未返回 balance 字段") {
		t.Fatalf("line = %q, want the provider's own error and note", line)
	}
	// The empty-account note reads the count the provider itself reported, and
	// the model count next to it is a field of that same document.
	line = channelStatusLine(map[string]any{"provider": "qoder", "accounts": 0, "account": nil, "model_count": 27})
	if line != "尚无账号 · 模型 27" {
		t.Fatalf("line = %q, want the provider's own zero count and model count", line)
	}
	// A provider WITH accounts must not claim to have none.
	if line := channelStatusLine(map[string]any{"accounts": 2, "model_count": 3}); strings.Contains(line, "尚无账号") {
		t.Fatalf("line = %q, must not report an empty account list", line)
	}
}

// TestChannelOverviewRendersMissingAndUnreadableRows: a provider that is not
// installed and one whose document cannot be read are both ordinary rows with a
// reason, never an error page or a blank line.
func TestChannelOverviewRendersMissingAndUnreadableRows(t *testing.T) {
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"), account("trae", "tr-1", "trae-1.json"))
	fake.install(t)
	fake.serve(
		// qoder answers with something that is not JSON at all.
		route{match: "/qoder/status?", status: http.StatusOK, body: "<html>not json</html>", contentType: "text/html"},
		// Everything else 404s, including trae: the provider plugin is absent.
		catchAll(),
	)
	withSettings(t, testConfig("qoder", "trae"))

	reports := channelReports(testHost(), settings())
	if len(reports) != 2 {
		t.Fatalf("rows = %d, want 2", len(reports))
	}
	if reports[0].State != channelUnreadable || reports[0].Error == "" {
		t.Fatalf("qoder row = %+v, want an unreadable row carrying the reason", reports[0])
	}
	if reports[1].State != channelMissing || reports[1].Status != "未安装或未启用" {
		t.Fatalf("trae row = %+v, want a missing row", reports[1])
	}

	page := string(renderStatusPage(reports, settings(), nil).Body)
	// The absent provider is rendered as such, and its account cell stays empty
	// rather than showing a zero the ledger does not actually hold.
	for _, want := range []string{"未安装或未启用", "状态不可读", "—"} {
		if !strings.Contains(page, want) {
			t.Fatalf("overview page is missing %q", want)
		}
	}
}

// TestChannelOverviewEscapesProviderText: the status line carries upstream
// strings, so it must never be rendered as markup.
func TestChannelOverviewEscapesProviderText(t *testing.T) {
	report := channelReport{
		Target:   mustTarget(t, "qoder"),
		State:    channelUnreadable,
		Accounts: 1,
		Status:   "状态不可读",
		Error:    `<img src=x onerror="alert(1)">`,
	}
	page := string(renderStatusPage([]channelReport{report}, DefaultConfig(), nil).Body)
	if strings.Contains(page, `onerror="alert(1)"`) {
		t.Fatal("the provider error was not escaped")
	}
	// The mark itself must survive: it is the one value written into an
	// attribute, and it is a compile-time constant of this program.
	if !strings.Contains(page, `<img src="data:image/`) {
		t.Fatal("the provider mark is missing")
	}
}

// TestChannelOverviewOnlyEverReadsStatusRoutes: the overview is the read-only
// half of the hub, so the URLs it builds must never carry a claim parameter,
// even when every provider answers.
func TestChannelOverviewOnlyEverReadsStatusRoutes(t *testing.T) {
	fake := newFakeHost(allAccounts()...)
	fake.install(t)
	fake.serve(scriptedRoutes()...)
	withSettings(t, testConfig())

	// Both representations of the menu route.
	dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status", nil, "text/html"))
	dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"format": {"json"}}, "application/json"))

	requests := fake.requestURLs()
	if len(requests) == 0 {
		t.Fatal("no provider was probed")
	}
	for _, rawURL := range requests {
		if strings.Contains(rawURL, "/checkin") || strings.Contains(rawURL, "action=") {
			t.Fatalf("the overview issued a claim-shaped request: %s", rawURL)
		}
	}
}

// TestStatusDocumentOverviewFields pins the machine-readable overview: one entry
// per provider with the state, both account counts, the status line and the link
// to the provider's own page.
func TestStatusDocumentOverviewFields(t *testing.T) {
	fake := newFakeHost(account("qoder", "qd-1", "qoder-1.json"))
	fake.install(t)
	fake.serve(
		jsonRoute("/qoder/status?", `{"provider":"qoder","accounts":1,"account":{"auth_index":"qd-1","status":"正常"},"credits":{"total":7}}`),
		catchAll(),
	)
	withSettings(t, testConfig("qoder"))

	response := dispatchManagement(t, managementRequest(http.MethodGet, "/v0/resource/plugins/hub/status",
		url.Values{"format": {"json"}}, "application/json"))
	document := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.Body, &document); errUnmarshal != nil {
		t.Fatalf("document is not JSON: %v", errUnmarshal)
	}
	channels, okChannels := document["channels"].([]any)
	if !okChannels || len(channels) != 1 {
		t.Fatalf("channels = %#v, want one row", document["channels"])
	}
	row, _ := channels[0].(map[string]any)
	for _, key := range []string{"provider", "label", "state", "installed", "accounts", "reported_accounts", "status", "url", "supported", "request"} {
		if _, okKey := row[key]; !okKey {
			t.Fatalf("channel row is missing %q: %#v", key, row)
		}
	}
	if row["provider"] != "qoder" || row["installed"] != true || row["accounts"] != float64(1) {
		t.Fatalf("channel row = %#v", row)
	}
	if row["url"] != "/v0/resource/plugins/qoder/status" {
		t.Fatalf("channel url = %v", row["url"])
	}
	if !strings.Contains(row["status"].(string), "积分 7") {
		t.Fatalf("channel status = %v, want the provider's own credit", row["status"])
	}
	// The provider-specific request description is still there for an operator
	// who wants to replay it by hand.
	if !strings.Contains(row["request"].(string), "/v0/resource/plugins/qoder/checkin") {
		t.Fatalf("channel request = %v", row["request"])
	}
}
