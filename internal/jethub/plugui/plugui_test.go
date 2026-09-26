package plugui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDocumentRendersFragments(t *testing.T) {
	page := Document("CodeArts",
		Card("账号状态", Fields(
			Field{Label: "账号", Value: "probe"},
			Field{Label: "剩余额度", Value: "12.5"},
		), Action{Label: "签到", Query: "action=checkin", Kind: "primary"}),
		Notice("success", "签到成功"),
		Badge("warning", "即将过期"),
	)
	html := string(page)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"<h1>CodeArts</h1>",
		"<h2>账号状态</h2>",
		`<dt>账号</dt><dd>probe</dd>`,
		// Actions must be GET navigations: the host dispatches the route that
		// management clients embed as GET only.
		`href="?action=checkin"`,
		`class="btn primary"`,
		`class="notice success"`,
		`class="badge warning"`,
		// The host theme variables must be consumed, never hard-coded colours.
		"var(--bg-primary",
		"var(--text-primary",
		"var(--primary-color",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if strings.Contains(html, "<form") {
		t.Error("actions must not be form submissions; the embedded resource route is GET-only")
	}
}

// TestDocumentEscapesValues is the injection guard: anything a plugin obtains at
// runtime (account names, upstream error text) must never become markup.
func TestDocumentEscapesValues(t *testing.T) {
	hostile := `<script>alert(1)</script>`
	page := Document("x", Fields(Field{Label: "name", Value: hostile}),
		Notice("danger", hostile), Badge("", hostile))

	if bytes.Contains(page, []byte("<script>alert(1)</script>")) {
		t.Fatal("hostile value was rendered as live markup")
	}
	if !strings.Contains(string(page), "&lt;script&gt;") {
		t.Fatal("hostile value was not escaped at all")
	}
}

func TestHTMLWrapsAsManagementResponse(t *testing.T) {
	response := HTML("CodeArts", Card("状态", Fields(Field{Label: "a", Value: "b"})))
	if response.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Headers["Content-Type"]; len(got) != 1 || got[0] != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %#v, want text/html", got)
	}
	if !bytes.Contains(response.Body, []byte("<!DOCTYPE html>")) {
		t.Fatal("body is not a complete document")
	}
}

// TestIsAddAccountRequest pins the opt-in marker the status page puts on the
// login link and the login page reads back. Anything else is an ordinary login,
// which is what every existing link must stay.
func TestIsAddAccountRequest(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		" 1 ":   true,
		"":      false,
		"0":     false,
		"no":    false,
		"other": false,
	}
	for value, want := range cases {
		request := pluginapi.ManagementRequest{Query: map[string][]string{"add": {value}}}
		if got := IsAddAccountRequest(request); got != want {
			t.Errorf("IsAddAccountRequest(add=%q) = %v, want %v", value, got, want)
		}
	}
	if IsAddAccountRequest(pluginapi.ManagementRequest{}) {
		t.Error("a request without the marker must not be an add-account request")
	}
	// The marker is a query-string link, never a form: it has to survive into the
	// href of the action the status page renders.
	rendered := string(Card("账号", Fields(Field{Label: "a", Value: "b"}),
		Action{Label: "新建账号", Path: "login", Query: AddAccountQuery}))
	if !strings.Contains(rendered, `href="login?add=1"`) {
		t.Fatalf("add-account action href = %s", rendered)
	}
	if !strings.Contains(AddAccountNotice, "已有账号") {
		t.Fatalf("AddAccountNotice = %q, want it to say the existing account survives", AddAccountNotice)
	}
}

// A page that has to follow a long-running flow reloads itself with a meta
// refresh: the resource mount is GET only, so the alternatives are a link the
// user clicks or a form, and a form would never reach the plugin.
func TestDocumentWithRefreshRendersTheMetaTag(t *testing.T) {
	page := string(DocumentWithRefresh(3, "QR", Notice("", "等待扫码")))
	if !strings.Contains(page, `<meta http-equiv="refresh" content="3">`) {
		t.Fatalf("page has no meta refresh: %s", page)
	}
	if strings.Contains(strings.ToLower(page), "<script") {
		t.Fatal("a self-refreshing page must not need JavaScript")
	}
	if strings.Contains(strings.ToLower(page), "<form") {
		t.Fatal("a self-refreshing page must not contain a form")
	}
	// A non-positive interval means "do not reload": that is what a finished flow
	// renders.
	if plain := string(DocumentWithRefresh(0, "QR")); strings.Contains(plain, "http-equiv=\"refresh\"") {
		t.Fatal("DocumentWithRefresh(0) must not reload")
	}
	if plain := string(Document("QR")); strings.Contains(plain, "http-equiv=\"refresh\"") {
		t.Fatal("Document must not reload")
	}
}

// Image inlines a base64 data URL and refuses anything else, so a value built
// from upstream bytes can never become an attribute or markup.
func TestImageRendersOnlyDataURLs(t *testing.T) {
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("qr-bytes"))
	rendered := string(Image(dataURL, "微信二维码", 220))
	if !strings.Contains(rendered, `src="`+dataURL+`"`) {
		t.Fatalf("rendered = %s, want the inline data URL", rendered)
	}
	if !strings.Contains(rendered, `alt="微信二维码"`) || !strings.Contains(rendered, `width="220"`) {
		t.Fatalf("rendered = %s, want the alt text and the size", rendered)
	}
	if !strings.Contains(string(Image(dataURL, "x", 0)), `width="220"`) {
		t.Fatal("a non-positive size must fall back to a scannable default")
	}

	for name, hostile := range map[string]string{
		"attribute escape": `data:image/jpeg;base64,AAAA" onerror="alert(1)`,
		"remote url":       "https://evil.example/qr.png",
		"html":             `<img src=x onerror=alert(1)>`,
		"empty":            "",
	} {
		out := string(Image(hostile, "二维码", 0))
		if strings.Contains(out, "<img") || strings.Contains(out, "onerror") {
			t.Errorf("%s: rendered as markup: %s", name, out)
		}
		if !strings.Contains(out, "图片不可用") {
			t.Errorf("%s: want a readable placeholder instead: %s", name, out)
		}
	}
}
