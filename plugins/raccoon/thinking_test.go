package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Thinking control (upstream `05873c1`).
//
// ⚠️ This provider has exactly TWO levels and one effective channel:
// `extra_body.thinking.type`. The tests below pin BOTH halves of the defect —
// the declaration (`info()`), because without it no selector is offered at all,
// and the wire, because a declaration the request never uses looks perfectly
// green in a unit test of the pure mapping function.

// chatBodyFor builds an outbound body for one payload and decodes it.
func chatBodyFor(t *testing.T, payload string) map[string]any {
	t.Helper()
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Model: "sn-kimi-k3", Payload: []byte(payload)})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("decode body: %v", errUnmarshal)
	}
	return decoded
}

// userPayload renders a minimal request body carrying one extra top-level field.
func userPayload(extra string) string {
	suffix := ""
	if extra != "" {
		suffix = "," + extra
	}
	return `{"model":"sn-kimi-k3","messages":[{"role":"user","content":"hi"}]` + suffix + `}`
}

// TestThinkingExtraBodyOnlyTheTwoMeasuredStates pins the mapping itself: `off`
// disables, `on` enables, and nothing is sent without a level.
func TestThinkingExtraBodyOnlyTheTwoMeasuredStates(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		want   string
	}{
		{"off disables thinking", EffortOff, ThinkingTypeDisabled},
		{"on enables thinking", EffortOn, ThinkingTypeEnabled},
		{"an unknown level is treated as on", "high", ThinkingTypeEnabled},
		{"a nonsense level is treated as on", "banana", ThinkingTypeEnabled},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			extra := thinkingExtraBody(testCase.effort)
			if extra == nil {
				t.Fatalf("thinkingExtraBody(%q) = nil, want a thinking object", testCase.effort)
			}
			encoded, errMarshal := json.Marshal(extra)
			if errMarshal != nil {
				t.Fatalf("marshal: %v", errMarshal)
			}
			want := `{"thinking":{"type":"` + testCase.want + `"}}`
			if string(encoded) != want {
				t.Fatalf("extra_body = %s, want %s", encoded, want)
			}
			// ⚠️ `reasoning_effort` and `enable_thinking` were both measured
			// inert; emitting either would be a silent no-op that still looks
			// like a working control.
			if strings.Contains(string(encoded), "reasoning_effort") ||
				strings.Contains(string(encoded), "enable_thinking") {
				t.Fatalf("an inert field was emitted: %s", encoded)
			}
			// ⚠️ Single level only: the double-nested LiteLLM form was measured
			// NOT to work.
			if _, doubled := extra["extra_body"]; doubled {
				t.Fatalf("double-nested extra_body was emitted: %s", encoded)
			}
		})
	}
	if extra := thinkingExtraBody(""); extra != nil {
		t.Fatalf("an absent level must emit no extra_body, got %#v", extra)
	}
}

// TestChatBodyWritesTheThinkingDialectNested pins the wire half: the field must
// be inside `extra_body`, because this server IGNORES a top-level `thinking`
// (it does not even reject an illegal value there).
func TestChatBodyWritesTheThinkingDialectNested(t *testing.T) {
	decoded := chatBodyFor(t, userPayload(`"reasoning_effort":"off"`))
	extra, okExtra := decoded["extra_body"].(map[string]any)
	if !okExtra {
		t.Fatalf("no extra_body in the body: %#v", decoded)
	}
	thinking, okThinking := extra["thinking"].(map[string]any)
	if !okThinking {
		t.Fatalf("extra_body carries no thinking: %#v", extra)
	}
	if thinking["type"] != ThinkingTypeDisabled {
		t.Fatalf("extra_body.thinking.type = %#v, want %q", thinking["type"], ThinkingTypeDisabled)
	}
	if _, topLevel := decoded["thinking"]; topLevel {
		t.Fatal("thinking must NOT be at the top level: this gateway silently ignores it there")
	}
	if _, present := decoded["reasoning_effort"]; present {
		t.Fatal("reasoning_effort must not be sent: it is measured inert on this gateway")
	}
}

// TestChatBodyThinkingLevels drives the four cases the reference measured, one
// per client-supplied level.
func TestChatBodyThinkingLevels(t *testing.T) {
	cases := []struct {
		name       string
		extra      string
		wantType   string
		wantAbsent bool
	}{
		{"off → disabled", `"reasoning_effort":"off"`, ThinkingTypeDisabled, false},
		{"on → enabled", `"reasoning_effort":"on"`, ThinkingTypeEnabled, false},
		{"no level → the field is absent", "", "", true},
		{"a whitespace-only level → the field is absent", `"reasoning_effort":"   "`, "", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded := chatBodyFor(t, userPayload(testCase.extra))
			extra, present := decoded["extra_body"]
			if testCase.wantAbsent {
				if present {
					t.Fatalf("extra_body must be absent, got %#v", extra)
				}
				return
			}
			if !present {
				t.Fatalf("extra_body is missing: %#v", decoded)
			}
			nested, _ := extra.(map[string]any)["thinking"].(map[string]any)
			if nested["type"] != testCase.wantType {
				t.Fatalf("thinking.type = %#v, want %q", nested["type"], testCase.wantType)
			}
		})
	}
}

// TestReadReasoningEffortFieldNames pins where the client's choice is read from.
//
// ⚠️ The evidence for `reasoning_effort` is not a guess. The host forwards the
// client's OpenAI chat-completions body verbatim as `Payload`, and:
//   - the sibling port reads the very same key out of that payload
//     (`plugins/cline/adapter.go:133`);
//   - the shared request type declares it
//     (`internal/jethub/openai/openai.go:60`);
//   - the CPA host itself extracts it under this name
//     (`sdk/api/handlers/handlers.go:318`, key
//     `cliproxyexecutor.ReasoningEffortMetadataKey = "reasoning_effort"`).
func TestReadReasoningEffortFieldNames(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"the OpenAI top-level field", userPayload(`"reasoning_effort":"off"`), EffortOff},
		{"a non-string level is ignored", userPayload(`"reasoning_effort":42`), ""},
		{"a null level is ignored", userPayload(`"reasoning_effort":null`), ""},
		{"an absent level", userPayload(""), ""},
		// The DSH-native block form, accepted as a fallback: the host's thinking
		// applier is not called on the plugin executor path.
		{"the native reasoning block", userPayload(`"reasoning":{"effort":"off"}`), EffortOff},
		{"the native reasoning level key", userPayload(`"reasoning":{"level":"off"}`), EffortOff},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var root map[string]any
			if errUnmarshal := json.Unmarshal([]byte(testCase.payload), &root); errUnmarshal != nil {
				t.Fatalf("decode payload: %v", errUnmarshal)
			}
			if got := readReasoningEffort(root); got != testCase.want {
				t.Fatalf("readReasoningEffort = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestInfoDeclaresTwoThinkingLevels pins the declaration half of the defect:
// with no `Thinking` block the composer shows "no reasoning levels for this
// model" and the user cannot choose at all.
func TestInfoDeclaresTwoThinkingLevels(t *testing.T) {
	now := time.Now()
	for _, entry := range fallbackModels() {
		info := entry.info(now)
		if info.Thinking == nil {
			t.Fatalf("model %s declares no Thinking support", entry.ID)
		}
		// ⚠️ Exactly two, in the reference's display order.
		if len(info.Thinking.Levels) != 2 {
			t.Fatalf("model %s declares %d levels, want 2: %#v", entry.ID, len(info.Thinking.Levels), info.Thinking.Levels)
		}
		if info.Thinking.Levels[0] != EffortOn || info.Thinking.Levels[1] != EffortOff {
			t.Fatalf("model %s levels = %#v, want [on off] in that order", entry.ID, info.Thinking.Levels)
		}
		// ⚠️ `off` is measured to work (zero reasoning tokens in 6/6 and 8/8
		// runs), so disabling reasoning must be advertised as supported.
		if !info.Thinking.ZeroAllowed {
			t.Fatalf("model %s does not allow zero reasoning, but `off` is measured to work", entry.ID)
		}
		// No strength level is offered: `reasoning_effort` is inert here, so
		// offering `high` would promise depth the provider cannot deliver.
		for _, level := range info.Thinking.Levels {
			if level == "high" || level == "max" || level == "minimal" {
				t.Fatalf("model %s offers the inert strength level %q", entry.ID, level)
			}
		}
	}
}

// TestThinkingLevelNamesAreTheDeclaredDisplayNames pins the display names the
// reference chose. "开启 / 关闭" rather than "深度思考": only the boolean
// dimension exists, and the stronger wording implies a depth scale that is not
// there (upstream `05873c1`, user request of 2026-09-28).
func TestThinkingLevelNamesAreTheDeclaredDisplayNames(t *testing.T) {
	if ReasoningLevelNames[EffortOn] != "开启" {
		t.Errorf("on level name = %q, want 开启", ReasoningLevelNames[EffortOn])
	}
	if ReasoningLevelNames[EffortOff] != "关闭" {
		t.Errorf("off level name = %q, want 关闭", ReasoningLevelNames[EffortOff])
	}
	// The declared default must be one of the declared levels, or the host
	// rejects the model's own reasoning configuration.
	found := false
	for _, level := range ReasoningEfforts {
		if level == DefaultEffort {
			found = true
		}
	}
	if !found {
		t.Fatalf("default level %q is not in %#v", DefaultEffort, ReasoningEfforts)
	}
}

// TestDeclaredLevelsReachTheWire is the anti-regression the reference calls out
// explicitly: a pure-function test of the mapping passes even when `stream()`
// forgets to use it ("声明了 reasoning 但 stream 忘了用"). Every declared level
// must therefore produce a real `extra_body` on the wire.
func TestDeclaredLevelsReachTheWire(t *testing.T) {
	info := fallbackCatalogue[0].info(time.Now())
	for _, level := range info.Thinking.Levels {
		t.Run(level, func(t *testing.T) {
			decoded := chatBodyFor(t, userPayload(`"reasoning_effort":"`+level+`"`))
			extra, okExtra := decoded["extra_body"].(map[string]any)
			if !okExtra {
				t.Fatalf("declared level %q produced no extra_body on the wire", level)
			}
			if _, okThinking := extra["thinking"].(map[string]any); !okThinking {
				t.Fatalf("declared level %q produced no extra_body.thinking", level)
			}
		})
	}
}
