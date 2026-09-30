package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"
	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file pins the standard `delta.tool_calls` id fallback
// (`llm-adapter.ts:1171`, `:1484-1485`).
//
// The defect it guards is not "one turn fails" but "the session is bricked
// forever": the Huawei backend intermittently omits `tool_calls[].id` entirely
// or sends an empty string, and an assistant tool call that goes out with
// `id:""` can never be paired with its result, because DSH's format-v4 check
// requires the result's `toolCallId` and the call's `callId` to match AND both
// to be non-empty (`length === 0`). Crash recovery then re-fails the same
// check, so the log stops at `tool/call` and only a human can repair it.
// Measured upstream: 3/3 CodeArts tool-call blocks were empty-id across 31
// sessions, against ~14,000 non-empty blocks from every other provider.
//
// The frames below are raw JSON so a test can express the two real shapes
// exactly: the `id` field ABSENT, and the `id` field present but empty. Wrapping
// them to a payload is what the collector loop does before the rewrite runs.

// rawFrames wraps bare JSON payloads the way handleExecutorExecuteStream hands
// them to the stream rewrites.
func rawFrames(frames ...string) []pluginapi.ExecutorStreamChunk {
	out := make([]pluginapi.ExecutorStreamChunk, 0, len(frames))
	for _, frame := range frames {
		out = append(out, pluginapi.ExecutorStreamChunk{Payload: sse.Payload(frame)})
	}
	return out
}

// frameToolCallIDs reads back the call ids of every frame, keeping the frame
// positions so a test can assert which fragment carried which id.
func frameToolCallIDs(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) [][]string {
	t.Helper()
	out := make([][]string, 0, len(chunks))
	for _, chunk := range decodeStream(t, chunks) {
		var ids []string
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				ids = append(ids, call.ID)
			}
		}
		out = append(out, ids)
	}
	return out
}

// The four required behaviours, table-driven: (a) no id anywhere, (b) a real id
// that later empty/missing fragments must not erase, (c) two concurrent calls
// with distinct indices, (d) a stream with no tool calls.
func TestApplyToolCallIDFallback(t *testing.T) {
	cases := []struct {
		name string
		// frames are the raw upstream payloads.
		frames []string
		// want is the id list of each frame; nil means "that frame has no
		// tool_calls", which keeps the position of every other frame pinned.
		want [][]string
		// wantUntouched asserts the input is returned byte for byte, which is
		// the conservatism ported from rewriteDsmlStream.
		wantUntouched bool
	}{
		{
			// (a) The backend never sends an id at all — the shape that bricked
			// the session. Every fragment of the call must still carry the SAME
			// non-empty id, or the tool result cannot be paired with its call.
			name: "no id anywhere yields one stable non-empty id per call",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{\"command\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"\"ls\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			want: [][]string{{"call_0"}, {"call_0"}, nil},
		},
		{
			// (b) The reverse regression: the real id arrives in the FIRST
			// fragment and later fragments repeat it as an empty string. An
			// unconditional overwrite (`if call.id != "" `-less assignment)
			// passes the absent case but fails here, because an empty string is
			// not "missing" to a presence-only check.
			name: "a real id survives later empty fragments",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-real","function":{"name":"pwsh","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":""}}]}}]}`,
			},
			want: [][]string{{"call-real"}, {"call-real"}, {"call-real"}},
		},
		{
			// (b') Same cause, one fragment later: the id is only delivered
			// with the second fragment, so the synthetic id holds until then and
			// the real one takes over from the moment it arrives. Every emitted
			// fragment — not only the first — must carry the resolved id.
			name: "a real id arriving late replaces the synthetic one from then on",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-late","function":{"name":"","arguments":"}"}}]}}]}`,
			},
			want: [][]string{{"call_0"}, {"call-late"}},
		},
		{
			// (c) Two concurrent calls. Indices are unique inside one response,
			// so the synthetic ids must differ from each other AND stay attached
			// to their own index even when the fragments interleave.
			name: "two concurrent calls get distinct stable ids",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"{"}},{"index":1,"function":{"name":"b","arguments":"{"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"","arguments":"}"}},{"index":0,"function":{"name":"","arguments":"}"}}]}}]}`,
			},
			want: [][]string{{"call_0", "call_1"}, {"call_1", "call_0"}},
		},
		{
			// A fragment that omits `index` belongs to the call at index 0
			// (`call.index ?? 0`, `llm-adapter.ts:1483`), so it inherits that
			// call's id instead of minting one of its own.
			name: "a fragment without an index belongs to index zero",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"","arguments":"}"}}]}}]}`,
			},
			want: [][]string{{"call_0"}, {"call_0"}},
		},
		{
			// A DSML call already carries a real id from the parser
			// (dsml.go newDsmlCallID). It must be preserved, not replaced by
			// the synthetic one for its wire index.
			name: "a DSML call keeps its own id",
			frames: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"dsml-abc123","type":"function","function":{"name":"bash","arguments":"{}"}}]}}]}`,
			},
			want: [][]string{{"dsml-abc123"}},
		},
		{
			// (d) No tool calls: the stream must come back untouched. Buffering
			// and re-encoding a plain reply would be pure churn.
			name: "a stream without tool calls is returned untouched",
			frames: []string{
				`{"choices":[{"delta":{"content":"hello "}}]}`,
				`{"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}]}`,
			},
			want:          [][]string{nil, nil},
			wantUntouched: true,
		},
		{
			// Files, usage and other frames that carry no `choices` array are
			// left alone, and an earlier rewrite is not disturbed.
			name: "frames without choices are passed through unchanged",
			frames: []string{
				`{"id":"chunk-1","object":"chat.completion.chunk","usage":{"prompt_tokens":3}}`,
				`{"choices":[{"delta":{"content":"plain"}}]}`,
			},
			want:          [][]string{nil, nil},
			wantUntouched: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			chunks := rawFrames(testCase.frames...)
			rewritten := applyToolCallIDFallback(chunks)

			if testCase.wantUntouched {
				if len(rewritten) != len(chunks) {
					t.Fatalf("chunk count changed: %d -> %d", len(chunks), len(rewritten))
				}
				for index := range chunks {
					if string(rewritten[index].Payload) != string(chunks[index].Payload) {
						t.Fatalf("frame %d was rewritten despite needing no id fallback: %s",
							index, rewritten[index].Payload)
					}
				}
			}

			got := frameToolCallIDs(t, rewritten)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("ids = %v, want %v", got, testCase.want)
			}
			if len(got) != len(testCase.frames) {
				t.Fatalf("frame count = %d, want %d", len(got), len(testCase.frames))
			}
			// The core criterion, asserted independently of the table: no call
			// may ever leave with an empty id, because an empty id is exactly
			// what the format-v4 check rejects.
			for frameIndex, ids := range got {
				for _, id := range ids {
					if strings.TrimSpace(id) == "" {
						t.Fatalf("frame %d emitted an empty tool call id", frameIndex)
					}
				}
			}
		})
	}
}

// TestApplyToolCallIDFallbackLeavesUnrecognisedStreamsAlone pins the
// conservatism shared with rewriteDsmlStream: a frame that does not parse means
// the stream is not shaped the way this rewrite assumes, so the whole input is
// returned exactly as upstream sent it rather than partially rewritten.
func TestApplyToolCallIDFallbackLeavesUnrecognisedStreamsAlone(t *testing.T) {
	chunks := rawFrames(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{"}}]}}]}`,
		`this is not JSON`,
	)
	rewritten := applyToolCallIDFallback(chunks)
	if len(rewritten) != len(chunks) {
		t.Fatalf("chunk count changed: %d -> %d", len(chunks), len(rewritten))
	}
	for index := range chunks {
		if string(rewritten[index].Payload) != string(chunks[index].Payload) {
			t.Fatalf("frame %d was rewritten although the stream does not parse: %s",
				index, rewritten[index].Payload)
		}
	}
}

// TestApplyToolCallIDFallbackKeepsFramePositions pins that the rewrite works on
// a copy indexed by position: an unrecognised or empty frame must not shift the
// frames around it.
func TestApplyToolCallIDFallbackKeepsFramePositions(t *testing.T) {
	chunks := append(
		rawFrames(`{"choices":[{"delta":{"content":"before"}}]}`),
		rawFrames(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pwsh","arguments":"{}"}}]}}]}`)...,
	)
	chunks = append(chunks, rawFrames(`{"choices":[{"delta":{"content":"after"}}]}`)...)

	decoded := decodeStream(t, applyToolCallIDFallback(chunks))
	if len(decoded) != 3 {
		t.Fatalf("frames = %d, want 3", len(decoded))
	}
	if decoded[0].Choices[0].Delta.Content != "before" || decoded[2].Choices[0].Delta.Content != "after" {
		t.Fatalf("surrounding frames moved: %q / %q",
			decoded[0].Choices[0].Delta.Content, decoded[2].Choices[0].Delta.Content)
	}
	if id := decoded[1].Choices[0].Delta.ToolCalls[0].ID; id != "call_0" {
		t.Fatalf("middle frame id = %q, want call_0", id)
	}
}

// TestAggregateCompletionFillsAnEmptyToolCallID pins the NON-STREAMING half of
// the same defect: handleExecutorExecute folds the upstream stream into one
// chat.completion without going through the streaming rewrite, so the fallback
// has to exist there too (`llm-adapter.ts:1678-1681`).
func TestAggregateCompletionFillsAnEmptyToolCallID(t *testing.T) {
	zero, one := 0, 1
	completion := aggregateCompletion([]openai.Chunk{{
		Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: []openai.ToolCall{
			{Index: &zero, Function: openai.ToolCallFunction{Name: "pwsh", Arguments: "{}"}},
			{Index: &one, ID: "call-real", Function: openai.ToolCallFunction{Name: "bash", Arguments: "{}"}},
		}}}},
	}}, "deepseek-v4-flash")

	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	if calls[0].ID != "call_0" {
		t.Fatalf("id = %q, want the synthetic call_0", calls[0].ID)
	}
	// A real id is still preferred over the synthetic one.
	if calls[1].ID != "call-real" {
		t.Fatalf("id = %q, want the backend's call-real", calls[1].ID)
	}
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			t.Fatal("a tool call reached the client with an empty id")
		}
	}
}

// TestAggregateCompletionKeepsARealIDAcrossFragments is the non-streaming
// counterpart of the "later empty fragment" regression: the id already recorded
// for the call must survive a follow-up fragment that omits it.
func TestAggregateCompletionKeepsARealIDAcrossFragments(t *testing.T) {
	zero := 0
	completion := aggregateCompletion([]openai.Chunk{
		{Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: []openai.ToolCall{
			{Index: &zero, ID: "call-real", Function: openai.ToolCallFunction{Name: "pwsh", Arguments: "{"}},
		}}}}},
		{Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: []openai.ToolCall{
			{Index: &zero, Function: openai.ToolCallFunction{Arguments: "}"}},
		}}}}},
	}, "deepseek-v4-flash")

	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call-real" {
		t.Fatalf("id = %q, want call-real to survive the id-less fragment", calls[0].ID)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Fatalf("arguments = %q, want the fragments concatenated", calls[0].Function.Arguments)
	}
}
