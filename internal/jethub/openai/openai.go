// Package openai holds the subset of the OpenAI Chat Completions wire format
// that the CodeArts executor produces and consumes. CodeArts' Snap-Access chat
// endpoint is Chat-Completions shaped, so these types are used both for the
// outbound upstream body and for the chunks streamed back to CPA.
package openai

// Message is one entry of the `messages` array. Content is `any` because the
// Chat Completions API accepts both a plain string and an array of content
// parts (text/image_url).
type Message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// ToolCallFunction carries the function name and its JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// FunctionDef describes a callable tool.
type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// Tool is a tool declaration in a request.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// Request is the body sent to the CodeArts chat endpoint. It mirrors the OpenAI
// Chat Completions request and adds the CodeArts-specific extension fields.
type Request struct {
	Model            string    `json:"model"`
	Messages         []Message `json:"messages"`
	Stream           bool      `json:"stream,omitempty"`
	MaxTokens        *int      `json:"max_tokens,omitempty"`
	Temperature      *float64  `json:"temperature,omitempty"`
	TopP             *float64  `json:"top_p,omitempty"`
	Stop             any       `json:"stop,omitempty"`
	Tools            []Tool    `json:"tools,omitempty"`
	ToolChoice       any       `json:"tool_choice,omitempty"`
	ParallelToolCall *bool     `json:"parallel_tool_calls,omitempty"`
	ResponseFormat   any       `json:"response_format,omitempty"`
	ReasoningEffort  string    `json:"reasoning_effort,omitempty"`
	User             string    `json:"user,omitempty"`

	// CodeArts extensions.
	PromptCacheKey   string `json:"prompt_cache_key,omitempty"`
	Include          any    `json:"include,omitempty"`
	ReasoningSummary any    `json:"reasoning_summary,omitempty"`
	ToolStream       *bool  `json:"tool_stream,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Delta is the incremental assistant message inside a stream chunk.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice is one streaming choice.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Chunk is a single `chat.completion.chunk` SSE payload.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// Choice is one non-streaming completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

// Completion is a non-streaming `chat.completion` response.
type Completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}
