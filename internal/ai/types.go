package ai

import "encoding/json"


type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
	// Gemini 3 attaches a thought signature here that must be echoed back on the next turn.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type FunctionDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ChatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []ChatMessage    `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  string           `json:"tool_choice,omitempty"`
	Temperature      float64          `json:"temperature,omitempty"`
	MaxTokens        int              `json:"max_tokens,omitempty"`
	FrequencyPenalty float64          `json:"frequency_penalty,omitempty"`
	PresencePenalty  float64          `json:"presence_penalty,omitempty"`

	// Provider-specific reasoning controls, set per attempt by sendChatCompletion so
	// chain-of-thought never lands in the reply text.
	ReasoningFormat  string           `json:"reasoning_format,omitempty"`  // Groq Qwen: "hidden"
	IncludeReasoning *bool            `json:"include_reasoning,omitempty"` // Groq GPT-OSS: false
	ReasoningEffort  string           `json:"reasoning_effort,omitempty"`  // Groq: "low"
	Reasoning        *ReasoningConfig `json:"reasoning,omitempty"`         // OpenRouter
}

type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Exclude bool   `json:"exclude"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage UsageInfo `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Tool invocation intent representations
type SendCryptoArgs struct {
	Chain     string  `json:"chain"`
	Recipient string  `json:"recipient"`
	Amount    float64 `json:"amount"`
}

type ToolResult struct {
	Name   string
	Data   string
	Error  error
	IsSend bool
	SendArgs *SendCryptoArgs
}
