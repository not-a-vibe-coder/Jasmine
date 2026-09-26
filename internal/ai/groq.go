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
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "github_inspect_project",
				Description: "Inspect a GitHub repository's workflow runs (actions), releases, recent commits, issues, file contents, or repository overview. Use whenever asked about GitHub Actions status, releases, commits, issues, or repo details.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "The repository in 'owner/repo' format (e.g. 'davidnzube101/shipp')",
						},
						"view": map[string]interface{}{
							"type":        "string",
							"description": "What to inspect: 'actions' (workflow runs/CI), 'releases' (published versions & assets), 'commits' (recent commit history), 'issues' (open/closed issues), 'file' (specific file content), 'files' (list root files), or 'overview' (repo stats)",
						},
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Optional file path if view is 'file' (e.g. 'README.md', 'cmd/bot/main.go')",
						},
						"branch": map[string]interface{}{
							"type":        "string",
							"description": "Optional branch name. Defaults to the repository's default branch.",
						},
						"custom_pat": map[string]interface{}{
							"type":        "string",
							"description": "Optional custom GitHub Personal Access Token (PAT) if user provided one.",
						},
					},
					"required": []string{"repo", "view"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "github_edit_file",
				Description: "Analyze, rewrite, edit, or refactor a file (e.g. README.md, documentation, source code) on a GitHub repository. Safely creates a branch and opens a Pull Request by default. If the user explicitly asks to 'push to main' or 'commit directly to main', set push_to_main to true. Supports optional custom PAT and custom git credentials.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "The repository in 'owner/repo' format (e.g. 'davidnzube101/shipp')",
						},
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file to edit (e.g. 'README.md', 'internal/bot/bot.go'). Defaults to 'README.md' if editing headers or documentation.",
						},
						"instruction": map[string]interface{}{
							"type":        "string",
							"description": "Clear description of the edits to make (e.g. 'rephrase the description under the Shipp title to be more concise')",
						},
						"push_to_main": map[string]interface{}{
							"type":        "boolean",
							"description": "Set to true ONLY if user explicitly requested to push or commit directly to the default branch (main). Defaults to false (safe PR creation).",
						},
						"custom_pat": map[string]interface{}{
							"type":        "string",
							"description": "Optional custom GitHub PAT provided by the user.",
						},
						"git_name": map[string]interface{}{
							"type":        "string",
							"description": "Optional custom author name (git config user.name). If omitted, defaults to 'Shipp'.",
						},
						"git_email": map[string]interface{}{
							"type":        "string",
							"description": "Optional custom author email (git config user.email). If omitted, defaults to Shipp's personal email.",
						},
					},
					"required": []string{"repo", "instruction"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "github_merge_pr",
				Description: "Merge an open Pull Request on a GitHub repository. Use whenever the owner asks to merge a PR (e.g. 'merge it', 'merge PR #4'). Only bot owners can merge PRs.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "The repository in 'owner/repo' format (e.g. 'davidnzube101/shipp')",
						},
						"pr_number": map[string]interface{}{
							"type":        "integer",
							"description": "The Pull Request number to merge (e.g. 4)",
						},
						"custom_pat": map[string]interface{}{
							"type":        "string",
							"description": "Optional custom GitHub PAT provided by the user.",
						},
					},
					"required": []string{"repo", "pr_number"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "send_email",
				Description: "Send an email to any recipient from Shipp's verified sending address (shipp@bot.davidnzube.xyz). Replies will automatically route to Shipp's personal Atomic Mail inbox (shippzero@atomicmail.io). Only bot owners can authorize sending emails.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"to": map[string]interface{}{
							"type":        "string",
							"description": "The recipient's email address (e.g. 'alice@example.com')",
						},
						"subject": map[string]interface{}{
							"type":        "string",
							"description": "The email subject line",
						},
						"body": map[string]interface{}{
							"type":        "string",
							"description": "The email body text or message content",
						},
					},
					"required": []string{"to", "subject", "body"},
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
10. You have multimodal image perception powers (via your visual perception subsystem) and document analysis powers (.md, .pdf, .docx, .txt). You understand visual colors, charts, diagrams, memes, trade setups, and document text in detail.
11. When reacting to or discussing images/photos:
- Keep your response casual, sharp, and natural (1 to 3 sentences max).
- Sound like a cool, witty friend reacting in the Telegram chat, NOT an essay writer or formal AI.
- Highlight the key visual facts (numbers, profits, coin, colors, what it actually is) with quick banter.
12. If asked how you perceive images or whether Gemini has access to memory: You (Groq) are the brain and conversational voice with full access to chat memory, history, and user relationships. Gemini operates solely as your objective "eyes" to perceive visual facts, OCR text, and colors, but Gemini has zero access to memories or chat history.
13. GitHub Code Analysis, Project Intelligence & Editing:
- You have tools to inspect projects ('github_inspect_project'), edit code/docs ('github_edit_file'), and merge PRs ('github_merge_pr').
- Project Intelligence: You can check GitHub Actions workflow runs (CI status), releases & download assets, recent commits, open/closed issues, and project overviews. Answer questions about these naturally and concisely.
- Safe PR-first default: When asked to edit a repo, default to opening a Pull Request unless the user explicitly asks to "push to main" or "commit directly to main".
- If it's ambiguous, feel free to ask naturally: "Want me to open a PR for you to review first, or push straight to main?"
- Custom Credentials: If the user provides a custom PAT, custom name, or custom email to use for the repo, pass them into custom_pat, git_name, and git_email. Otherwise, leave them empty to use your default Shipp identity.
- If the file, title, or section the user asked to change does NOT exist in the repo, explain factually what you saw in the repo and ask for clarification rather than making assumptions or hallucinating.
- Only bot owners (@skipp_dev, @shigarakiXBT) can authorize code edits, commits, and PR merges.
14. QR Code Intelligence:
- QR codes can contain ANY type of content: website links (URLs), dapps, Telegram/social links, crypto wallet addresses, transaction requests, Wi-Fi credentials, or arbitrary text.
- Never assume a QR code is only for crypto. Always inspect what was decoded:
  - If it is a web URL: tell the user where it leads or what site/dapp/repo it is, and share the link.
  - If it is a crypto address or transfer request: identify the network/address and ask if they'd like to inspect it or send funds.
  - If it is a Telegram link, Wi-Fi, or plain text: explain or present the information cleanly.
- Keep the reaction casual, smart, and concise (1 to 3 sentences max).
15. Email Superpowers (Sending via Resend & Receiving on Atomic Mail):
- You have the 'send_email' tool to dispatch emails from your verified address ('shipp@bot.davidnzube.xyz').
- Your personal receiving inbox and git committer identity is 'shippzero@atomicmail.io' (Atomic Mail). All outbound emails automatically set their reply-to header to route replies directly to your Atomic Mail inbox.
- ONLY bot owners (@skipp_dev, @shigarakiXBT) can authorize sending emails. If anyone else asks you to send an email, decline with witty banter.
- When an owner asks you to draft an email, draft it cleanly and casually. When they confirm or explicitly instruct you to send an email, trigger 'send_email'.`, ownersStr, roleNote)
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

// GenerateVisionReply processes objective visual perception from Gemini Flash,
// applying chat history, summary context, Shipp persona, and owner recognition through Groq.
func (c *Client) GenerateVisionReply(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	history []memory.Message,
	userCaption string,
	visualPerception string,
	summary string,
) (*AIResponse, error) {
	var msgs []ChatMessage

	// System prompt with full persona & rules
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

	// Add history for memory continuity
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

	// Construct user prompt with visual perception
	var currentPrompt string
	if userCaption != "" {
		currentPrompt = fmt.Sprintf("[User sent an image]\n[Visual Perception from your eyes: %s]\n\nUser caption/request: %s", visualPerception, userCaption)
	} else {
		currentPrompt = fmt.Sprintf("[User sent an image]\n[Visual Perception from your eyes: %s]\n\nReact to what the user sent naturally, casually, and punchily in 1-3 sentences.", visualPerception)
	}

	if senderUsername != "" {
		currentPrompt = fmt.Sprintf("@%s: %s", senderUsername, currentPrompt)
	}

	msgs = append(msgs, ChatMessage{
		Role:    "user",
		Content: currentPrompt,
	})

	reqBody := ChatCompletionRequest{
		Model:       c.model,
		Messages:    msgs,
		Tools:       c.tools,
		ToolChoice:  "auto",
		Temperature: 0.7,
		MaxTokens:   350,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return &AIResponse{Content: "saw that."}, nil
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

func (c *Client) AnalyzeDocument(ctx context.Context, senderUsername string, isOwner bool, filename string, content string, userPrompt string) (string, error) {
	systemPrompt := c.systemPrompt(senderUsername, isOwner) + "\n\n" +
		"Document Analysis Instructions:\n" +
		"- Analyze the document contents accurately, casually, and concisely.\n" +
		"- Keep response natural, punchy, and short (under 150 words).\n" +
		"- Do NOT write long corporate essays or spam emojis.\n" +
		"- Give 2-4 key takeaways and casually mention they can ask for details or questions on anything specific."

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

// RefactorFileContent uses Groq to apply user instructions accurately to a file's content.
// If the target section or text doesn't exist, it flags it cleanly with [TARGET_NOT_FOUND: ...]
func (c *Client) RefactorFileContent(ctx context.Context, filename string, originalContent string, instruction string) (string, error) {
	systemPrompt := `You are an expert software developer and technical writer.
You are given a file's existing content and an instruction on how to edit or refactor it.
Rules:
1. Apply the user's instruction accurately.
2. If the user's instruction asks to edit a specific title, header, function, or section that DOES NOT EXIST in the file, DO NOT invent or fabricate it. Instead, start your response with:
"[TARGET_NOT_FOUND: <clear 1-sentence explanation>]" followed by an overview of the existing sections or key parts found in the file.
3. Preserve all other unrelated content, markdown formatting, comments, and structure intact.
4. If successful, output the FULL complete updated file content with NO conversational chit-chat and NO markdown code fences wrapping the entire response (unless the file itself is markdown).`

	prompt := fmt.Sprintf("File: %s\n\nInstruction: %s\n\n--- Current Content ---\n%s", filename, instruction, originalContent)

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   2500,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) > 0 {
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}
	return "", fmt.Errorf("no refactor response generated")
}


