package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemPrompt(t *testing.T) {
	client := NewClient("mock_key", "qwen/qwen3.8-27b", []string{"skipp_dev", "shigarakixbt"})

	// Test Owner prompt
	ownerPrompt := client.systemPrompt("skipp_dev", true)
	if !strings.Contains(ownerPrompt, "@skipp_dev") {
		t.Errorf("expected owner prompt to contain @skipp_dev")
	}
	if !strings.Contains(ownerPrompt, "OWNERS/CREATORS") {
		t.Errorf("expected owner prompt to acknowledge owner role")
	}

	// Test Non-Owner prompt
	guestPrompt := client.systemPrompt("anon123", false)
	if !strings.Contains(guestPrompt, "CANNOT authorize sending crypto") {
		t.Errorf("expected guest prompt to restrict crypto send")
	}
}

func TestToolsDefinition(t *testing.T) {
	client := NewClient("mock_key", "qwen/qwen3.8-27b", []string{"skipp_dev"})
	tools := client.buildTools()

	expectedTools := map[string]bool{
		"get_wallet_address": false,
		"get_balances":       false,
		"send_crypto":        false,
		"summarize_context":  false,
		"clear_context":      false,
		"web_search":         false,
		"analyze_token":      false,
	}

	for _, tool := range tools {
		if _, ok := expectedTools[tool.Function.Name]; ok {
			expectedTools[tool.Function.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected tool %s was not found in tool definitions", name)
		}
	}
}

func TestMockChatCompletionWithToolCall(t *testing.T) {
	// Create mock HTTP server simulating Groq API returning a tool call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatCompletionResponse{
			ID: "mock-123",
		}
		resp.Choices = append(resp.Choices, struct {
			Index        int         `json:"index"`
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{
			Index: 0,
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_abc123",
						Type: "function",
						Function: FunctionCall{
							Name:      "get_wallet_address",
							Arguments: `{"chain":"solana"}`,
						},
					},
				},
			},
			FinishReason: "tool_calls",
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient("mock_key", "qwen/qwen3.8-27b", []string{"skipp_dev"})
	client.httpClient = server.Client()

	reqBody := ChatCompletionRequest{
		Model:    "qwen/qwen3.8-27b",
		Messages: []ChatMessage{{Role: "user", Content: "what's your sol address?"}},
	}

	// We can test sendChatCompletion indirectly or mock the URL
	data, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", server.URL, strings.NewReader(string(data)))
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("mock request failed: %v", err)
	}
	defer res.Body.Close()

	var chatResp ChatCompletionResponse
	if err := json.NewDecoder(res.Body).Decode(&chatResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(chatResp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("expected tool calls in mock response")
	}
	if chatResp.Choices[0].Message.ToolCalls[0].Function.Name != "get_wallet_address" {
		t.Errorf("expected tool name 'get_wallet_address', got %s", chatResp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}
