package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// This file pins the thinking on/off declaration and its request-body mapping
// (`llm-adapter.ts:831-849`, `:931-938`).
//
// The gateway's ONLY effective thinking control is a TOP-LEVEL
// `thinking:{type:'disabled'}`. Measured 2026-09-29, judged by the
// server-reported `reasoning_tokens`:
//
//	baseline                            76, re-measured 122   (noisy)
//	thinking:{type:'enabled'}           180                  equivalent to sending nothing
//	thinking:{type:'disabled'}          0 (3/3 runs)         genuinely off, answer still correct
//	reasoning_effort low/high/none/minimal  79/58/61/58      inert (`high` was even the lowest)
//	reasoning:{effort:'low'} nested     68                   inert too
//
// Because it never validates unknown fields, an HTTP 200 is NOT evidence of
// support — only an order-of-magnitude change in `reasoning_tokens` counts.
// Upstream therefore declares exactly two levels and refuses to invent a
// low/high/max ladder, which would be three fake levels.
//
// Two separate defects are covered: modelInfoFor declared no Thinking at all
// (so the host rendered no selector — the DSH selector is drawn from the
// declared levels only), and prepareRequestBody never wrote `thinking` (so the
// choice could not reach the gateway even if it had been offered).

// TestModelInfoDeclaresThinkingOnOff pins the level declaration. It is
// asserted for EVERY model, not just the deepseek-v4 family that was measured
// thinking, because no endpoint publishes per-model capability and the upstream
// adapter declares the switch unconditionally.
func TestModelInfoDeclaresThinkingOnOff(t *testing.T) {
	ids := []string{
		"GLM-5.2", "GLM-5.1", "GLM-5", BenefitModel,
		"openpangu-2.0-flash", "openpangu-2.0-pro",
		"deepseek-v4-flash", "deepseek-v4-pro",
		// A model nobody listed still gets the switch: discovery returns ids
		// this table has never seen.
		"some-newly-discovered-model",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			info := modelInfoFor(id, "")
			if info.Thinking == nil {
				t.Fatal("Thinking is nil: the host renders no thinking selector at all")
			}
			// Display order is load-bearing: `on` first, matching upstream.
			if want := []string{"on", "off"}; !slices.Equal(info.Thinking.Levels, want) {
				t.Fatalf("levels = %v, want %v", info.Thinking.Levels, want)
			}
			// `off` is a measured level, so disabling reasoning must be
			// declared as supported.
			if !info.Thinking.ZeroAllowed {
				t.Fatal("ZeroAllowed is false, but thinking:{type:'disabled'} measured reasoning_tokens 0 in 3/3 runs")
			}
			// No invented ladder: the inert effort spellings must not appear.
			for _, fake := range []string{"low", "medium", "high", "max", "minimal", "none"} {
				if slices.Contains(info.Thinking.Levels, fake) {
					t.Fatalf("level %q is declared, but reasoning_effort is inert on this gateway", fake)
				}
			}
		})
	}
}

// TestModelInfoThinkingLevelsAreNotAliased pins that each model gets its own
// copy of the level slice: the host clones into a registry entry, and a shared
// backing array would let one mutation rewrite every model's selector.
func TestModelInfoThinkingLevelsAreNotAliased(t *testing.T) {
	first := modelInfoFor("GLM-5.2", "")
	second := modelInfoFor("deepseek-v4-flash", "")
	if first.Thinking == nil || second.Thinking == nil {
		t.Fatal("Thinking is nil on at least one model")
	}
	first.Thinking.Levels[0] = "mutated"
	if second.Thinking.Levels[0] != "on" {
		t.Fatal("models share one levels backing array")
	}
	if thinkingLevels[0] != "on" {
		t.Fatalf("the package-level level table was mutated: %v", thinkingLevels)
	}
}

// decodeWireBody renders the prepared body as a generic map so a test can
// assert on the exact key path the gateway reads, and can distinguish an
// absent key from a null one.
func decodeWireBody(t *testing.T, payload string) map[string]any {
	t.Helper()
	encoded, _, errPrepare := prepareRequestBody([]byte(payload), "", DefaultConfig(), "sess-thinking")
	if errPrepare != nil {
		t.Fatalf("prepareRequestBody: %v", errPrepare)
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &body); errUnmarshal != nil {
		t.Fatalf("decode wire body: %v", errUnmarshal)
	}
	return body
}

// TestPrepareRequestBodyMapsThinkingOffToDisabled pins the request-body mapping
// for every effort spelling. The field must be TOP-LEVEL `thinking`, not the
// `extra_body.thinking` dialect other gateways use — mixing the two is silently
// ineffective, which is why the nested path is asserted absent.
func TestPrepareRequestBodyMapsThinkingOffToDisabled(t *testing.T) {
	cases := []struct {
		name string
		// payload is the inbound body; the effort spelling under test lives in
		// it exactly as the client sends it.
		payload string
	}{
		{
			name:    "the declared off level",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"off"}`,
		},
		{
			// Plain OpenAI spells "do not think" as `none`, and this gateway
			// ignores `reasoning_effort` anyway — only the top-level switch has
			// any effect — so the intent is honoured rather than dropped.
			name:    "the OpenAI none spelling",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
		},
		{
			name:    "the explicit disabled spelling",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"disabled"}`,
		},
		{
			// Case and surrounding whitespace are not a different level.
			name:    "off with different casing and padding",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"  OFF  "}`,
		},
		{
			// The switch is independent of the model: upstream declares it for
			// every model, so a GLM request must be able to turn thinking off.
			name:    "a non-deepseek model",
			payload: `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"off"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := decodeWireBody(t, testCase.payload)
			thinking, present := body["thinking"]
			if !present {
				t.Fatal("no thinking key: the off level never reaches the gateway")
			}
			switched, ok := thinking.(map[string]any)
			if !ok {
				t.Fatalf("thinking = %#v, want an object", thinking)
			}
			if switched["type"] != "disabled" {
				t.Fatalf("thinking.type = %v, want disabled", switched["type"])
			}
			if len(switched) != 1 {
				t.Fatalf("thinking = %#v, want exactly {type:disabled}", switched)
			}
			// Top-level, NOT the nested dialect.
			if _, nested := body["extra_body"]; nested {
				t.Fatalf("body carries extra_body: %#v", body["extra_body"])
			}
			if _, nested := body["reasoning"]; nested {
				t.Fatalf("body carries a nested reasoning object: %#v", body["reasoning"])
			}
		})
	}
}

// TestPrepareRequestBodyOmitsThinkingWhenOn pins the other half: an "on" effort
// and an unset effort must both leave the key OUT of the body entirely.
//
// `{type:'enabled'}` is equivalent to sending nothing because the server
// defaults to thinking on, so sending it would be pure noise — the assertion is
// on key ABSENCE, not on a null value.
func TestPrepareRequestBodyOmitsThinkingWhenOn(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "the declared on level",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"on"}`,
		},
		{
			// No effort at all: the client did not choose, and the gateway's
			// default is thinking on, so nothing is declared.
			name:    "no effort set",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			// An empty string is "unset", not "off".
			name:    "an empty effort",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":""}`,
		},
		{
			// The inert spellings measured as no-ops must NOT be mapped to the
			// switch: doing so would report a control this gateway does not
			// have, and there is no evidence any of them means "off".
			name:    "an inert high effort",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		},
		{
			name:    "an inert low effort",
			payload: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := decodeWireBody(t, testCase.payload)
			if thinking, present := body["thinking"]; present {
				t.Fatalf("thinking = %#v, want the key to be absent", thinking)
			}
		})
	}
}

// TestPrepareRequestBodyThinkingSwitchKeepsTheRestOfTheBody pins that wrapping
// the request to add the top-level switch does not disturb the fields the
// adapter is responsible for.
func TestPrepareRequestBodyThinkingSwitchKeepsTheRestOfTheBody(t *testing.T) {
	body := decodeWireBody(t, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],`+
		`"max_tokens":123,"temperature":0.25,"reasoning_effort":"off"}`)

	if body["model"] != "deepseek-v4-flash" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	if body["max_tokens"] != float64(123) {
		t.Fatalf("max_tokens = %v, want the caller's 123", body["max_tokens"])
	}
	if body["temperature"] != 0.25 {
		t.Fatalf("temperature = %v, want the caller's 0.25", body["temperature"])
	}
	if body["reasoning_summary"] != "auto" || body["tool_stream"] != true {
		t.Fatalf("CodeArts extensions were lost: %#v", body)
	}
	if messages, ok := body["messages"].([]any); !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", body["messages"])
	}
}

// TestThinkingDisabledRecognisesOnlyTheOffSpellings pins the mapping table
// itself, so an accidental widening of the disable signal is caught directly
// rather than through a body assertion.
func TestThinkingDisabledRecognisesOnlyTheOffSpellings(t *testing.T) {
	cases := []struct {
		effort string
		want   bool
	}{
		{"off", true}, {"OFF", true}, {" off ", true},
		{"none", true}, {"NONE", true},
		{"disabled", true}, {"Disabled", true},
		{"on", false}, {"", false}, {"   ", false},
		{"low", false}, {"high", false}, {"medium", false}, {"max", false},
		{"minimal", false}, {"banana", false},
	}
	for _, testCase := range cases {
		if got := thinkingDisabled(testCase.effort); got != testCase.want {
			t.Errorf("thinkingDisabled(%q) = %v, want %v", testCase.effort, got, testCase.want)
		}
	}
}
