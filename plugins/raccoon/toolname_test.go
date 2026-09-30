package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// History hygiene for unusable tool-call names (upstream `a391dcc`).
//
// ⚠️ A tool call with an empty name is a SECOND, INDEPENDENT poisoning path from
// the orphan/pairing gap. Measured on the wire: `"read"` answers 200 and even
// `"unknown_tool"` answers 200 — the server validates that the name is
// non-empty, not that the tool exists — while `""`, `null` and an absent name are
// rejected with HTTP 400 `code 11133`. Because the bad block is persisted into
// the session, it is replayed on EVERY later request and the session is dead.

// normalisedMessages decodes the `messages` array of a normalised payload.
func normalisedMessages(t *testing.T, payload string) []map[string]any {
	t.Helper()
	normalised := normaliseChatPayload([]byte(payload))
	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(normalised, &decoded); errUnmarshal != nil {
		t.Fatalf("decode normalised payload: %v", errUnmarshal)
	}
	return decoded.Messages
}

// assistantWithFunction renders a minimal assistant turn carrying one tool call
// whose `function` object is the given raw JSON.
func assistantWithFunction(id, function string) string {
	return `{"role":"assistant","content":"","tool_calls":[{"id":"` + id +
		`","type":"function","function":` + function + `}]}`
}

// TestHasUsableToolNameRejectsNonStrings is the judge the whole fix rests on.
//
// ⚠️ The predicate must NOT be `String(name).length > 0`: `undefined` and `null`
// stringify to the NON-EMPTY literals `"undefined"` / `"null"`, so a missing name
// would be read as present and the request would still be rejected upstream
// (upstream `a391dcc` warns about exactly this substitution).
func TestHasUsableToolNameRejectsNonStrings(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"a real tool name", "read", true},
		{"another real tool name", "unknown_tool", true},
		{"an empty string", "", false},
		{"a whitespace-only string", "   ", false},
		{"a tab-only string", "\t\n", false},
		// The literal strings are USABLE names: they are JSON strings, not the
		// missing value. Only the non-string forms below are rejected, which is
		// precisely why `String(x)` is the wrong predicate.
		{"the literal string undefined", "undefined", true},
		{"the literal string null", "null", true},
		{"a JSON null is not a string", nil, false},
		{"a number", float64(42), false},
		{"an object", map[string]any{}, false},
		{"an array", []any{"read"}, false},
		{"a boolean", true, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call := map[string]any{"function": map[string]any{"name": testCase.value}}
			if got := hasUsableToolName(call); got != testCase.want {
				t.Fatalf("hasUsableToolName(%#v) = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
	// A call with no `function` object at all, one with no `name` key, and one
	// whose function is not an object are all unusable — and must not panic.
	if hasUsableToolName(map[string]any{}) {
		t.Error("a call with no function object must be unusable")
	}
	if hasUsableToolName(map[string]any{"function": map[string]any{}}) {
		t.Error("a call with no name key must be unusable")
	}
	if hasUsableToolName(map[string]any{"function": "not an object"}) {
		t.Error("a call whose function is not an object must be unusable")
	}
}

// TestNormaliseMessagesDropsNamelessCallsAndTheirResults is the table-driven
// core of the fix: an unusable call AND the result that pairs with it must both
// go, because a result whose call was dropped is itself an orphan.
func TestNormaliseMessagesDropsNamelessCallsAndTheirResults(t *testing.T) {
	cases := []struct {
		name string
		// function is the raw JSON of the tool call's `function` object.
		function string
		// wantKept reports whether both the call and its result survive.
		wantKept bool
	}{
		{"a usable name keeps both sides", `{"name":"read","arguments":"{}"}`, true},
		{"an unknown but non-empty name is still accepted by the server", `{"name":"unknown_tool","arguments":"{}"}`, true},
		{"an empty name drops both sides", `{"name":"","arguments":"{}"}`, false},
		{"a whitespace-only name drops both sides", `{"name":"   ","arguments":"{}"}`, false},
		{"an explicit null name drops both sides", `{"name":null,"arguments":"{}"}`, false},
		{"an absent name key drops both sides", `{"arguments":"{}"}`, false},
		{"a non-string name drops both sides", `{"name":42,"arguments":"{}"}`, false},
		// The literal strings are usable names: they are strings, not the
		// missing value. This is the case `String(name).length > 0` gets WRONG
		// in the opposite direction — it accepts everything before it, so these
		// two lines exist to prove the predicate is not a length test.
		{"the literal string undefined is a usable name", `{"name":"undefined","arguments":"{}"}`, true},
		{"the literal string null is a usable name", `{"name":"null","arguments":"{}"}`, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := `{"model":"m","messages":[` +
				assistantWithFunction("call_1", testCase.function) + `,` +
				`{"role":"tool","tool_call_id":"call_1","content":"42"}` + `]}`
			messages := normalisedMessages(t, payload)

			keptCalls := 0
			keptResults := 0
			for _, message := range messages {
				if calls, okCalls := message["tool_calls"].([]any); okCalls {
					keptCalls += len(calls)
				}
				if message["role"] == "tool" {
					keptResults++
				}
			}
			if (keptCalls > 0) != testCase.wantKept {
				t.Fatalf("kept %d calls, want kept=%v (messages: %#v)", keptCalls, testCase.wantKept, messages)
			}
			if (keptResults > 0) != testCase.wantKept {
				t.Fatalf("kept %d tool results, want kept=%v: the result of a dropped call is an orphan",
					keptResults, testCase.wantKept)
			}
		})
	}
}

// TestNormaliseMessagesNamelessCallDoesNotPoisonUsableSiblings pins that a bad
// call must not take its batch down with it. The measured online shape is "one
// nameless call + one legitimate pwsh call"; discarding the whole batch would
// throw away a valid invocation.
func TestNormaliseMessagesNamelessCallDoesNotPoisonUsableSiblings(t *testing.T) {
	payload := `{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"call_bad","type":"function","function":{"name":"","arguments":"{}"}},` +
		`{"id":"call_good","type":"function","function":{"name":"pwsh","arguments":"{}"}}` +
		`]},` +
		`{"role":"tool","tool_call_id":"call_bad","content":"bad"},` +
		`{"role":"tool","tool_call_id":"call_good","content":"good"}` +
		`]}`
	normalised := normaliseChatPayload([]byte(payload))
	text := string(normalised)
	if strings.Contains(text, "call_bad") {
		t.Errorf("the nameless call or its result survived: %s", text)
	}
	if !strings.Contains(text, "call_good") {
		t.Errorf("the usable call was discarded along with the bad one: %s", text)
	}
}

// TestNormaliseMessagesKeepsTheExistingPairingRules is the no-regression guard
// for trap #24: the orphan filtering that was already correct must stay correct.
func TestNormaliseMessagesKeepsTheExistingPairingRules(t *testing.T) {
	payload := `{"model":"m","messages":[` +
		assistantWithFunction("call_1", `{"name":"lookup","arguments":"{}"}`) + `,` +
		`{"role":"tool","tool_call_id":"call_1","content":"42"},` +
		assistantWithFunction("call_orphan", `{"name":"ghost","arguments":"{}"}`) + `,` +
		`{"role":"tool","tool_call_id":"call_missing","content":"orphan result"},` +
		`{"role":"user","content":"hi"}` +
		`]}`
	text := string(normaliseChatPayload([]byte(payload)))
	if strings.Contains(text, "call_orphan") {
		t.Errorf("an unanswered call survived: %s", text)
	}
	if strings.Contains(text, "call_missing") {
		t.Errorf("an orphan result survived: %s", text)
	}
	if !strings.Contains(text, "call_1") {
		t.Errorf("the valid pair was dropped: %s", text)
	}
}

// TestNamelessCallIsDroppedByTheOutboundBodyToo pins that the rule reaches the
// request the executor actually sends, not just the translate path.
func TestNamelessCallIsDroppedByTheOutboundBodyToo(t *testing.T) {
	payload := `{"model":"m","messages":[` +
		assistantWithFunction("call_1", `{"name":"","arguments":"{}"}`) + `,` +
		`{"role":"tool","tool_call_id":"call_1","content":"42"}` +
		`]}`
	body, errBody := chatRequestBody(pluginapi.ExecutorRequest{Model: "m", Payload: []byte(payload)})
	if errBody != nil {
		t.Fatalf("build body: %v", errBody)
	}
	if strings.Contains(string(body), "call_1") {
		t.Fatalf("the nameless call reached the outbound body: %s", body)
	}
}
