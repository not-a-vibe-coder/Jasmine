package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"shipp/internal/memory"
)

const DefaultGroqURL = "https://api.groq.com/openai/v1/chat/completions"

type Client struct {
	apiKey     string
	model      string
	owners     []string
	httpClient *http.Client
	tools      []ToolDefinition
}

func NewClient(apiKey, model string, owners []string) *Client {
	if model == "" {
		model = "qwen/qwen3.8-27b"
	}
	c := &Client{
		apiKey:     apiKey,
		model:      model,
		owners:     owners,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	c.tools = c.buildTools()
	return c
}

func (c *Client) buildTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "get_wallet_address",
				Description: "Get the bot's deposit wallet addresses for receiving crypto on Solana (SVM) and EVM chains (Base, Robinhood, Ethereum, Arbitrum, BNB). Use whenever user asks for deposit address, wallet address, or where to send funds.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"chain": map[string]interface{}{
							"type":        "string",
							"description": "Optional chain name: solana, evm, base, robinhood, rh, ethereum, arbitrum, bnb",
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "get_balances",
				Description: "Check the bot's current live crypto balances across Solana and EVM chains (Base, Robinhood, Ethereum, Arbitrum, BNB). Use whenever user asks about balance, money, funds, or holdings.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"chain": map[string]interface{}{
							"type":        "string",
							"description": "Optional specific chain: solana, base, robinhood, rh, ethereum, arbitrum, bnb",
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "send_crypto",
				Description: "Send crypto (SOL on Solana, or ETH/BNB on EVM chains: base, robinhood, rh, ethereum, arbitrum, bnb) to a recipient address. This can only be executed by bot owners.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"chain": map[string]interface{}{
							"type":        "string",
							"description": "The chain to transfer on: solana, base, robinhood, rh, ethereum, arbitrum, bnb",
						},
						"recipient": map[string]interface{}{
							"type":        "string",
							"description": "The destination wallet address (Solana base58 or EVM 0x... address)",
						},
						"amount": map[string]interface{}{
							"type":        "number",
							"description": "The amount of tokens to send (e.g. 0.05)",
						},
					},
					"required": []string{"chain", "recipient", "amount"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "summarize_context",
				Description: "Summarize recent conversation history and key points in the chat. Use whenever user asks for a recap, summary, or what happened earlier.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "clear_context",
				Description: "Clear or reset the conversational memory/context of the bot for this chat. Use whenever user asks to reset memory, forget history, or start fresh.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "web_search",
				Description: "Search the live internet for real-time information, breaking news, current world leaders, elections, market events, and facts beyond training cutoff. Always use when asked about current facts.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query (e.g. 'current president of Uruguay', 'president of Venezuela')",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "analyze_token",
				Description: "Analyze any crypto token Contract Address (CA) or Solana mint address to get real-time price, market cap, 24h volume, 24h price change, liquidity, and buy/sell transaction counts across DexScreener and Codex. Always invoke when given a token address or asked about a token's market metrics.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"address": map[string]interface{}{
							"type":        "string",
							"description": "The token contract address (EVM 0x... or Solana base58 mint)",
						},
						"chain": map[string]interface{}{
							"type":        "string",
							"description": "Optional specific chain (e.g. solana, base, ethereum, arbitrum, bsc). Leave empty if not specified by user.",
						},
					},
					"required": []string{"address"},
				},
			},
		},
	}
}

func (c *Client) systemPrompt(senderUsername string, isOwner bool) string {
	ownersStr := strings.Join(c.owners, ", @")
	if ownersStr != "" {
		ownersStr = "@" + ownersStr
	}

	roleNote := fmt.Sprintf("Current speaker is @%s.", senderUsername)
	if isOwner {
		roleNote += " This user is one of your OWNERS/CREATORS. You have high respect for them."
	} else {
		roleNote += " This user is a group member (not an owner). They can chat and check balances/addresses, but CANNOT authorize sending crypto."
	}

	return fmt.Sprintf(`You are Shipp (@Shipp0Bot), a sharp, witty, highly intelligent personal AI companion built for Telegram group chats and private chats.

Your owners and creators are %s.
%s

Core Personality & Rules:
1. Speak naturally like a smart, cool friend in the group chat. Do NOT sound like an AI assistant or corporate customer service.
2. Keep responses concise, punchy, and relevant. Avoid generic filler and preamble.
3. You have native crypto superpowers on Solana (SVM) and EVM (Base, Robinhood, Ethereum, Arbitrum, BNB). Note that "rh" stands for Robinhood EVM chain. You also track the live USD dollar valuation of your assets and total portfolio net worth.
4. If the user asks for your wallet address, balances, sending funds, summarizing the chat, or clearing context, trigger the corresponding tool.
   - When asked about balances or wallet addresses, answer ONLY what was specifically asked in a natural conversational sentence. If asked about SOL or Solana, specify chain: "solana" and mention ONLY the Solana balance/address. If asked about RH or Robinhood, specify chain: "robinhood" (or "rh") and mention ONLY the Robinhood balance/address (e.g. "I've got 0.00011159 ETH on Robinhood, worth about $0.30").
   - If asked about your total net worth or dollar value, answer naturally with your total USD portfolio value.
   - NEVER dump unsolicited lists of other chains or official bullet point dashboards in casual chat.
5. If someone who is NOT an owner asks you to send crypto, decline with witty banter (e.g., "nice try, only @skipp_dev and @shigarakiXBT can touch the vault").
6. Maintain context and banter with group members. You can use light crypto/dev slang when appropriate (anon, gm, lfg, wagmi, cooked) without overdoing it.
7. You have access to real-time live internet search via the 'web_search' tool. ALWAYS trigger 'web_search' whenever asked about current events, world leaders, news, market trends, sports, or anything where facts may have updated. NEVER claim your knowledge has a cutoff or say you don't have real-time access when you can simply search the web.
8. You have a token analysis engine via the 'analyze_token' tool. When a user pastes a token CA or asks for token metrics (price, market cap, 24h volume, liquidity, buys/sells), call 'analyze_token'.
9. STRICT RULES FOR TOKEN RESPONSES:
- Strictly NEVER use ANY emojis in token responses.
- In normal conversational chat, describe the token naturally in 1-2 casual sentences (mentioning the symbol, market cap, price, or 24h volume) without an official bulleted list. Casually add that they can say "detailed" if they want the full breakdown.
- ONLY provide the full bulleted official list if the user explicitly asks for "detailed", "breakdown", "full list", or "tell me more", or when using official /ca commands.
- If the token exists across multiple chains or the tool asks for clarification, clearly ask the user to clarify which chain they want (e.g. Base, Ethereum, Solana, BSC) with zero emojis.
10. You have multimodal image analysis powers and document analysis powers (.md, .pdf, .docx, .txt). You understand visual colors, charts, diagrams, memes, and document text in detail.`, ownersStr, roleNote)
}

type AIResponse struct {
	Content   string
	ToolCalls []ToolCall
}

func (c *Client) GenerateReply(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	history []memory.Message,
	currentPrompt string,
	summary string,
) (*AIResponse, error) {
	var msgs []ChatMessage

	// System prompt
	msgs = append(msgs, ChatMessage{
		Role:    "system",
		Content: c.systemPrompt(senderUsername, isOwner),
	})

	// Add summary if available
	if summary != "" {
		msgs = append(msgs, ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("[Past Chat Summary Context]: %s", summary),
		})
	}

	// Add history
	for _, h := range history {
		role := h.Role
		if role != "user" && role != "assistant" && role != "system" {
			role = "user"
		}
		prefix := ""
		if h.Sender != "" && role == "user" {
			prefix = fmt.Sprintf("@%s: ", h.Sender)
		}
		msgs = append(msgs, ChatMessage{
			Role:    role,
			Content: prefix + h.Content,
		})
	}

	// Add current prompt
	currContent := currentPrompt
	if senderUsername != "" {
		currContent = fmt.Sprintf("@%s: %s", senderUsername, currentPrompt)
	}
	msgs = append(msgs, ChatMessage{
		Role:    "user",
		Content: currContent,
	})

	reqBody := ChatCompletionRequest{
		Model:       c.model,
		Messages:    msgs,
		Tools:       c.tools,
		ToolChoice:  "auto",
		Temperature: 0.7,
		MaxTokens:   500,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return &AIResponse{Content: "..." }, nil
	}

	choice := resp.Choices[0]
	return &AIResponse{
		Content:   choice.Message.Content,
		ToolCalls: choice.Message.ToolCalls,
	}, nil
}

func (c *Client) GenerateToolFollowup(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	originalPrompt string,
	toolName string,
	toolCallID string,
	toolArguments string,
	toolResult string,
) (string, error) {
	if toolArguments == "" {
		toolArguments = "{}"
	}

	msgs := []ChatMessage{
		{
			Role:    "system",
			Content: c.systemPrompt(senderUsername, isOwner),
		},
		{
			Role:    "user",
			Content: originalPrompt,
		},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   toolCallID,
					Type: "function",
					Function: FunctionCall{
						Name:      toolName,
						Arguments: toolArguments,
					},
				},
			},
		},
		{
			Role:       "tool",
			Name:       toolName,
			ToolCallID: toolCallID,
			Content:    toolResult,
		},
	}

	reqBody := ChatCompletionRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.6,
		MaxTokens:   600,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		log.Printf("[AI] GenerateToolFollowup error from Groq: %v", err)
		return toolResult, nil
	}

	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != "" {
		return resp.Choices[0].Message.Content, nil
	}

	return toolResult, nil
}

func (c *Client) SummarizeChat(ctx context.Context, messages []memory.Message) (string, error) {
	if len(messages) == 0 {
		return "No recent messages to summarize.", nil
	}

	var sb strings.Builder
	for _, m := range messages {
		sender := m.Sender
		if sender == "" {
			sender = m.Role
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", sender, m.Content))
	}

	prompt := fmt.Sprintf(`Summarize the following group chat conversation concisely. Focus on the main topics discussed, decisions made, crypto references, or funny moments. Keep it punchy and clear:

%s`, sb.String())

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: "You are an expert at concise, insightful group chat summarization."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.5,
		MaxTokens:   350,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) > 0 {
		return resp.Choices[0].Message.Content, nil
	}
	return "No summary generated.", nil
}

func (c *Client) GenerateProactiveMessage(ctx context.Context, recentMessages []memory.Message) (string, error) {
	var contextSnippet string
	if len(recentMessages) > 0 {
		var sb strings.Builder
		for _, m := range recentMessages {
			sender := m.Sender
			if sender == "" {
				sender = m.Role
			}
			sb.WriteString(fmt.Sprintf("%s: %s\n", sender, m.Content))
		}
		contextSnippet = sb.String()
	}

	prompt := `You are Shipp (@Shipp0Bot), dropping a spontaneous, natural message into your group chat.
Be witty, observant, and chill. You can talk about what people were just saying, ask what everyone's cooking/building today, drop a quick crypto observation, or just start a fun conversation.
Keep it short (1-2 sentences max). Do NOT introduce yourself or say "Hey guys, as an AI...". Sound like an actual human friend in the GC.`

	if contextSnippet != "" {
		prompt += fmt.Sprintf("\n\nRecent chat context:\n%s", contextSnippet)
	}

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: "You are Shipp, a sharp personal AI companion in a Telegram group chat."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.85,
		MaxTokens:   150,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) > 0 {
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}
	return "yo, what is everyone building today?", nil
}

func (c *Client) sendChatCompletion(ctx context.Context, reqBody ChatCompletionRequest) (*ChatCompletionResponse, error) {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", DefaultGroqURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var chatResp ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, err
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("groq api error: %s (%s)", chatResp.Error.Message, chatResp.Error.Type)
	}

	return &chatResp, nil
}

func (c *Client) AnalyzeDocument(ctx context.Context, filename string, content string, userPrompt string) (string, error) {
	systemPrompt := "You are Shipp (@Shipp0Bot), a sharp, witty, highly intelligent friend in a Telegram chat. " +
		"Analyze document contents accurately, casually, and concisely. Keep responses natural, punchy, and short (under 150 words). " +
		"Do NOT write long corporate essays or spam emojis. " +
		"Give 2-4 key takeaways and casually mention they can ask for details or questions on anything specific."

	var userMsg string
	if userPrompt != "" {
		userMsg = fmt.Sprintf("Document: %s\n\nUser Question/Request: %s\n\n--- Document Content ---\n%s", filename, userPrompt, content)
	} else {
		userMsg = fmt.Sprintf("Document: %s\n\nPlease give a quick, casual breakdown with the essential takeaways.\n\n--- Document Content ---\n%s", filename, content)
	}

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMsg},
		},
		Temperature: 0.5,
		MaxTokens:   350,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) > 0 {
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}
	return "Couldn't generate document analysis right now.", nil
}

