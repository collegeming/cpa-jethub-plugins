// Package plugui renders the HTML pages a CPA plugin exposes to management
// clients.
//
// CPA turns every entry registered through `management.register` into a menu
// entry, and CPA-Manager-Plus renders each menu as a page containing a
// same-origin iframe pointed at that route (see the CPAMP proxy allowlist for
// `/v0/resource/plugins/...`). CPAMP additionally injects a stylesheet into the
// iframe <head> that defines the host theme as CSS custom properties, so a page
// only has to consume those variables to look native in both light and dark
// themes — no JavaScript, no build step, no asset pipeline.
//
// The variables the host guarantees are:
//
//	--bg-primary --bg-secondary --bg-tertiary --bg-hover
//	--text-primary --text-secondary --text-tertiary
//	--border-color --border-hover
//	--primary-color --primary-hover --primary-active --primary-contrast
//	--success-color --warning-color --danger-color
//	--app-surface --app-surface-muted --app-border --app-text-*
//	--app-input-bg --app-input-bg-focus --app-radius-sm --app-radius-md
//	--focus-bg --focus-border --focus-inset
package plugui

import (
	"bytes"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// documentTemplate is the page shell. Every rule is expressed through host
// variables with a neutral fallback so the page also renders standalone when a
// developer opens the route directly in a browser.
const documentTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="{{.Refresh}}">
{{end}}<title>{{.Title}}</title>
<style>
:root {
  --ui-gap: 16px;
  --ui-radius: var(--app-radius-md, 10px);
  --ui-font: var(--cpamp-plugin-font-family, Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif);
}
html, body { margin: 0; min-height: 100%; }
body {
  background: var(--bg-primary, #ffffff);
  color: var(--text-primary, #1f2328);
  font-family: var(--ui-font);
  font-size: 14px;
  line-height: 1.5;
  padding: var(--ui-gap);
}
.wrap { max-width: 720px; margin: 0 auto; display: flex; flex-direction: column; gap: var(--ui-gap); }
.head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.head h1 { font-size: 16px; font-weight: 600; margin: 0; letter-spacing: 0; }
.card {
  background: var(--app-surface, var(--bg-secondary, #f6f8fa));
  border: 1px solid var(--app-border, var(--border-color, #d8dee4));
  border-radius: var(--ui-radius);
  padding: 14px 16px;
}
.card > h2 { font-size: 14px; font-weight: 600; margin: 0 0 10px; color: var(--text-primary, #1f2328); }
.card > h2:last-child { margin-bottom: 0; }
.fields { display: grid; grid-template-columns: max-content 1fr; gap: 6px 14px; margin: 0; }
.fields dt { color: var(--text-secondary, #59636e); }
.fields dd { margin: 0; color: var(--text-primary, #1f2328); word-break: break-all; }
.actions { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 12px; }
.btn, button {
  font: inherit;
  cursor: pointer;
  border-radius: var(--app-radius-sm, 6px);
  border: 1px solid var(--app-border, var(--border-color, #d8dee4));
  background: var(--app-input-bg, var(--bg-tertiary, #f6f8fa));
  color: var(--text-primary, #1f2328);
  padding: 6px 14px;
  display: inline-block;
  text-decoration: none;
  line-height: 1.4;
}
.btn:hover, button:hover { background: var(--bg-hover, #eef1f4); border-color: var(--border-hover, #c9d1d9); }
.btn.primary, button.primary {
  background: var(--primary-color, #1f6feb);
  border-color: var(--primary-color, #1f6feb);
  color: var(--primary-contrast, #ffffff);
}
.btn.primary:hover, button.primary:hover { background: var(--primary-hover, #1a5fd0); border-color: var(--primary-hover, #1a5fd0); }
button:disabled { opacity: .55; cursor: default; }
a { color: var(--primary-color, #1f6feb); }
.notice {
  border-radius: var(--ui-radius);
  border: 1px solid var(--app-border, var(--border-color, #d8dee4));
  padding: 10px 14px;
}
.notice.success { border-color: var(--success-color, #1a7f37); color: var(--success-color, #1a7f37); }
.notice.warning { border-color: var(--warning-color, #9a6700); color: var(--warning-color, #9a6700); }
.notice.danger  { border-color: var(--danger-color, #cf222e);  color: var(--danger-color, #cf222e); }
/* qr: an inline data: URL image keeps its own white quiet zone, which a dark
   host theme would otherwise swallow. */
.qr { display: block; margin: 6px auto; padding: 8px; background: #ffffff; border-radius: var(--ui-radius); }
.qr-missing { text-align: center; color: var(--text-secondary, #59636e); padding: 24px 0; }
.badge {
  display: inline-block;
  border-radius: 999px;
  padding: 1px 9px;
  font-size: 12px;
  border: 1px solid var(--border-color, #d8dee4);
  color: var(--text-secondary, #59636e);
}
.badge.success { border-color: var(--success-color, #1a7f37); color: var(--success-color, #1a7f37); }
.badge.warning { border-color: var(--warning-color, #9a6700); color: var(--warning-color, #9a6700); }
.badge.danger  { border-color: var(--danger-color, #cf222e);  color: var(--danger-color, #cf222e); }
.muted { color: var(--text-secondary, #59636e); }
code { background: var(--app-surface-muted, var(--bg-tertiary, #f6f8fa)); padding: 1px 5px; border-radius: 4px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
</style>
</head>
<body>
<div class="wrap">
<div class="head"><h1>{{.Heading}}</h1></div>
{{.Body}}
</div>
</body>
</html>
`

// Action is a link that performs a management action by reloading the current
// route with an extra query string.
//
// Actions are navigations rather than form submissions on purpose: the host
// dispatches the resource route that management clients embed (`/v0/resource/
// plugins/<id>/...`) as GET only, so a POST form would silently never reach the
// plugin from inside the UI.
type Action struct {
	Label string
	// Path is an optional relative target route, for example "status". When it
	// is empty the action targets the current route.
	Path string
	// Query is appended to the target, for example "action=checkin".
	Query string
	// Kind is "" for a neutral button, "primary" for the accented one.
	Kind string
}

// AddAccountQuery is the query a status page appends to the login link when the
// user asked for an ADDITIONAL account instead of a re-login.
//
// Every provider derives its credential file name from the account's own
// identity (`defaultAuthFileName`), so a second account lands in a second file
// and the first one is left untouched. The query exists only so the login page
// can say that out loud: the login flow itself is the same one, deliberately —
// there is no second code path that could behave differently.
const AddAccountQuery = "add=1"

// AddAccountNotice is the one-line caveat a login page shows when it was
// reached through AddAccountQuery. It is the only place the distinction is
// explained; the action itself is labelled 新建账号.
const AddAccountNotice = "本次登录用于新增账号，已有账号的凭据不受影响。"

// IsAddAccountRequest reports whether a page was opened to add another account.
func IsAddAccountRequest(request pluginapi.ManagementRequest) bool {
	switch strings.ToLower(strings.TrimSpace(request.Query.Get("add"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Field is one label/value row.
type Field struct {
	Label string
	Value string
}

var (
	documentTmpl = template.Must(template.New("document").Parse(documentTemplate))
	cardTmpl     = template.Must(template.New("card").Parse(
		`<section class="card">{{if .Title}}<h2>{{.Title}}</h2>{{end}}{{.Body}}{{if .Actions}}` +
			`<div class="actions">{{range .Actions}}` +
			`<a class="btn{{if .Kind}} {{.Kind}}{{end}}" href="{{.Href}}">{{.Label}}</a>` +
			`{{end}}</div>{{end}}</section>`))
	fieldsTmpl = template.Must(template.New("fields").Parse(
		`<dl class="fields">{{range .}}<dt>{{.Label}}</dt><dd>{{.Value}}</dd>{{end}}</dl>`))
	noticeTmpl = template.Must(template.New("notice").Parse(
		`<div class="notice{{if .Tone}} {{.Tone}}{{end}}">{{.Message}}</div>`))
	badgeTmpl = template.Must(template.New("badge").Parse(
		`<span class="badge{{if .Tone}} {{.Tone}}{{end}}">{{.Label}}</span>`))
)

func render(tmpl *template.Template, data any) template.HTML {
	var buffer bytes.Buffer
	// Templates are compiled from constants and only ever receive escaped
	// values, so an execution error cannot leave a partially trusted page.
	if errExecute := tmpl.Execute(&buffer, data); errExecute != nil {
		return template.HTML(template.HTMLEscapeString(buffer.String()))
	}
	return template.HTML(buffer.String())
}

// Document renders a complete page. body is composed from the helpers below.
func Document(heading string, body ...template.HTML) []byte {
	return renderDocument(0, heading, body)
}

// DocumentWithRefresh renders a page that reloads itself every seconds seconds.
//
// A resource route is dispatched as GET only, so a page that has to follow a
// long-running flow has exactly two form-free ways to ask for the next step: a
// link the user clicks, or `<meta http-equiv="refresh">`. The second one is what
// a QR page uses — each load performs one server-side poll and re-renders, with
// no form and no JavaScript involved.
func DocumentWithRefresh(seconds int, heading string, body ...template.HTML) []byte {
	return renderDocument(seconds, heading, body)
}

// renderDocument is the shared shell renderer; seconds <= 0 omits the refresh.
func renderDocument(seconds int, heading string, body []template.HTML) []byte {
	var buffer bytes.Buffer
	refresh := 0
	if seconds > 0 {
		refresh = seconds
	}
	data := struct {
		Title   string
		Heading string
		Refresh int
		Body    template.HTML
	}{Title: heading, Heading: heading, Refresh: refresh, Body: template.HTML(join(body))}
	if errExecute := documentTmpl.Execute(&buffer, data); errExecute != nil {
		return []byte("<!DOCTYPE html><html><body>failed to render page</body></html>")
	}
	return buffer.Bytes()
}

// HTML renders a page and wraps it as a management API response.
func HTML(heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       Document(heading, body...),
	}
}

// HTMLWithRefresh renders a self-refreshing page as a management API response.
func HTMLWithRefresh(seconds int, heading string, body ...template.HTML) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       DocumentWithRefresh(seconds, heading, body...),
	}
}

// Card renders a titled section with optional action links.
func Card(title string, body template.HTML, actions ...Action) template.HTML {
	rendered := make([]cardAction, 0, len(actions))
	for _, action := range actions {
		rendered = append(rendered, cardAction{
			Label: action.Label,
			Href:  actionHref(action.Path, action.Query),
			Kind:  action.Kind,
		})
	}
	return render(cardTmpl, struct {
		Title   string
		Body    template.HTML
		Actions []cardAction
	}{Title: title, Body: body, Actions: rendered})
}

// cardAction is an Action with its href pre-computed and marked safe.
type cardAction struct {
	Label string
	Href  template.URL
	Kind  string
}

// actionHref builds an action target from a relative path and a query string.
//
// The query is re-encoded through url.Values so the result is well-formed, then
// handed to the template as a template.URL. Without that, Go's URL filter
// percent-encodes the '=' inside the attribute and the host would receive the
// whole action as a single key with no value.
func actionHref(path, query string) template.URL {
	values, errParse := url.ParseQuery(strings.TrimPrefix(query, "?"))
	if errParse != nil {
		return template.URL("?")
	}
	encoded := values.Encode()
	target := strings.TrimSpace(path)
	switch {
	case target == "":
		return template.URL("?" + encoded)
	case encoded == "":
		return template.URL(target)
	default:
		return template.URL(target + "?" + encoded)
	}
}

// Group concatenates several body fragments into the single body a Card takes.
func Group(fragments ...template.HTML) template.HTML {
	return template.HTML(join(fragments))
}

// Fields renders label/value rows.
func Fields(pairs ...Field) template.HTML {
	return render(fieldsTmpl, pairs)
}

// Notice renders an inline message. tone is "", "success", "warning" or "danger".
func Notice(tone, message string) template.HTML {
	return render(noticeTmpl, struct{ Tone, Message string }{tone, message})
}

// Badge renders a small status pill.
func Badge(tone, label string) template.HTML {
	return render(badgeTmpl, struct{ Tone, Label string }{tone, label})
}

// dataImagePattern is the only shape Image accepts: a base64 image data URL.
// Anything else is refused rather than escaped, because a page must never render
// a URL a caller built out of upstream bytes.
var dataImagePattern = regexp.MustCompile(`^data:image/(?:png|jpeg|gif);base64,[A-Za-z0-9+/]+={0,2}$`)

// Image renders a base64 data: URL image inline, so the page needs no second
// request and no external host.
//
// size is the rendered square edge in CSS pixels (a QR needs enough of them to
// stay scannable); a non-positive value falls back to 220. A value that is not a
// base64 image data URL renders a readable placeholder instead of a broken
// image, which is what an upstream error page would otherwise look like.
func Image(dataURL, alt string, size int) template.HTML {
	if size <= 0 {
		size = 220
	}
	if !dataImagePattern.MatchString(dataURL) {
		return template.HTML(`<div class="qr-missing">` + template.HTMLEscapeString(alt) + `：图片不可用</div>`)
	}
	// The pattern above admits only base64 characters, so the data URL cannot
	// break out of the attribute; the alt text is escaped as usual.
	return template.HTML(`<img class="qr" src="` + dataURL + `" alt="` +
		template.HTMLEscapeString(alt) + `" width="` + strconv.Itoa(size) + `" height="` + strconv.Itoa(size) + `">`)
}

// join concatenates fragments without letting html/template re-escape the
// already-rendered markup.
func join(fragments []template.HTML) string {
	var buffer bytes.Buffer
	for _, fragment := range fragments {
		buffer.WriteString(string(fragment))
	}
	return buffer.String()
}
