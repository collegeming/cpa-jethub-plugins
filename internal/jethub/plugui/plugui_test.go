package plugui

import (
	"bytes"
	"strings"
	"testing"
)

func TestDocumentRendersFragments(t *testing.T) {
	page := Document("CodeArts",
		Card("账号状态", Fields(
			Field{Label: "账号", Value: "probe"},
			Field{Label: "剩余额度", Value: "12.5"},
		), Action{Label: "签到", Path: "checkin", Kind: "primary"}),
		Notice("success", "签到成功"),
		Badge("warning", "即将过期"),
	)
	html := string(page)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"<h1>CodeArts</h1>",
		"<h2>账号状态</h2>",
		`<dt>账号</dt><dd>probe</dd>`,
		`action="checkin"`,
		`class="primary"`,
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
