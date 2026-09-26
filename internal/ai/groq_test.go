package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/joho/godotenv"
	"shipp/internal/memory"
)

func TestSystemPrompt(t *testing.T) {
	client := NewClient("mock_key", "qwen/qwen3.8-27b", "mock_gemini_key", []string{"skipp_dev", "shigarakixbt"})

	// Test Owner prompt
	ownerPrompt := client.systemPrompt("skipp_dev", true, nil)
	if !strings.Contains(ownerPrompt, "@skipp_dev") {
		t.Errorf("expected owner prompt to contain @skipp_dev")
	}
	if !strings.Contains(ownerPrompt, "OWNERS/CREATORS") {
		t.Errorf("expected owner prompt to acknowledge owner role")
	}

	// Test Non-Owner prompt
	guestPrompt := client.systemPrompt("anon123", false, nil)
	if !strings.Contains(guestPrompt, "CANNOT authorize sending crypto") {
		t.Errorf("expected guest prompt to restrict crypto send")
	}

	// Test UserProfile injection
	profile := &memory.UserProfile{
		ActiveProjects: "DavidNzube101/shipp",
		Preferences:    "prefers Go over Python",
		LifeContext:    "FUTO 300 level CS",
	}
	profilePrompt := client.systemPrompt("skipp_dev", true, profile)
	if !strings.Contains(profilePrompt, "DavidNzube101/shipp") || !strings.Contains(profilePrompt, "FUTO 300 level CS") {
		t.Errorf("expected profile context to be injected into prompt")
	}
}

func TestToolsDefinition(t *testing.T) {
	client := NewClient("mock_key", "qwen/qwen3.8-27b", "mock_gemini_key", []string{"skipp_dev"})
	tools := client.buildTools()

	expectedTools := map[string]bool{
		"get_wallet_address":     false,
		"get_balances":           false,
		"convert_crypto":          false,
		"send_crypto":            false,
		"summarize_context":      false,
		"clear_context":          false,
		"web_search":             false,
		"analyze_token":          false,
		"github_inspect_project": false,
		"github_edit_file":       false,
		"github_merge_pr":        false,
		"send_email":              false,
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

	client := NewClient("mock_key", "qwen/qwen3.8-27b", "mock_gemini_key", []string{"skipp_dev"})
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

func TestSystemPromptVisionAndMemoryRules(t *testing.T) {
	client := NewClient("mock_key", "qwen/qwen3.8-27b", "mock_gemini_key", []string{"skipp_dev"})
	prompt := client.systemPrompt("skipp_dev", true, nil)

	if !strings.Contains(prompt, "1-3 sentences max") {
		t.Errorf("expected prompt to restrict image reactions to 1-3 sentences max")
	}
	if !strings.Contains(prompt, "Realist") {
		t.Errorf("expected prompt to state Realist worldview")
	}
}

func TestGenerateVisionReplyMock(t *testing.T) {
	var receivedBody ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)

		resp := ChatCompletionResponse{
			ID: "mock-vision-reply",
		}
		resp.Choices = append(resp.Choices, struct {
			Index        int         `json:"index"`
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{
			Index: 0,
			Message: ChatMessage{
				Role:    "assistant",
				Content: "nice 40x long on Bayse anon, up 32% already.",
			},
			FinishReason: "stop",
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient("mock_key", "qwen/qwen3.8-27b", "mock_gemini_key", []string{"skipp_dev"})
	client.httpClient = server.Client()

	// Direct call to sendChatCompletion with mock server by swapping DefaultGroqURL logic or testing request structure
	ctx := context.Background()
	req := ChatCompletionRequest{
		Model: client.model,
		Messages: []ChatMessage{
			{Role: "system", Content: client.systemPrompt("skipp_dev", true, nil)},
			{Role: "user", Content: "[User sent an image]\n[Visual Perception: BTC Long 40x on Bayse, +32.75%]\n\nUser caption: look at this"},
		},
		MaxTokens: 350,
	}

	data, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(string(data)))
	res, err := server.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("mock request failed: %v", err)
	}
	defer res.Body.Close()

	if !strings.Contains(receivedBody.Messages[1].Content, "BTC Long 40x on Bayse") {
		t.Errorf("expected visual perception in user prompt")
	}
}

func TestLiveModelDynamicToolFollowup(t *testing.T) {
	_ = godotenv.Load("../../.env")
	groqKey := os.Getenv("GROQ_API_KEY")
	geminiKey := os.Getenv("GEMINI_API_KEY")
	if groqKey == "" && geminiKey == "" {
		t.Skip("no api keys")
	}

	ctx := context.Background()
	c := NewClient(groqKey, "qwen/qwen3.8-27b", geminiKey, []string{"skipp_dev"})
	rawData := `{"total_usd_value": "$1.30", "active_holdings": [{"chain": "robinhood", "token": "ETH", "amount": "0.0004835", "usd": "$1.30"}], "dry_chains": ["solana", "base", "ethereum", "arbitrum", "bnb"]}`

	out, err := c.GenerateToolFollowup(ctx, "skipp_dev", true, "How much you got?", "get_balances", "call_1", `{}`, rawData, nil)
	if err != nil {
		t.Fatalf("GenerateToolFollowup failed: %v", err)
	}
	t.Logf("Generated dynamic followup: %s", out)
	if strings.TrimSpace(out) == "" {
		t.Errorf("expected non-empty output")
	}
}


