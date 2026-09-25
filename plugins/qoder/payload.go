package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The encrypted inference payload.
//
// Ported from `QoderEncryptedInfer.prepareInfer` (`qoder-wasm.ts:497-565`), which
// replicates the official `G4A()` structure. The shape matters:
//   - `chat_context` must NOT be an empty object; it carries the question and the
//     model configuration, and an empty one fails at runtime with
//     `[FAIL]node:... msg:Execution failed` (`qoder-wasm.ts:503-505`);
//   - `model_config` has ten fields upstream; all of them are reproduced
//     (`qoder-wasm.ts:544-557`);
//   - `business` decides the server routing pool and is mandatory
//     (`qoder-wasm.ts:563-564`).

// inferTextBlock is one entry of the payload's `system` array.
type inferTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// inferModelConfigExtra is `chat_context.extra.modelConfig`.
type inferModelConfigExtra struct {
	Key         string `json:"key"`
	IsReasoning bool   `json:"is_reasoning"`
}

// inferChatContextExtra is `chat_context.extra`.
type inferChatContextExtra struct {
	Context         []any                 `json:"context"`
	ModelConfig     inferModelConfigExtra `json:"modelConfig"`
	OriginalContent string                `json:"originalContent"`
}

// inferChatContext is `chat_context`.
type inferChatContext struct {
	Text       string                `json:"text"`
	Features   []any                 `json:"features"`
	Extra      inferChatContextExtra `json:"extra"`
	ChatPrompt string                `json:"chatPrompt"`
	ImageURLs  any                   `json:"imageUrls"`
}

// inferModelConfig is `model_config`.
type inferModelConfig struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	Model          string `json:"model"`
	Format         string `json:"format"`
	IsVL           bool   `json:"is_vl"`
	IsReasoning    bool   `json:"is_reasoning"`
	APIKey         string `json:"api_key"`
	URL            string `json:"url"`
	Source         string `json:"source"`
	MaxInputTokens int64  `json:"max_input_tokens"`
}

// inferPayload is the JSON handed to the WASM for encryption.
type inferPayload struct {
	RequestID      string           `json:"request_id"`
	RequestSetID   string           `json:"request_set_id"`
	ChatRecordID   string           `json:"chat_record_id"`
	SessionID      string           `json:"session_id"`
	Stream         bool             `json:"stream"`
	ChatTask       string           `json:"chat_task"`
	ChatContext    inferChatContext `json:"chat_context"`
	IsReply        bool             `json:"is_reply"`
	IsRetry        bool             `json:"is_retry"`
	Source         int              `json:"source"`
	Version        string           `json:"version"`
	AgentID        string           `json:"agent_id"`
	TaskID         string           `json:"task_id"`
	SessionType    string           `json:"session_type"`
	AliyunUserType string           `json:"aliyun_user_type"`
	ModelConfig    inferModelConfig `json:"model_config"`
	CustomModel    any              `json:"custom_model"`
	System         []inferTextBlock `json:"system"`
	Messages       []inferMessage   `json:"messages"`
	Tools          []any            `json:"tools"`
	Parameters     map[string]any   `json:"parameters"`
	Business       map[string]any   `json:"business,omitempty"`
}

// buildInferPayload renders the payload with a fresh request/session identity.
func buildInferPayload(ask inferAsk) ([]byte, error) {
	requestID := randomUUID()
	sessionID := randomUUID()

	parameters := map[string]any{}
	if ask.MaxTokens != nil {
		parameters["max_tokens"] = *ask.MaxTokens
	}
	if ask.ReasoningEffort != "" {
		parameters["reasoning_effort"] = ask.ReasoningEffort
		// `enable_thinking` follows the effort: only the literal `none` disables
		// thinking (`qoder-wasm.ts:508-511`).
		parameters["enable_thinking"] = ask.ReasoningEffort != "none"
	}

	messages := make([]inferMessage, 0, len(ask.History)+1)
	messages = append(messages, ask.History...)
	if len(messages) == 0 {
		messages = append(messages, inferMessage{Role: "user", Content: ask.UserText})
	}

	system := []inferTextBlock{}
	if ask.SystemText != "" {
		system = append(system, inferTextBlock{Type: "text", Text: ask.SystemText})
	}

	source := ask.Source
	if source == "" {
		source = "system"
	}
	format := ask.Format
	if format == "" {
		format = "openai"
	}
	isVL := true
	if ask.IsVL != nil {
		isVL = *ask.IsVL
	}
	maxInputTokens := ask.MaxInputTokens
	if maxInputTokens <= 0 {
		maxInputTokens = 200_000
	}

	payload := inferPayload{
		RequestID:    requestID,
		RequestSetID: requestID,
		ChatRecordID: requestID,
		SessionID:    sessionID,
		Stream:       true,
		ChatTask:     "FREE_INPUT",
		ChatContext: inferChatContext{
			Text:     ask.UserText,
			Features: []any{},
			Extra: inferChatContextExtra{
				Context:         []any{},
				ModelConfig:     inferModelConfigExtra{Key: ask.ModelKey, IsReasoning: ask.IsReasoning},
				OriginalContent: ask.UserText,
			},
			ChatPrompt: "",
			ImageURLs:  nil,
		},
		IsReply:        true,
		IsRetry:        false,
		Source:         1,
		Version:        "3",
		AgentID:        "agent_common",
		TaskID:         "common",
		SessionType:    ask.SessionType,
		AliyunUserType: "",
		ModelConfig: inferModelConfig{
			Key:            ask.ModelKey,
			DisplayName:    ask.DisplayName,
			Model:          "",
			Format:         format,
			IsVL:           isVL,
			IsReasoning:    ask.IsReasoning,
			APIKey:         "",
			URL:            "",
			Source:         source,
			MaxInputTokens: maxInputTokens,
		},
		CustomModel: nil,
		System:      system,
		Messages:    messages,
		Tools:       []any{},
		Parameters:  parameters,
		Business:    ask.Business,
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, abiboot.Errorf("encode_infer_payload", "encode encrypted inference payload: %v", errMarshal)
	}
	return encoded, nil
}

// inferAskFromRequest projects a chat-completions request onto the WASM ask.
//
// The system prompt is lifted out of `messages` into the payload's dedicated
// `system` block, which is where the official client puts it. Jet-Hub can pass
// `options.system` separately (`qoder-adapter.ts:267-269`); in CPA the system
// prompt arrives as a `system` role message, so it must be split out here or it
// would be sent twice.
func inferAskFromRequest(request pluginapi.ExecutorRequest, credential *Credential, cfg Config, p *product) (inferAsk, error) {
	wire, errParse := parseWireRequest(request.Payload)
	if errParse != nil {
		return inferAsk{}, errParse
	}
	model := request.Model
	if strings.TrimSpace(wire.Model) != "" {
		model = wire.Model
	}
	if strings.TrimSpace(model) == "" {
		return inferAsk{}, abiboot.HTTPError("invalid_request", http.StatusBadRequest, "请求缺少 model 字段")
	}

	history := make([]inferMessage, 0, len(wire.Messages))
	systemParts := make([]string, 0, 1)
	userText := ""
	for _, message := range wire.Messages {
		text := messageText(message.Content)
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "user":
			if text != "" {
				userText = text
			}
		}
		history = append(history, inferMessage{Role: message.Role, Content: text})
	}
	if userText == "" {
		// The last user turn is the question; fall back to the last message so a
		// tool-result-only tail still produces a usable request.
		for index := len(history) - 1; index >= 0; index-- {
			if history[index].Role == "user" {
				userText = history[index].Content
				break
			}
		}
	}

	entry, known := catalogModelFor(p, model)
	ask := inferAsk{
		ModelKey:        model,
		UserText:        userText,
		SystemText:      strings.Join(systemParts, "\n\n"),
		History:         history,
		ReasoningEffort: strings.TrimSpace(wire.ReasoningEffort),
		Source:          "system",
		Format:          "openai",
		SessionType:     sessionType(cfg, p),
		// Mandatory: without it the server routes `qfmodel` to a broken node
		// (`qoder-wasm.ts:563-564`, `qoder-adapter.ts:305-315`).
		Business: map[string]any{"type": "agent"},
	}
	if known {
		ask.IsReasoning = entry.SupportsThinking
		isVL := entry.SupportsImage
		ask.IsVL = &isVL
		ask.DisplayName = entry.Display
		ask.MaxInputTokens = entry.ContextWindow
	}
	switch {
	case wire.MaxTokens != nil && *wire.MaxTokens > 0:
		maxTokens := *wire.MaxTokens
		ask.MaxTokens = &maxTokens
	case cfg.DefaultMaxTokens > 0:
		maxTokens := cfg.DefaultMaxTokens
		ask.MaxTokens = &maxTokens
	}
	return ask, nil
}
