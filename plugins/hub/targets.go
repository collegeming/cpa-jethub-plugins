package main

import (
	"net/url"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/brandicons"
)

// This file is the catalogue of providers the one-click run drives.
//
// A CPA plugin cannot call another plugin directly and the management API needs
// a key this plugin does not have, but the host mounts every provider plugin's
// browser pages under `/v0/resource/plugins/<pluginID>/<path>` WITHOUT
// management authentication, and dispatches them as GET (verified in
// internal/pluginhost/management.go: ServeResourceHTTP rejects any non-GET and
// looks the path up in the resource route table built from
// `management.register`). The route table is the contract this file encodes:
// every entry below was read out of the provider plugin's own registration and
// its check-in handler, not guessed from the provider's web console.
//
// Two shapes exist:
//
//   - supportJSON: the provider registers a `/checkin` resource route that
//     returns machine-readable JSON when the caller passes `format=json`, so the
//     verdict can be read straight out of the response body;
//   - supportStatusHTML: the provider's check-in button is a query string on its
//     STATUS page and only the HTML representation performs the write
//     (`plugins/codearts/pluginui.go`, renderStatusPage reads `action=checkin`
//     while statusJSON does not). Passing `format=json` there returns the status
//     document and silently skips the claim, so the driver must ask for HTML and
//     then re-read `?format=json` to obtain the provider's own post-claim state.
type support int

const (
	// supportJSON drives a provider `/checkin` resource route that emits JSON.
	supportJSON support = iota
	// supportStatusHTML drives a provider whose claim lives on its status page.
	supportStatusHTML
	// supportNone is a provider with no check-in endpoint anywhere upstream.
	// It is reported as 不支持 instead of being omitted or faked.
	supportNone
)

// target describes one provider the orchestrator can drive.
type target struct {
	// ID is both the provider key in auth files and the CPA plugin id (the
	// artifact file name stem), so it is also the `/v0/resource/plugins/<id>`
	// segment.
	ID string
	// Label is the Chinese display name used in the aggregated table.
	Label string
	// Icon is the provider mark the channel overview renders: a data URL from
	// internal/jethub/brandicons, embedded in the page instead of linked. A
	// vendor favicon would be an outbound request from a panel that is otherwise
	// fully offline, and the marks would differ in size and shape.
	Icon string
	// Support selects the driver.
	Support support
	// CheckinPath is the resource route that performs the claim.
	CheckinPath string
	// CheckinQuery carries the parameters that route requires to actually
	// write. It is empty when the route claims unconditionally.
	CheckinQuery url.Values
	// Note explains a caveat shown next to the target.
	Note string
}

// targetCatalogue is every provider CPA has a plugin for, in a stable order.
//
// The order is the order of the channel overview rows, and it matches the
// reference Jet Hub panel's provider list. The CodeBuddy family appears as four
// separate CPA plugin ids — one source tree is built once per product
// (`scripts/build.sh` VARIANTS), because a CPA plugin registers exactly one
// provider key — so a host that installs only one variant simply reports the
// other three as unreachable.
func targetCatalogue() []target {
	return []target{
		{
			ID: "codearts", Label: "CodeArts Agent", Icon: brandicons.CodeArts,
			Support:     supportStatusHTML,
			CheckinPath: "/status",
			CheckinQuery: url.Values{
				// `action=checkin` is read by renderStatusPage; there is no JSON
				// representation of this claim.
				"action": {"checkin"},
			},
			Note: "签到入口在状态页（/status?action=checkin，HTML），没有 JSON 版；结果用随后的 ?format=json 状态复核",
		},
		{
			ID: "codebuddy", Label: "CodeBuddy", Icon: brandicons.CodeBuddy,
			Support:      supportJSON,
			CheckinPath:  "/checkin",
			CheckinQuery: url.Values{"action": {"checkin"}},
			Note:         "只有启用签到接口的产品才注册 /checkin 资源路由，其它产品会如实报告不支持",
		},
		{
			ID: "codebuddy-intl", Label: "CodeBuddy（国际版）", Icon: brandicons.CodeBuddy,
			Support:      supportJSON,
			CheckinPath:  "/checkin",
			CheckinQuery: url.Values{"action": {"checkin"}},
			Note:         "同一份 codebuddy 源码按产品构建的独立插件 id；未安装时报告未启用",
		},
		{
			ID: "workbuddy-cn", Label: "WorkBuddy（国内版）", Icon: brandicons.WorkBuddy,
			Support:      supportJSON,
			CheckinPath:  "/checkin",
			CheckinQuery: url.Values{"action": {"checkin"}},
			Note:         "同一份 codebuddy 源码按产品构建的独立插件 id；未安装时报告未启用",
		},
		{
			ID: "workbuddy", Label: "WorkBuddy（国际版）", Icon: brandicons.WorkBuddy,
			Support:      supportJSON,
			CheckinPath:  "/checkin",
			CheckinQuery: url.Values{"action": {"checkin"}},
			Note:         "同一份 codebuddy 源码按产品构建的独立插件 id；未安装时报告未启用",
		},
		{
			ID: "lobsterai", Label: "LobsterAI", Icon: brandicons.LobsterAI,
			Support:     supportJSON,
			CheckinPath: "/checkin",
			// lobsterai's checkinResponse claims unconditionally and its page
			// links to `?action=checkin`; `daily_checkin=false` in that plugin's
			// own config is reported back as status=inactive.
			CheckinQuery: url.Values{"action": {"checkin"}},
		},
		{
			ID: "qoder", Label: "Qoder", Icon: brandicons.Qoder,
			Support:     supportJSON,
			CheckinPath: "/checkin",
			// qoder's checkinResponse claims unconditionally; the provider's own
			// page links to `?action=checkin`, so the same parameter is sent to
			// stay on its documented surface.
			CheckinQuery: url.Values{"action": {"checkin"}},
		},
		{
			ID: "trae", Label: "TRAE", Icon: brandicons.Trae,
			Support:     supportJSON,
			CheckinPath: "/checkin",
			// REQUIRED: trae's checkinResponse only claims when the query carries
			// `action=claim`; without it the route merely reports the state
			// (`plugins/trae/pluginui.go`). The status page uses the word
			// `checkin`, but that is a different route.
			CheckinQuery: url.Values{"action": {"claim"}},
			Note:         "领取参数是 action=claim，不是 status 页上的 action=checkin",
		},
		{
			ID: "cline", Label: "Cline", Icon: brandicons.Cline,
			Support: supportNone,
			Note:    "Cline 上游没有任何签到接口（插件只提供登录、余额与推理），因此不支持一键签到",
		},
		{
			ID: "loomy", Label: "Loomy（讯飞）", Icon: brandicons.Loomy,
			Support:     supportJSON,
			CheckinPath: "/checkin",
			// REQUIRED: loomy's checkinJSON answers with a confirm hint unless
			// `action=claim` is present. Only the DAILY GRANT belongs here; the
			// one-off onboarding tasks live on /onboarding and are deliberately
			// NOT part of the one-click run.
			CheckinQuery: url.Values{"action": {"claim"}},
			Note:         "只执行每日赠送额度初始化，不含一次性新手任务（/onboarding）",
		},
		{
			ID: "raccoon", Label: "Raccoon（商汤）", Icon: brandicons.Raccoon,
			Support: supportNone,
			// No daily check-in exists upstream: the daily 300 is granted by the
			// server on its own. The one-off desktop login reward is a separate,
			// explicitly-invoked action on the plugin's own page and must never be
			// swept into a "claim everything" run, which is what this catalogue
			// drives.
			Note: "Raccoon 没有每日签到（每日额度由服务端自动发放）；一次性登录奖励需在插件页单独领取，不参与一键动作",
		},
	}
}

// targetByID resolves one catalogue entry. Callers that start from a provider id
// — the overview and the tests — use this instead of a slice index, so
// reordering the catalogue (which the panel mirrors) cannot silently point them
// at a different provider.
func targetByID(id string) (target, bool) {
	for _, entry := range targetCatalogue() {
		if entry.ID == id {
			return entry, true
		}
	}
	return target{}, false
}

// selectTargets returns the catalogue filtered by the `providers` setting.
func selectTargets(cfg Config) []target {
	wanted := selectedProviders(cfg)
	if len(wanted) == 0 {
		return targetCatalogue()
	}
	allowed := make(map[string]bool, len(wanted))
	for _, id := range wanted {
		allowed[id] = true
	}
	out := make([]target, 0, len(wanted))
	for _, entry := range targetCatalogue() {
		if allowed[entry.ID] {
			out = append(out, entry)
		}
	}
	return out
}

// supportsCheckin reports whether the provider has a check-in endpoint at all.
func (t target) supportsCheckin() bool { return t.Support != supportNone }

// checkinDescription renders the exact request the driver will send, for the
// status page and the JSON document, so an operator can verify it by hand.
func (t target) checkinDescription() string {
	if !t.supportsCheckin() {
		return "不支持"
	}
	query := url.Values{}
	for key, values := range t.CheckinQuery {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	query.Set("auth_index", "<索引>")
	if t.Support != supportStatusHTML {
		query.Set("format", "json")
	}
	return "GET " + resourcePrefix + t.ID + t.CheckinPath + "?" + query.Encode()
}

// resourcePrefix is the host mount point of every plugin's browser pages.
const resourcePrefix = "/v0/resource/plugins/"

// resourceURL builds an absolute URL for one provider resource route.
func resourceURL(baseURL, pluginID, path string, query url.Values) string {
	origin := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	target := origin + resourcePrefix + url.PathEscape(pluginID) + path
	if len(query) == 0 {
		return target
	}
	return target + "?" + query.Encode()
}
