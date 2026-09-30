package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Identity-block structure tests.
//
// These lock the ONE thing the `3012` gate turns on. The reference measured that
// sending the same ~2900 characters as a SINGLE concatenated block still returned
// `3012` while separate blocks returned `200`, so "the content is there" is not
// the property under test — the STRUCTURE is.
//
// ⚠ None of these tests touches the network. That is deliberate and permanent:
// `3012` carries an account penalty (30 minutes, then 24 hours from the third
// occurrence within 24 hours, disablement on the fifth), so the gate can only ever
// be verified by construction here.

// blockTexts decodes a system-block array into its texts, failing the test when
// the shape is not an array of text blocks.
func blockTexts(t *testing.T, blocks []textBlock) []string {
	t.Helper()
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			t.Fatalf("block type = %q, want %q", block.Type, "text")
		}
		out = append(out, block.Text)
	}
	return out
}

// TestIdentityConstantsMatchTheReference pins the published sizes. The reference
// self-checks `cliPrefix + stable` at 2900 characters (rounded); the exact sum is
// 2898, and a drift here means the extracted text no longer matches
// `zcode-identity.ts`.
func TestIdentityConstantsMatchTheReference(t *testing.T) {
	// ⚠ The published numbers are CHARACTER counts (the reference reads
	// JavaScript `String.length`, and the extracted table was cross-checked with a
	// code-point count). Go's `len()` counts BYTES, and these constants contain
	// multi-byte characters — two em dashes alone account for four extra bytes in
	// section 0 — so comparing raw lengths would report a drift that is not there.
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"officialCLIPrefix", utf8.RuneCountInString(officialCLIPrefix), 42},
		{"officialStableSection0", utf8.RuneCountInString(officialStableSection0), 1211},
		{"officialStableSection1", utf8.RuneCountInString(officialStableSection1), 1100},
		{"officialStableSection2", utf8.RuneCountInString(officialStableSection2), 541},
		{"stable joined", utf8.RuneCountInString(officialStableSections()), 2856},
		{"cliPrefix + stable",
			utf8.RuneCountInString(officialCLIPrefix) + utf8.RuneCountInString(officialStableSections()),
			officialIdentityChars},
		{"contextPrefixIntro", utf8.RuneCountInString(contextPrefixIntro), 70},
		{"contextPrefixOutro", utf8.RuneCountInString(contextPrefixOutro), 153},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s length = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// The multi-byte characters must survive verbatim: an em dash is not a hyphen
	// and a smart quote is not an ASCII one, and upstream compares text.
	if !strings.Contains(officialStableSection0, "—") {
		t.Error("the em dash in the stable section was replaced (likely with an ASCII hyphen)")
	}
	if !strings.Contains(officialStableSections(), "don't") {
		t.Error("the apostrophe in \"don't\" was replaced (likely with a typographic one)")
	}
}

// TestIdentityBlockStructureIsOrdered guards the exact ORDER, which is what the
// upstream check looks at: the official prefix must be first, and the caller's own
// system prompt must be LAST.
func TestIdentityBlockStructureIsOrdered(t *testing.T) {
	cases := []struct {
		name         string
		callerSystem string
		wantCount    int
		wantLast     string
	}{
		{
			name:         "no caller system yields exactly three blocks",
			callerSystem: "",
			wantCount:    3,
			wantLast:     "",
		},
		{
			name:         "a whitespace-only caller system is treated as absent",
			callerSystem: "   \n\t ",
			wantCount:    3,
			wantLast:     "",
		},
		{
			name:         "caller system is appended LAST",
			callerSystem: "You are a helpful harness.",
			wantCount:    4,
			wantLast:     "You are a helpful harness.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := buildSystemBlocks(identityOptions{
				CallerSystem: tc.callerSystem,
				Environment:  environmentOptions{CWD: "/work"},
			})
			if len(blocks) != tc.wantCount {
				t.Fatalf("block count = %d, want %d", len(blocks), tc.wantCount)
			}
			texts := blockTexts(t, blocks)
			if texts[0] != officialCLIPrefix {
				t.Errorf("block[0] is not the CLI prefix:\n got %q", texts[0])
			}
			if texts[1] != officialStableSections() {
				t.Errorf("block[1] is not the joined stable sections (%d chars, got %d)",
					len(officialStableSections()), len(texts[1]))
			}
			if !strings.HasPrefix(texts[2], "# Environment") {
				t.Errorf("block[2] is not the environment section:\n got %q", texts[2])
			}
			if tc.wantLast != "" && texts[len(texts)-1] != tc.wantLast {
				t.Errorf("last block = %q, want the caller system %q", texts[len(texts)-1], tc.wantLast)
			}
		})
	}
}

// TestIdentityBlocksAreSeparateNotConcatenated is the structural regression this
// whole file exists for.
//
// The block count must grow with the caller's system, and no block may contain a
// concatenation of the previous one: a single merged blob is exactly the shape
// that was measured to still return `3012`.
func TestIdentityBlocksAreSeparateNotConcatenated(t *testing.T) {
	withCaller := buildSystemBlocks(identityOptions{
		CallerSystem: "HARNESS-RULES",
		Environment:  environmentOptions{CWD: "/work"},
	})
	if len(withCaller) != 4 {
		t.Fatalf("block count = %d, want 4 (cliPrefix, stable, environment, caller)", len(withCaller))
	}
	for index, block := range withCaller {
		if strings.Contains(block.Text, officialCLIPrefix) && index != 0 {
			t.Errorf("block[%d] contains the CLI prefix; the prefix must exist ONLY as its own block", index)
		}
		if index != 1 && strings.Contains(block.Text, officialStableSection0) {
			t.Errorf("block[%d] contains the stable section text; the sections must stay in block[1]", index)
		}
	}
	// A single block holding everything is the failing shape; assert it is NOT
	// what this function produces.
	merged := strings.Join(blockTexts(t, withCaller), "")
	if withCaller[0].Text == merged {
		t.Fatal("all identity text collapsed into block[0]; upstream measured this as a 3012")
	}
}

// TestCacheBreakpointIsOnTheLastBlockOnly guards the prompt-caching placement.
//
// A breakpoint is prefix-shaped, so one at the end covers everything before it —
// and the per-request budget (Anthropic allows four) has to stay free for the tool
// table's own marker.
func TestCacheBreakpointIsOnTheLastBlockOnly(t *testing.T) {
	blocks := buildSystemBlocks(identityOptions{
		CallerSystem:    "RULES",
		Environment:     environmentOptions{CWD: "/work"},
		CacheBreakpoint: true,
	})
	for index, block := range blocks {
		hasMarker := block.CacheControl != nil
		wantMarker := index == len(blocks)-1
		if hasMarker != wantMarker {
			t.Errorf("block[%d] cache_control present = %v, want %v", index, hasMarker, wantMarker)
		}
		if hasMarker && block.CacheControl.Type != "ephemeral" {
			t.Errorf("block[%d] cache_control type = %q, want ephemeral", index, block.CacheControl.Type)
		}
	}
	// Without the switch nothing is marked at all.
	plain := buildSystemBlocks(identityOptions{Environment: environmentOptions{CWD: "/work"}})
	for index, block := range plain {
		if block.CacheControl != nil {
			t.Errorf("block[%d] carries cache_control although the switch is off", index)
		}
	}
}

// TestEnvironmentSectionShape pins the environment block line by line.
//
// Omitting it is how "which model are you?" degrades to a generic "GLM": the model
// is simply not told what it is running as, nor where it runs.
func TestEnvironmentSectionShape(t *testing.T) {
	cases := []struct {
		name     string
		options  environmentOptions
		contains []string
	}{
		{
			name: "windows platform selects powershell",
			options: environmentOptions{
				CWD: "/work", Provider: "zcode", Model: "GLM-5.3", Platform: "win32", OSGroup: "windows",
			},
			contains: []string{
				"# Environment",
				"You have been invoked in the following environment:",
				" - Primary working directory: /work",
				" - Is a git repository: no",
				" - Platform: win32",
				" - Shell: powershell",
				" - OS Version: windows",
				" - You are powered by the model named zcode/GLM-5.3.",
			},
		},
		{
			name: "non-windows platform selects bash and reports a git repository",
			options: environmentOptions{
				CWD: "/srv/app", Provider: "zcode", Model: "GLM-5.3-Flash",
				Platform: "linux", OSGroup: "linux", IsGitRepo: true,
			},
			contains: []string{
				" - Primary working directory: /srv/app",
				" - Is a git repository: yes",
				" - Platform: linux",
				" - Shell: bash",
				" - You are powered by the model named zcode/GLM-5.3-Flash.",
			},
		},
		{
			name:    "empty inputs fall back instead of producing blank lines",
			options: environmentOptions{},
			contains: []string{
				" - Primary working directory: .",
				" - Platform: win32",
				" - Shell: powershell",
				" - You are powered by the model named zcode/glm-5.3-flash.",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			section := buildEnvironmentSection(tc.options)
			for _, want := range tc.contains {
				if !strings.Contains(section, want) {
					t.Errorf("environment section missing %q:\n%s", want, section)
				}
			}
			// The section is exactly eight lines: seven entries and no trailing newline.
			if lines := strings.Split(section, "\n"); len(lines) != 8 {
				t.Errorf("environment section has %d lines, want 8:\n%s", len(lines), section)
			}
		})
	}
}

// TestFormatLocalISODateUsesTheLocalCalendarDay guards the date helper's
// timezone: the reference formats the LOCAL date, not UTC, and the two differ for
// a third of the world for part of every day.
func TestFormatLocalISODateUsesTheLocalCalendarDay(t *testing.T) {
	moment := time.Date(2026, time.October, 2, 23, 30, 0, 0, time.Local)
	if got := formatLocalISODate(moment); got != "2026-10-02" {
		t.Fatalf("formatLocalISODate = %q, want 2026-10-02", got)
	}
	// Single-digit month and day are zero padded, which is what the ISO shape needs.
	single := time.Date(2026, time.January, 5, 0, 0, 0, 0, time.Local)
	if got := formatLocalISODate(single); got != "2026-01-05" {
		t.Fatalf("formatLocalISODate = %q, want 2026-01-05", got)
	}
}

// TestContextPrefixBlockIsByteExact pins the `<system-reminder>` block's wording.
//
// ⚠ The SIX SPACES before the outro are load-bearing, and so is the blank line
// between the date and the outro: the reference calls this block "the last switch
// of 3012", and it is compared by upstream as text.
func TestContextPrefixBlockIsByteExact(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.Local)
	block := buildContextPrefixBlock(now)
	if block.Type != "text" {
		t.Fatalf("block type = %q, want text", block.Type)
	}
	want := "<system-reminder>" +
		"As you answer the user's questions, you can use the following context:\n" +
		"# currentDate\nToday's date is 2026-10-02.\n" +
		"\n" +
		"      IMPORTANT: this context may or may not be relevant to your tasks. " +
		"You should not respond to this context unless it is highly relevant to your task." +
		"</system-reminder>"
	if block.Text != want {
		t.Fatalf("context prefix differs from the reference:\n got %q\nwant %q", block.Text, want)
	}
	if !strings.Contains(block.Text, "\n      IMPORTANT:") {
		t.Fatal("the outro's six-space indent was lost")
	}
}

// TestWithContextPrefixPrependsToTheFirstUserMessage covers the insertion rules:
// only the FIRST message, only when it is a user turn, and idempotently.
func TestWithContextPrefixPrependsToTheFirstUserMessage(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.Local)

	t.Run("a string content becomes a block array with the prefix first", func(t *testing.T) {
		messages := []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"hello"}`),
			json.RawMessage(`{"role":"assistant","content":"hi"}`),
		}
		out := withContextPrefix(messages, now)
		var first struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if errUnmarshal := json.Unmarshal(out[0], &first); errUnmarshal != nil {
			t.Fatalf("decode first message: %v", errUnmarshal)
		}
		if len(first.Content) != 2 {
			t.Fatalf("first message has %d blocks, want 2 (prefix + original text)", len(first.Content))
		}
		if !strings.HasPrefix(first.Content[0].Text, "<system-reminder>") {
			t.Errorf("block[0] is not the date block: %q", first.Content[0].Text)
		}
		if first.Content[1].Text != "hello" {
			t.Errorf("block[1] = %q, want the original text", first.Content[1].Text)
		}
		// The SECOND message is untouched.
		if string(out[1]) != string(messages[1]) {
			t.Errorf("the second message was modified: %s", out[1])
		}
	})

	t.Run("an existing block array is prefixed, not replaced", func(t *testing.T) {
		messages := []json.RawMessage{
			json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}`),
		}
		out := withContextPrefix(messages, now)
		var first struct {
			Content []json.RawMessage `json:"content"`
		}
		if errUnmarshal := json.Unmarshal(out[0], &first); errUnmarshal != nil {
			t.Fatalf("decode: %v", errUnmarshal)
		}
		if len(first.Content) != 3 {
			t.Fatalf("blocks = %d, want 3 (prefix + 2 original)", len(first.Content))
		}
		if !strings.Contains(string(first.Content[2]), "image") {
			t.Errorf("the image block was lost: %s", first.Content[2])
		}
	})

	t.Run("an assistant first message is left alone", func(t *testing.T) {
		messages := []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"prefill"}`)}
		out := withContextPrefix(messages, now)
		if string(out[0]) != string(messages[0]) {
			t.Errorf("an assistant-first conversation was modified: %s", out[0])
		}
	})

	t.Run("the operation is idempotent", func(t *testing.T) {
		messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)}
		once := withContextPrefix(messages, now)
		twice := withContextPrefix(once, now)
		if string(twice[0]) != string(once[0]) {
			t.Fatalf("a second pass changed the message:\n once %s\ntwice %s", once[0], twice[0])
		}
	})

	t.Run("content already carrying a reminder string is not prefixed", func(t *testing.T) {
		messages := []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"<system-reminder>already</system-reminder>"}`),
		}
		out := withContextPrefix(messages, now)
		if string(out[0]) != string(messages[0]) {
			t.Errorf("a already-prefixed message was modified: %s", out[0])
		}
	})

	t.Run("an empty message list is returned unchanged", func(t *testing.T) {
		if out := withContextPrefix(nil, now); len(out) != 0 {
			t.Fatalf("len = %d, want 0", len(out))
		}
	})
}

// TestStartsWithSystemReminderHandlesBothShapes covers the idempotency probe.
func TestStartsWithSystemReminderHandlesBothShapes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"string with the prefix", `"<system-reminder>x"`, true},
		{"string without it", `"hello"`, false},
		{"block array with the prefix first", `[{"type":"text","text":"<system-reminder>x"}]`, true},
		{"block array with plain text", `[{"type":"text","text":"hello"}]`, false},
		{"empty block array", `[]`, false},
		{"null", `null`, false},
		{"absent", ``, false},
		{"not JSON", `plain`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startsWithSystemReminder(json.RawMessage(tc.content)); got != tc.want {
				t.Fatalf("startsWithSystemReminder(%s) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}
