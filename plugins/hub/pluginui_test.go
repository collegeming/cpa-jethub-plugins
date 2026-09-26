package main

import (
	"strings"
	"testing"
	"time"
)

// TestNoFormInAnyRenderedPage enforces the host's dispatch rule: the resource
// mount is GET only, so a form would silently never reach the plugin. Every
// action must be a query-string link.
func TestNoFormInAnyRenderedPage(t *testing.T) {
	cfg := DefaultConfig()
	run := &runResult{
		BaseURL:    cfg.HostBaseURL,
		StartedAt:  time.Now().Add(-time.Second),
		FinishedAt: time.Now(),
		Providers: []providerRun{
			{
				Target:   targetCatalogue()[5], // qoder
				Accounts: 1,
				Rows: []row{{
					Provider: "qoder", ProviderLabel: "Qoder",
					AuthIndex: "qd-1", Account: "qoder-1.json",
					Result: kindClaimed, ResultText: verdictText(kindClaimed),
					Message: "领取成功", ProviderStatus: "claimed",
				}},
			},
		},
	}

	pages := map[string][]byte{
		"status (no run)":   renderStatusPage(cfg, nil).Body,
		"status (with run)": renderStatusPage(cfg, run).Body,
		"status (disabled)": renderStatusPage(disabledConfig(), nil).Body,
		"result":            renderResultPage(run).Body,
		"disabled":          renderDisabledPage().Body,
		"confirm":           renderConfirmPage().Body,
		"error":             renderErrorPage("未知的 action", "只有 action=checkin 会执行签到。").Body,
	}
	for name, body := range pages {
		markup := strings.ToLower(string(body))
		for _, forbidden := range []string{"<form", "</form", "<input", "<button", `method="post"`, "method=post"} {
			if strings.Contains(markup, forbidden) {
				t.Fatalf("%s page contains %q; a form POST can never reach a resource route", name, forbidden)
			}
		}
		if !strings.Contains(string(body), `class="btn`) {
			t.Fatalf("%s page has no action link", name)
		}
	}
}

func disabledConfig() Config {
	cfg := DefaultConfig()
	cfg.Enabled = false
	return cfg
}

// TestResultPageRendersTheAggregatedTable checks the three columns the feature
// promises: provider, account, result — including the provider's own raw value.
func TestResultPageRendersTheAggregatedTable(t *testing.T) {
	run := &runResult{
		BaseURL:    DefaultHostBaseURL,
		StartedAt:  time.Now().Add(-2 * time.Second),
		FinishedAt: time.Now(),
		Providers: []providerRun{
			{
				Target:   targetCatalogue()[5], // qoder
				Accounts: 2,
				Reported: 2,
				Rows: []row{
					{Provider: "qoder", ProviderLabel: "Qoder", AuthIndex: "qd-1", Account: "qoder-1.json",
						Result: kindClaimed, ResultText: verdictText(kindClaimed), ProviderStatus: "claimed",
						Message: "领取成功；本次 +5"},
					{Provider: "qoder", ProviderLabel: "Qoder", AuthIndex: "qd-2", Account: "qoder-2.json",
						Result: kindAlreadyClaimed, ResultText: verdictText(kindAlreadyClaimed), ProviderStatus: "already-claimed",
						Message: "今天已领取"},
				},
			},
			{
				Target: targetCatalogue()[9], // cline
				Rows: []row{{Provider: "cline", ProviderLabel: "Cline",
					Result: kindUnsupported, ResultText: verdictText(kindUnsupported), Message: "上游没有签到接口"}},
			},
		},
	}

	page := string(renderResultPage(run).Body)
	for _, want := range []string{
		"<th>提供方</th>", "<th>账号</th>", "<th>结果</th>",
		"Qoder（qoder）", "qoder-1.json", "qd-1",
		"已领取", "今日已签到", "领取成功", "今天已领取",
		"provider 原文：claimed",
		"Cline（cline）", "不支持", "上游没有签到接口",
		"共 3 条结果",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("result page is missing %q", want)
		}
	}
}

// TestPagesEscapeProviderSuppliedText: account names and upstream messages come
// from credentials and remote servers, so they must never be rendered as markup.
func TestPagesEscapeProviderSuppliedText(t *testing.T) {
	run := &runResult{
		BaseURL:    DefaultHostBaseURL,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Providers: []providerRun{{
			Target: targetCatalogue()[5],
			Rows: []row{{
				Provider: "qoder", ProviderLabel: "Qoder",
				Account: `<script>alert(1)</script>`,
				Result:  kindFailed, ResultText: verdictText(kindFailed),
				Message: `<img src=x onerror="alert(2)">`,
			}},
		}},
	}
	page := string(renderResultPage(run).Body)
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("account name was not escaped")
	}
	if strings.Contains(page, `onerror="alert(2)"`) {
		t.Fatal("upstream message was not escaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("expected the escaped account name in the table")
	}
}

// TestStatusPageListsEveryTargetIncludingUnsupported: the menu page must name
// what it will drive and must show cline as 不支持 instead of hiding it.
func TestStatusPageListsEveryTargetIncludingUnsupported(t *testing.T) {
	page := string(renderStatusPage(DefaultConfig(), nil).Body)
	for _, entry := range targetCatalogue() {
		if !strings.Contains(page, entry.ID) {
			t.Fatalf("status page does not list %s", entry.ID)
		}
	}
	if !strings.Contains(page, "不支持") {
		t.Fatal("status page does not mark unsupported providers")
	}
	if !strings.Contains(page, "href=\"?action=checkin\"") {
		t.Fatal("status page does not link the one-click action as a GET query string")
	}
	if !strings.Contains(page, "尚未运行") {
		t.Fatal("status page claims a count before any run")
	}
}
