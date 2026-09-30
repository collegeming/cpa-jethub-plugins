package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The official ZCode identity block — the ONLY thing that opens the `3012` gate.
//
// ## Why the text is embedded
//
// Upstream inspects the request BODY of the `zcode-plan` channel. When `system`
// lacks the official structure it answers
// `{"code":3012,"msg":"request has been blocked due to unusual activity."}` —
// measured, and independent of HTTP headers and of the runtime that sent it
// (curl behaves like Electron). The measurement matrix from the reference
// (`zcode-identity.ts`):
//
//	| system content                    | chars | result |
//	|-----------------------------------|-------|--------|
//	| none                              |     0 | 3012   |
//	| cliPrefix only                    |    42 | 3012   |
//	| cliPrefix + stable  (first two)   |  2898 | 200    |
//	| all four blocks (with the dynamic |  7599 | 200    |
//	| section)                          |       |        |
//
// ⇒ the criterion is that the identity block EXISTS, not how many blocks follow.
//
// ⚠ **STRUCTURE MATTERS, NOT ONLY CONTENT.** Sending the same ~2900 characters
// as ONE concatenated block still returned `3012` in a direct check; splitting
// them into separate blocks returned `200`. Do not "simplify" this into a single
// string.
//
// ## ⚠ Maintenance warning
//
// Upstream policy is strongly coupled to this structure. When the official client
// changes its identity block, this file has to follow — and a mismatch re-triggers
// `3012`, which carries an ACCOUNT PENALTY: 30 minutes on the first occurrence,
// 24 hours from the third within 24 hours, and account disablement on the fifth.
// **Never write a test that hits the live endpoint**, and never retry a `3012`.
//
// The text itself is programmatically extracted from the reference
// (`src/zcode-identity.ts`, which in turn took it from the official 3.11.2
// bundle) and lives in identity_constants.go, byte for byte.

// textBlock is one Anthropic text content block.
//
// `cache_control` is written on the LAST block only. Anthropic's prompt caching
// is prefix-shaped, so a single breakpoint at the end covers everything before
// it — and the request may carry at most four breakpoints in total, a budget the
// tool table also needs (see withToolCacheBreakpoint).
type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl is the ephemeral prompt-caching marker.
type cacheControl struct {
	Type string `json:"type"`
}

// environmentOptions describes the `# Environment` section.
//
// The reference declares `cwd` mandatory because the official client never sends
// "unknown" for it; the section is not optional either — without it the model
// answers "GLM" to "which model are you", guesses relative paths, and assumes
// bash on a Windows host.
type environmentOptions struct {
	CWD      string
	Provider string
	Model    string
	Platform string
	OSGroup  string
	// IsGitRepo is resolved by the caller, which owns filesystem access.
	IsGitRepo bool
}

// identityOptions are the inputs of buildSystemBlocks.
type identityOptions struct {
	// CallerSystem is the caller's own system prompt. It is appended LAST: the
	// official identity block must stay at the head of the array, because the
	// upstream check looks at the prefix structure.
	CallerSystem string
	// Environment supplies the third block.
	Environment environmentOptions
	// CacheBreakpoint puts `cache_control` on the final block.
	CacheBreakpoint bool
}

// buildSystemBlocks assembles the official system array.
//
// The shape is reproduced exactly; do not "optimise" it:
//
//	block[0] = officialCLIPrefix    (42 characters)   ← required
//	block[1] = the stable sections  (2856 characters) ← required
//	block[2] = "# Environment" section
//	block[3] = the caller's system  (only when non-empty, ALWAYS last)
func buildSystemBlocks(options identityOptions) []textBlock {
	blocks := make([]textBlock, 0, 4)
	blocks = append(blocks, textBlock{Type: "text", Text: officialCLIPrefix})
	blocks = append(blocks, textBlock{Type: "text", Text: officialStableSections()})
	blocks = append(blocks, textBlock{Type: "text", Text: buildEnvironmentSection(options.Environment)})
	if caller := options.CallerSystem; strings.TrimSpace(caller) != "" {
		blocks = append(blocks, textBlock{Type: "text", Text: caller})
	}
	if options.CacheBreakpoint {
		blocks[len(blocks)-1].CacheControl = &cacheControl{Type: "ephemeral"}
	}
	return blocks
}

// officialStableSections joins the published sections with a blank line, which
// is the reference's own `OFFICIAL_STABLE_SECTIONS.join('\n\n')`.
func officialStableSections() string {
	return strings.Join([]string{
		officialStableSection0,
		officialStableSection1,
		officialStableSection2,
	}, "\n\n")
}

// buildEnvironmentSection renders the `# Environment` block.
//
// Every line is load-bearing: the model is told its working directory, whether
// that directory is a git repository, the platform, the shell, the OS version
// and the model it is running as. Omitting it is how the answer to "which model
// are you?" degrades to a generic "GLM".
func buildEnvironmentSection(options environmentOptions) string {
	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		cwd = "."
	}
	provider := strings.TrimSpace(options.Provider)
	if provider == "" {
		provider = ProviderKey
	}
	model := strings.TrimSpace(options.Model)
	if model == "" {
		model = "glm-5.3-flash"
	}
	platform := strings.TrimSpace(options.Platform)
	if platform == "" {
		platform = DefaultPlatform
	}
	shell := "bash"
	if platform == "win32" {
		shell = "powershell"
	}
	osVersion := strings.TrimSpace(options.OSGroup)
	if osVersion == "" {
		osVersion = platform
	}
	gitRepository := "no"
	if options.IsGitRepo {
		gitRepository = "yes"
	}
	return strings.Join([]string{
		"# Environment",
		"You have been invoked in the following environment:",
		" - Primary working directory: " + cwd,
		" - Is a git repository: " + gitRepository,
		" - Platform: " + platform,
		" - Shell: " + shell,
		" - OS Version: " + osVersion,
		" - You are powered by the model named " + provider + "/" + model + ".",
	}, "\n")
}

// isGitRepository reports whether the directory holds a `.git` entry.
//
// Only presence is checked, exactly as the reference does: a cheap, side-effect
// free test is enough for a line of prose.
func isGitRepository(cwd string) bool {
	trimmed := strings.TrimSpace(cwd)
	if trimmed == "" {
		return false
	}
	info, errStat := os.Stat(filepath.Join(trimmed, ".git"))
	return errStat == nil && info != nil
}

// isoDateOf renders a `time.Time` as the local calendar date, which is the
// helper formatLocalISODate implements inline.

// formatLocalISODate renders the LOCAL calendar date.
//
// The reference uses the local date, not UTC, and the difference is visible to
// the model on the days around midnight.
func formatLocalISODate(moment time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d", moment.Year(), int(moment.Month()), moment.Day())
}

// buildContextPrefixBlock renders the `<system-reminder>` date block.
//
// Three details are reproduced verbatim because they are load-bearing:
//
//   - the whole block is ONE `{type:"text"}` inserted at the FRONT of the
//     `content` ARRAY — concatenating it into a text string changes the shape and
//     is still read as a bare request;
//   - `contextPrefixOutro` keeps its six leading spaces;
//   - the blank line comes from the empty element of the join.
func buildContextPrefixBlock(now time.Time) textBlock {
	body := strings.Join([]string{
		contextPrefixIntro,
		"# currentDate\nToday's date is " + formatLocalISODate(now) + ".",
		"",
		contextPrefixOutro,
	}, "\n")
	return textBlock{Type: "text", Text: "<system-reminder>" + body + "</system-reminder>"}
}

// wireMessage is one Anthropic message with its content left raw, because the
// content may be a string or an array of blocks and both spellings reach here.
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// startsWithSystemReminder reports whether a content value has already been
// prefixed, which is what makes withContextPrefix idempotent.
func startsWithSystemReminder(content json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &text); errUnmarshal == nil {
			return strings.HasPrefix(strings.TrimSpace(text), "<system-reminder>")
		}
		return false
	}
	var blocks []textBlock
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &blocks); errUnmarshal != nil {
		return false
	}
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		return strings.HasPrefix(strings.TrimSpace(block.Text), "<system-reminder>")
	}
	return false
}

// withContextPrefix inserts the date block at the front of the FIRST user
// message's content array.
//
// Rules, all from the reference:
//
//   - only the first message is considered, and only when its role is `user`;
//   - the operation is idempotent: a content already starting with
//     `<system-reminder>` is left alone;
//   - plain-string content becomes a block ARRAY, which is the shape the official
//     client sends.
//
// The bridge source calls this "the last switch of 3012": the official client
// always prefixes the first user turn, and a bare request is judged on exactly
// that.
func withContextPrefix(messages []json.RawMessage, now time.Time) []json.RawMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]json.RawMessage, len(messages))
	copy(out, messages)

	var first wireMessage
	if errUnmarshal := json.Unmarshal(out[0], &first); errUnmarshal != nil {
		return out
	}
	if !strings.EqualFold(strings.TrimSpace(first.Role), "user") {
		return out
	}
	if startsWithSystemReminder(first.Content) {
		return out
	}

	prefix := buildContextPrefixBlock(now)
	content := make([]json.RawMessage, 0, 4)
	if encoded, errMarshal := json.Marshal(prefix); errMarshal == nil {
		content = append(content, encoded)
	}

	trimmed := strings.TrimSpace(string(first.Content))
	switch {
	case trimmed == "" || trimmed == "null":
		// No content at all: the prefix is the content.
	case strings.HasPrefix(trimmed, `"`):
		var text string
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &text); errUnmarshal == nil {
			block, errMarshal := json.Marshal(textBlock{Type: "text", Text: text})
			if errMarshal == nil {
				content = append(content, block)
			}
		}
	case strings.HasPrefix(trimmed, "["):
		var blocks []json.RawMessage
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &blocks); errUnmarshal == nil {
			content = append(content, blocks...)
		}
	default:
		content = append(content, first.Content)
	}

	joined, errMarshal := json.Marshal(content)
	if errMarshal != nil {
		return out
	}
	first.Content = joined
	rebuilt, errMarshal := json.Marshal(first)
	if errMarshal != nil {
		return out
	}
	out[0] = rebuilt
	return out
}
