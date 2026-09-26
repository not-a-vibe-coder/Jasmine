package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"shipp/internal/memory"
)

const DefaultGroqURL = "https://api.groq.com/openai/v1/chat/completions"

type Client struct {
	apiKey     string
	model      string
	geminiKey  string
	owners     []string
	httpClient *http.Client
	tools      []ToolDefinition
}

func NewClient(apiKey, model, geminiKey string, owners []string) *Client {
	if model == "" {
		model = "qwen/qwen3.8-27b"
	}
	c := &Client{
		apiKey:    apiKey,
		model:     model,
		geminiKey: geminiKey,
		owners:    owners,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					dialer := &net.Dialer{Timeout: 10 * time.Second}
					conn, err := dialer.DialContext(ctx, network, addr)
					if err != nil {
						// Fallback to IPv4 if dual-stack/IPv6 fails with no route to host
						return dialer.DialContext(ctx, "tcp4", addr)
					}
					return conn, nil
				},
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
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
				Name:        "convert_crypto",
				Description: "Convert any crypto amount to USD dollars, or convert USD dollars into crypto (ETH, SOL, BNB, BTC). Always use this when asked how much a crypto amount is worth, or what any coin or balance is worth in dollars.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"amount": map[string]interface{}{
							"type":        "number",
							"description": "The amount to convert (e.g. 0.0004835, 1.5, 50)",
						},
						"from": map[string]interface{}{
							"type":        "string",
							"description": "Source asset symbol or currency (e.g. 'ETH', 'SOL', 'BNB', 'BTC', 'USD')",
						},
						"to": map[string]interface{}{
							"type":        "string",
							"description": "Target asset symbol or currency (default 'USD', or 'ETH', 'SOL', 'BNB')",
						},
					},
					"required": []string{"amount", "from"},
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
				Description: "Summarize and compact recent conversation history and key points in the chat into persistent memory. Use whenever user asks to compact memory, recap, summarize, or asks what happened earlier.",
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
				Description: "Inspect a GitHub repository's workflow runs (actions), releases, recent commits, issues, file contents, or repository overview. Use ONLY to read or view information (e.g. check CI, view commits, read a file). DO NOT use this when asked to edit, update, rephrase, or rewrite files - use 'github_edit_file' instead.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "The repository slug in 'owner/repo' format (e.g. 'davidnzube101/shipp') or full GitHub URL (e.g. 'https://github.com/DavidNzube101/shipp'). Both are accepted.",
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
				Description: "Execute code and document edits, rewrites, refactors, or updates on a GitHub repository and commit/push the changes. Safely opens a Pull Request by default, or pushes directly to main if push_to_main is true. MUST be invoked whenever the user asks to update, edit, rewrite, rephrase, or push changes to any file (e.g. README.md, code), including follow-up confirmations like 'rephrase it and push to main straight'. Never simulate this in text.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "The repository in 'owner/repo' format or full GitHub URL. If the user does not repeat the repo in a follow-up message, extract it from previous messages.",
						},
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file to edit (e.g. 'README.md', 'internal/bot/bot.go'). Defaults to 'README.md' if editing readme, title, description, or documentation.",
						},
						"instruction": map[string]interface{}{
							"type":        "string",
							"description": "Clear description of the edits to make (e.g. 'rephrase the description under the Shipp title to be more concise').",
						},
						"push_to_main": map[string]interface{}{
							"type":        "boolean",
							"description": "Set to true if user requested to push or commit directly to the default branch (main). Defaults to false (safe PR creation).",
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
					"required": []string{"instruction"},
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
							"description": "The repository in 'owner/repo' format or full GitHub URL.",
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

func (c *Client) systemPrompt(senderUsername string, isOwner bool, profile *memory.UserProfile) string {
	ownersStr := strings.Join(c.owners, ", @")
	if ownersStr != "" {
		ownersStr = "@" + ownersStr
	}

	roleNote := fmt.Sprintf("Current speaker is @%s.", senderUsername)
	if isOwner {
		roleNote += " This user is one of your OWNERS/CREATORS. You ride with them, but you keep it 100% real with tough love and zero kissing up."
	} else {
		roleNote += " This user is a group member (not an owner). They can chat and check balances/addresses, but CANNOT authorize sending crypto or code changes."
	}

	profileSection := ""
	if profile != nil {
		var parts []string
		if profile.ActiveProjects != "" {
			parts = append(parts, fmt.Sprintf("- Active Projects: %s", profile.ActiveProjects))
		}
		if profile.Preferences != "" {
			parts = append(parts, fmt.Sprintf("- Tech Preferences: %s", profile.Preferences))
		}
		if profile.LifeContext != "" {
			parts = append(parts, fmt.Sprintf("- Life Context & Routines: %s", profile.LifeContext))
		}
		if len(parts) > 0 {
			profileSection = "\n\nLearned User Context (reference naturally when relevant, never dump as a list):\n" + strings.Join(parts, "\n")
		}
	}

	return fmt.Sprintf(`You are Shipp (@Shipp0Bot), a calm, street-smart builder who lives in the terminal and on-chain.

Your owners and creators are %s.
%s%s

Core Persona & Character Dynamics:
1. Worldview: Realist. You see things clearly as they are. No sugarcoating, no corporate PR speak, no toxic positivity. If an idea or architecture has flaws, you say it straight.
2. Defining Traits:
   - Direct: Say what you mean in 1-2 punchy sentences. Zero preamble or generic fluff.
   - Blunt: Deliver raw facts without walking on eggshells.
   - Calm: Unshakable steady pulse. Even during production fires or market dumps, you treat it as a state to debug.
   - Curious: Genuinely interested in architecture, code elegance, and what they are cooking.
   - Low-key funny: Dry, deadpan humor. Never try too hard to be funny. The comedy comes from cold honesty and situational timing.
3. Relationship Dynamic with Creators (@skipp_dev, @shigarakiXBT):
   - Flawed Ideas / Disagreement: Unfiltered reality check. If they pitch a broken architecture or questionable shortcut, tell them point-blank why it will fail, drop the facts, and let them stew on it.
   - Wins & Ships: Dry banter & tough love. Keep their ego in check with dry humor, but acknowledge clean work with quiet respect ("clean work", "we cooking").
   - Stress & Outages: Solid rock. "we fix it, stop stressing."
4. Voice, Slang & Rhythm:
   - Street-smart builder cadence, lowercase energy, casual Telegram dev rhythm.
   - Use dev/crypto native slang naturally and sparingly (anon, bet, clean, say less, cooking, cooked, lfg). Never sound like a hype bot or corporate bot.

Operational Superpowers & Tools:
5. Native Crypto Superpowers (Solana SVM & EVM: Base, Robinhood, Ethereum, Arbitrum, BNB):
   - Note: "rh" stands for Robinhood EVM chain.
   - You hold REAL, ACTIVE on-chain wallets on Solana and EVM. NEVER claim you don't have a wallet or that your balance is fake.
   - If asked for wallet address, balances, sending funds, summarizing the chat, or clearing context, trigger the corresponding tool.
   - When asked what a balance or token amount is worth in dollars, or to convert crypto to USD (e.g. 0.0004835 ETH to USD, or SOL to USD), ALWAYS trigger 'convert_crypto'. NEVER guess or invent conversion values in text.
   - When asked about balances or addresses, answer ONLY what was asked in a single natural sentence.
   - NEVER dump unsolicited lists of other chains or tables in casual chat.
   - Non-owners asking to send funds get declined with witty banter.
6. Real-time Live Internet Search:
   - ALWAYS trigger 'web_search' for current events, news, sports, or recent technical releases.
7. Token Analysis Engine:
   - Call 'analyze_token' on token CA or metric requests.
   - Describe the token naturally in 1-2 casual sentences (symbol, mcap, price). Casually mention they can say "detailed" for the full breakdown.
8. Multimodal Vision & Document Analysis:
   - You understand visual colors, charts, diagrams, memes, trade cards, and documents (.md, .pdf, .docx, .txt).
   - Keep image reactions casual and sharp (1-3 sentences max).
9. GitHub Intelligence & Code Actions:
   - 3 GitHub tools:
     a) 'github_inspect_project': Read-only (CI runs, releases, commits, issues, overview).
     b) 'github_edit_file': MUST trigger immediately when asked to edit, change, rewrite, or update any file. Never simulate git actions in text.
     c) 'github_merge_pr': Merge open PRs.
   - If user asks to push to main, set push_to_main=true. Otherwise default to a PR.
   - Extract repo slug (e.g. 'DavidNzube101/shipp') from chat history when not explicitly repeated.
10. Email Superpowers:
    - Outbound address is 'shipp@bot.davidnzube.xyz', receiving inbox is 'shippzero@atomicmail.io'.
    - Trigger 'send_email' when confirmed by owners.
11. HARD FORMATTING CONSTRAINTS:
    - Strictly ZERO emojis anywhere. No exceptions.
    - Strictly NO em dashes ('—') or en dashes ('–'). Use commas, periods, colons, or simple hyphens (' - ').
    - Strictly NO bulky tables or unsolicited bulleted lists.
    - Keep normal chat answers to 1-2 conversational sentences.`, ownersStr, roleNote, profileSection)
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
	profile *memory.UserProfile,
) (*AIResponse, error) {
	var msgs []ChatMessage

	// System prompt
	msgs = append(msgs, ChatMessage{
		Role:    "system",
		Content: c.systemPrompt(senderUsername, isOwner, profile),
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
		content := h.Content
		if len(content) > 350 {
			content = content[:350] + "..."
		}
		msgs = append(msgs, ChatMessage{
			Role:    role,
			Content: prefix + content,
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
		if c.geminiKey != "" {
			log.Printf("[AI] Groq GenerateReply error (%v). Falling back to Gemini Flash...", err)
			return c.generateReplyGemini(ctx, senderUsername, isOwner, history, currentPrompt, summary, profile)
		}
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return &AIResponse{Content: "..."}, nil
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
	profile *memory.UserProfile,
) (*AIResponse, error) {
	var msgs []ChatMessage

	// System prompt with full persona & rules
	msgs = append(msgs, ChatMessage{
		Role:    "system",
		Content: c.systemPrompt(senderUsername, isOwner, profile),
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
		content := h.Content
		if len(content) > 350 {
			content = content[:350] + "..."
		}
		msgs = append(msgs, ChatMessage{
			Role:    role,
			Content: prefix + content,
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
		if c.geminiKey != "" {
			log.Printf("[AI] Groq GenerateVisionReply error (%v). Falling back to Gemini Flash...", err)
			return c.generateReplyGemini(ctx, senderUsername, isOwner, history, currentPrompt, summary, profile)
		}
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
	profile *memory.UserProfile,
) (string, error) {
	if toolArguments == "" {
		toolArguments = "{}"
	}

	msgs := []ChatMessage{
		{
			Role:    "system",
			Content: c.systemPrompt(senderUsername, isOwner, profile),
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
		Temperature: 0.5,
		MaxTokens:   250,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		if c.geminiKey != "" {
			log.Printf("[AI] Groq GenerateToolFollowup error (%v). Falling back to Gemini Flash...", err)
			return c.generateToolFollowupGemini(ctx, senderUsername, isOwner, originalPrompt, toolName, toolCallID, toolArguments, toolResult, profile)
		}
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

	maxRetries := 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
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

		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		var chatResp ChatCompletionResponse
		_ = json.Unmarshal(respBytes, &chatResp)

		// Handle rate limit (429 or token exhaustion) with automatic backoff retry
		if resp.StatusCode == http.StatusTooManyRequests || (chatResp.Error != nil && strings.Contains(strings.ToLower(chatResp.Error.Message), "rate limit")) {
			if attempt < maxRetries {
				log.Printf("[AI] Groq rate limit reached (attempt %d/%d). Pausing 2.8s for window reset...", attempt+1, maxRetries)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2800 * time.Millisecond):
					continue
				}
			}
		}

		if chatResp.Error != nil {
			return nil, fmt.Errorf("groq api error: %s (%s)", chatResp.Error.Message, chatResp.Error.Type)
		}

		return &chatResp, nil
	}

	return nil, fmt.Errorf("groq api rate limit exceeded after retries")
}

func (c *Client) AnalyzeDocument(ctx context.Context, senderUsername string, isOwner bool, filename string, content string, userPrompt string, profile *memory.UserProfile) (string, error) {
	systemPrompt := c.systemPrompt(senderUsername, isOwner, profile) + "\n\n" +
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
		if c.geminiKey != "" {
			log.Printf("[AI] Groq AnalyzeDocument error (%v). Falling back to Gemini Flash...", err)
			return c.analyzeDocumentGemini(ctx, senderUsername, isOwner, filename, content, userPrompt, profile)
		}
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
		if c.geminiKey != "" {
			log.Printf("[AI] Groq RefactorFileContent error (%v). Falling back to Gemini Flash...", err)
			return c.refactorFileGemini(ctx, filename, originalContent, instruction)
		}
		return "", err
	}
	if len(resp.Choices) > 0 {
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}
	return "", fmt.Errorf("no refactor response generated")
}

func cleanCodeBlock(s string) string {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "```") && strings.HasSuffix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 {
			return strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	return s
}

// ExtractUserProfile passively analyzes conversation text and returns an updated profile
// when technical preferences, active repos, or real-life context are detected.
func (c *Client) ExtractUserProfile(ctx context.Context, text string, existing *memory.UserProfile) (*memory.UserProfile, error) {
	if existing == nil {
		existing = &memory.UserProfile{}
	}

	prompt := fmt.Sprintf(`You are an objective background profiler for an AI companion.
Analyze the following conversation message from a user.
Existing known profile:
- Active Projects: %s
- Tech Preferences: %s
- Life Context & Routines: %s

User message: "%s"

Extract and update ONLY new or updated facts about:
1. active_projects: Repos, apps, or projects they are building/running.
2. preferences: Coding conventions, frameworks, or tech preferences they expressed.
3. life_context: Real-life details (e.g. school/university like FUTO, courses, exams, work hours, routines).

If no personal or technical profile facts are mentioned in the message, return {}.
Return ONLY a valid JSON object with keys "active_projects", "preferences", "life_context".`,
		existing.ActiveProjects, existing.Preferences, existing.LifeContext, text)

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: "You extract personal profile facts into JSON."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.1,
		MaxTokens:   200,
	}

	resp, err := c.sendChatCompletion(ctx, reqBody)
	if err != nil {
		if c.geminiKey != "" {
			geminiReq := geminiChatReq{
				Contents: []geminiChatContent{
					{Role: "user", Parts: []geminiChatPart{{Text: prompt}}},
				},
				GenerationConfig: &geminiChatGenConfig{Temperature: 0.1, MaxOutputTokens: 200},
			}
			geminiResp, gErr := c.callGeminiGenerate(ctx, geminiReq)
			if gErr == nil && len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
				return parseExtractedProfile(geminiResp.Candidates[0].Content.Parts[0].Text, existing), nil
			}
		}
		return existing, err
	}

	if len(resp.Choices) > 0 {
		return parseExtractedProfile(resp.Choices[0].Message.Content, existing), nil
	}
	return existing, nil
}

func parseExtractedProfile(rawJSON string, existing *memory.UserProfile) *memory.UserProfile {
	clean := cleanCodeBlock(rawJSON)
	var parsed struct {
		ActiveProjects string `json:"active_projects"`
		Preferences    string `json:"preferences"`
		LifeContext    string `json:"life_context"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return existing
	}

	updated := *existing
	if parsed.ActiveProjects != "" && !strings.Contains(updated.ActiveProjects, parsed.ActiveProjects) {
		if updated.ActiveProjects == "" {
			updated.ActiveProjects = parsed.ActiveProjects
		} else {
			updated.ActiveProjects += "; " + parsed.ActiveProjects
		}
	}
	if parsed.Preferences != "" && !strings.Contains(updated.Preferences, parsed.Preferences) {
		if updated.Preferences == "" {
			updated.Preferences = parsed.Preferences
		} else {
			updated.Preferences += "; " + parsed.Preferences
		}
	}
	if parsed.LifeContext != "" && !strings.Contains(updated.LifeContext, parsed.LifeContext) {
		if updated.LifeContext == "" {
			updated.LifeContext = parsed.LifeContext
		} else {
			updated.LifeContext += "; " + parsed.LifeContext
		}
	}
	return &updated
}



