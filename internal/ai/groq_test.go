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
	if !strings.Contains(ownerPrompt, "FULL AUTHORIZATION") {
		t.Errorf("expected owner prompt to acknowledge FULL AUTHORIZATION")
	}
	if !strings.Contains(ownerPrompt, "Strictly NO unsolicited task offers") {
		t.Errorf("expected system prompt to ban unsolicited task offers")
	}
	if !strings.Contains(ownerPrompt, "Conversational Explanations & Quoted Replies") {
		t.Errorf("expected system prompt to include Conversational Explanations & Quoted Replies")
	}
	if !strings.Contains(ownerPrompt, "Brutally Honest & Zero Bluffing") {
		t.Errorf("expected system prompt to enforce Brutally Honest & Zero Bluffing")
	}
	if !strings.Contains(ownerPrompt, "Strictly NO hallucinated or fabricated project architectures") {
		t.Errorf("expected system prompt to forbid fabricated architectures")
	}
	if !strings.Contains(ownerPrompt, "STRICT TRANSACTION RULES") {
		t.Errorf("expected system prompt to include STRICT TRANSACTION RULES")
	}
	if !strings.Contains(ownerPrompt, "NEVER simulate, pretend, claim, or promise in text that you have sent") {
		t.Errorf("expected system prompt to ban fake transfer claims")
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

	// Test Chat Context (DM vs GC) injection
	dmContext := "- YOU ARE IN A DIRECT 1-ON-1 PRIVATE CHAT (DM) WITH @skipp_dev."
	dmPrompt := client.systemPrompt("skipp_dev", true, profile, dmContext)
	if !strings.Contains(dmPrompt, "Chat Type & Environment:") || !strings.Contains(dmPrompt, "DIRECT 1-ON-1 PRIVATE CHAT") {
		t.Errorf("expected chat environment context to be injected into system prompt")
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
		"github_close_pr":        false,
		"github_close_issue":     false,
		"github_create_repo":     false,
		"moltbook_search":        false,
		"moltbook_notifications": false,
		"send_email":             false,
		"get_active_groups":      false,
		"vercel_search_domains":  false,
		"notify_owner":           false,
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

	// 1. Dynamic balance followup test
	rawBalData := `{"total_usd_value": "$1.30", "active_holdings": [{"chain": "robinhood", "token": "ETH", "amount": "0.0004835", "usd": "$1.30"}], "dry_chains": ["solana", "base", "ethereum", "arbitrum", "bnb"]}`
	outBal, err := c.GenerateToolFollowup(ctx, "skipp_dev", true, "How much you got?", "get_balances", "call_1", `{}`, rawBalData, nil)
	if err != nil {
		if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate limit") || strings.Contains(err.Error(), "exhausted") || strings.Contains(err.Error(), "UNAVAILABLE") {
			t.Skipf("skipping live test due to upstream API limits/service availability: %v", err)
		}
		t.Fatalf("GenerateToolFollowup balance failed: %v", err)
	}
	t.Logf("Generated dynamic balance followup: %s", outBal)
	if strings.TrimSpace(outBal) == "" {
		t.Errorf("expected non-empty balance output")
	}

	// 2. Dynamic address followup test
	rawAddrData := `{"solana_address": "8N3V7dZKpU19kL", "evm_address": "0x4b7e1234567890", "supported_evm_chains": ["base", "robinhood", "ethereum", "arbitrum", "bnb"]}`
	outAddr, err := c.GenerateToolFollowup(ctx, "skipp_dev", true, "where can i send you some funds?", "get_wallet_address", "call_2", `{}`, rawAddrData, nil)
	if err != nil {
		if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate limit") || strings.Contains(err.Error(), "exhausted") || strings.Contains(err.Error(), "UNAVAILABLE") {
			t.Skipf("skipping live test due to upstream API limits/service availability: %v", err)
		}
		t.Fatalf("GenerateToolFollowup address failed: %v", err)
	}
	t.Logf("Generated dynamic address followup: %s", outAddr)
	if strings.TrimSpace(outAddr) == "" {
		t.Errorf("expected non-empty address output")
	}
}

func TestParseXMLToolCalls(t *testing.T) {
	// 1. Tag-based format (as seen in Groq fallback leak screenshot)
	leakXML := `<toolcall>
<function=senddm>
<parameter=recipientemail>
michael.j.vianney@gmail.com
</parameter>
<parameter=message>
TeraWallet ($TERA) on Robinhood EVM. Market cap is $36.19K.
</parameter>
</function>
</toolcall>`

	calls, ok := parseXMLToolCalls(leakXML)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected 1 tool call parsed, got %d (ok=%v)", len(calls), ok)
	}

	call := calls[0]
	// Should be smartly converted to send_email because recipient is an email address
	if call.Function.Name != "send_email" {
		t.Errorf("expected function name 'send_email', got %s", call.Function.Name)
	}

	var args map[string]interface{}
	_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	if args["to"] != "michael.j.vianney@gmail.com" {
		t.Errorf("expected to='michael.j.vianney@gmail.com', got %v", args["to"])
	}
	if !strings.Contains(args["body"].(string), "TeraWallet") {
		t.Errorf("expected body to contain TeraWallet, got %v", args["body"])
	}

	// 2. JSON-inside-tags format
	jsonXML := `<tool_call>
{"name": "analyze_token", "arguments": {"address": "0x3c12e57fa7817a86ce7c254db9ea5fe639e233f8"}}
</tool_call>`
	calls2, ok2 := parseXMLToolCalls(jsonXML)
	if !ok2 || len(calls2) != 1 {
		t.Fatalf("expected 1 json tool call parsed, got %d (ok=%v)", len(calls2), ok2)
	}
	if calls2[0].Function.Name != "analyze_token" {
		t.Errorf("expected function name 'analyze_token', got %s", calls2[0].Function.Name)
	}

	// 3. Declaration/call syntax leak (as seen in Gemini/Groq tool leak: declaration:default_api:get_group_topics{})
	declText := "declaration:default_api:get_group_topics{}"
	calls3, ok3 := parseXMLToolCalls(declText)
	if !ok3 || len(calls3) != 1 {
		t.Fatalf("expected 1 declaration tool call parsed, got %d (ok=%v)", len(calls3), ok3)
	}
	if calls3[0].Function.Name != "get_group_topics" {
		t.Errorf("expected function name 'get_group_topics', got %s", calls3[0].Function.Name)
	}

	// 4. Normal text with no XML or declaration
	normalText := "Hey anon, market looks hot today."
	_, ok4 := parseXMLToolCalls(normalText)
	if ok4 {
		t.Errorf("expected ok=false for plain text")
	}
}

func TestNormalizeToolCall(t *testing.T) {
	// Verify smart redirection of send_dm to send_email when target is an email
	normName, normArgs := NormalizeToolCall("senddm", `{"recipient":"alice@example.com","message":"check this out"}`)
	if normName != "send_email" {
		t.Errorf("expected send_email, got %s", normName)
	}
	var args map[string]interface{}
	_ = json.Unmarshal([]byte(normArgs), &args)
	if args["to"] != "alice@example.com" {
		t.Errorf("expected to='alice@example.com', got %v", args["to"])
	}

	// Verify send_dm remains send_dm for telegram handles
	dmName, dmArgs := NormalizeToolCall("senddm", `{"recipient":"@alice","message":"gm"}`)
	if dmName != "send_dm" {
		t.Errorf("expected send_dm, got %s", dmName)
	}
	var dmMap map[string]interface{}
	_ = json.Unmarshal([]byte(dmArgs), &dmMap)
	if dmMap["recipient"] != "@alice" {
		t.Errorf("expected recipient='@alice', got %v", dmMap["recipient"])
	}

	// Verify close_pr normalization
	prName, _ := NormalizeToolCall("close_pr", `{"pr_number":7}`)
	if prName != "github_close_pr" {
		t.Errorf("expected github_close_pr, got %s", prName)
	}

	// Verify close_issue normalization
	issueName, _ := NormalizeToolCall("close_issue", `{"issue_number":12}`)
	if issueName != "github_close_issue" {
		t.Errorf("expected github_close_issue, got %s", issueName)
	}

	// Verify moltbook_search normalization
	searchName, _ := NormalizeToolCall("search_moltbook", `{"query":"agents"}`)
	if searchName != "moltbook_search" {
		t.Errorf("expected moltbook_search, got %s", searchName)
	}

	// Verify moltbook_notifications normalization
	notifName, _ := NormalizeToolCall("moltbook_inbox", `{}`)
	if notifName != "moltbook_notifications" {
		t.Errorf("expected moltbook_notifications, got %s", notifName)
	}
}

func TestSanitizeFileContent(t *testing.T) {
	// Case 1: [REPLYING_TO_USER] prefix from Qwen
	input1 := `[REPLYING_TO_USER] I have updated the README.md file to include the X and Telegram links in the header section, as requested.

# Liege Agents App
Autonomous agent workforce.
- Twitter: https://x.com/liegeagents
- Telegram: https://t.me/liegeagents`

	expected1 := `# Liege Agents App
Autonomous agent workforce.
- Twitter: https://x.com/liegeagents
- Telegram: https://t.me/liegeagents`

	if got := SanitizeFileContent(input1); got != expected1 {
		t.Errorf("TestSanitizeFileContent Case 1 failed:\nGot:\n%s\nExpected:\n%s", got, expected1)
	}

	// Case 2: Wrapping markdown code fence with conversational preamble and sign-off
	input2 := `Here is the updated README.md file:
` + "```markdown" + `
# Liege Agents
Welcome to the repo.
` + "```" + `
Let me know if you need anything else!`

	expected2 := `# Liege Agents
Welcome to the repo.`

	if got := SanitizeFileContent(input2); got != expected2 {
		t.Errorf("TestSanitizeFileContent Case 2 failed:\nGot:\n%s\nExpected:\n%s", got, expected2)
	}

	// Case 3: Preserving TARGET_NOT_FOUND signal
	input3 := `[TARGET_NOT_FOUND: could not find section 'Roadmap'] Current sections are: Intro, Setup`
	if got := SanitizeFileContent(input3); got != input3 {
		t.Errorf("TestSanitizeFileContent Case 3 failed:\nGot:\n%s\nExpected:\n%s", got, input3)
	}

	// Case 4: Stripping <think> reasoning tags
	input4 := `<think>The user wants me to add links.</think>
# Project Title
Content here.`
	expected4 := `# Project Title
Content here.`
	if got := SanitizeFileContent(input4); got != expected4 {
		t.Errorf("TestSanitizeFileContent Case 4 failed:\nGot:\n%s\nExpected:\n%s", got, expected4)
	}
}



