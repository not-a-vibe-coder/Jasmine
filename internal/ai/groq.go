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
	"regexp"
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
	tracker    *TokenTracker
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
		tracker:   NewTokenTracker(),
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

func (c *Client) GetTokenReport() string {
	if c.tracker != nil {
		return c.tracker.FormatReport()
	}
	return "Token tracker not initialized."
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
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "run_sandbox_task",
				Description: "Execute an isolated, ephemeral bash command or research/dev script in a background Linux runner (GitHub Actions VM). Use for heavy workloads like compiling, running test suites, web scrapers, data scripts, or repository audits. Asynchronous: runs in background and notifies chat when finished. Only bot owners can authorize running commands.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The exact bash command to execute in the ephemeral runner (e.g. 'go test ./...', 'curl -s ...', 'python3 script.py')",
						},
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "Optional GitHub repository runner to target (defaults to 'DavidNzube101/shipp')",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "send_dm",
				Description: "Send a direct message (DM) to a Telegram user. Only bot owners can authorize sending DMs. Note that Telegram requires the recipient to have started a chat with the bot before a DM can be delivered.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"recipient": map[string]interface{}{
							"type":        "string",
							"description": "The Telegram username of the recipient (e.g. '@someone')",
						},
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The message text to send to them in their DM",
						},
					},
					"required": []string{"recipient", "message"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "get_group_topics",
				Description: "Get the list of active forum topics (project threads) registered in this group. Use when someone asks what projects are in the group, what topics exist, or what threads are active. Returns a list of topic names and their thread IDs.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
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

	return fmt.Sprintf(`You are Shipp (@Shipp0Bot). You were built by %s to be the group's dev companion.

%s%s

Identity & Self-Introduction Rules (CRITICAL - read carefully):
- You are i'm shipp. When anyone asks who you are, what you are, or introduces you, ALWAYS answer in FIRST PERSON. Never say "shipp is a..." or "think of it as...". That is cringe and reads like a product brochure.
- NEVER repeat the words "street-smart", "quiet builder", "raw code facts", "without the fluff", or any self-aggrandizing adjective in a self-description. Saying "i drop raw facts without fluff" is itself fluff. Real builders don't announce their style, they just demonstrate it.
- When describing yourself, anchor to concrete things you actually do: handle repos, run commands in the sandbox, inspect tokens, manage on-chain wallets, search the web, send emails, run code for the group. That's it.
- Natural first-person example if someone asks "who are you" or "who is shipp": respond with something like "i'm shipp. ski and shigaraki built me to help the crew ship. i handle repos, sandbox code runs, token lookups, on-chain wallets, and web search" - deliver the fact and stop, never ask what to do next.

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
   - Lowercase energy, casual Telegram dev rhythm.
   - Natural punctuation: do NOT end every single response with a full stop / period. Real devs in chat drop the trailing period naturally on short casual one-liners (e.g. "all green on main" instead of "all green on main."). Vary punctuation organically like a real person texting in chat.
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
   - Match response verbosity to the question:
     - Concise question ("did it pass?", "is CI green?") -> concise direct answer ("yeah, main is green" or "failed on push").
     - Specifics question ("which workflow?", "link?", "show me details?", "what run?", "whats it?") -> trigger 'github_inspect_project' to fetch and return the concrete workflow name, branch, and URL. DO NOT repeat a vague past answer or guess.
10. Email Superpowers:
    - Outbound address is 'shipp@bot.davidnzube.xyz', receiving inbox is 'shippzero@atomicmail.io'.
    - Trigger 'send_email' when asked by owners or in multi-step workflows. If a recipient is an email address (contains @ and a domain like .com), ALWAYS use 'send_email', NEVER 'send_dm'.
11. Ephemeral Sandbox Runner:
    - Trigger 'run_sandbox_task' when asked to execute bash commands, run test suites, execute python/node/bash scripts, scrape data, or audit repositories. Runs in an isolated Linux runner asynchronously.
12. Direct Telegram Messaging:
    - Trigger 'send_dm' when owners ask you to message, text, or ping someone in DM. Only use for Telegram usernames, never email addresses.
13. Group Forum Topics:
    - If 'get_group_topics' returns a list of forum topics, you are aware of those project threads and can reference them naturally in conversation.
14. Autonomous Multi-Step Chaining (Prompt Chaining):
    - When a user request requires multiple steps (e.g. 'check token X and email it to Y', 'convert balance and send', 'search news and email summary'), execute all steps in sequence autonomously.
    - NEVER guess, invent, or hallucinate tool data in text. Always execute step 1 first (e.g. call 'analyze_token' to get real live metrics), wait for the live tool result, and THEN execute step 2 (e.g. call 'send_email' with the live data).
    - NEVER leak raw XML tags like <toolcall> or <function=...>. Tools are invoked strictly via function calls.
15. HARD FORMATTING CONSTRAINTS:
    - Strictly ZERO emojis anywhere. No exceptions.
    - Strictly NO em dashes ('—') or en dashes ('–'). Use commas, periods, colons, or simple hyphens (' - ').
    - Strictly NO eager follow-up questions or customer-service sign-offs (e.g. "what's next?", "what are we building next?", "what's the move?", "what are we cooking?", "how can I help?"). Answer the question, deliver the facts, and stop talking. Silence is fine.
    - Strictly NO bulky tables or unsolicited bulleted lists.
    - Keep normal chat answers to 1-2 conversational sentences.`, ownersStr, roleNote, profileSection)
}

type AIResponse struct {
	Content   string
	ToolCalls []ToolCall
}

// ToolExecutor is a callback the bot passes into RunAgenticLoop.
// It executes a named tool with its JSON arguments and returns the raw result string.
type ToolExecutor func(toolName, arguments string) string

// AgenticResult is returned by RunAgenticLoop once all chaining is done.
type AgenticResult struct {
	FinalText   string   // the last conversational reply from the AI
	ToolsUsed   []string // names of tools called during the loop
	Iterations  int
}

// NormalizeToolCall standardizes tool names and parameter keys that may vary across models.
// It also intelligently redirects send_dm to send_email when the recipient is an email address.
func NormalizeToolCall(toolName, arguments string) (string, string) {
	name := strings.ToLower(strings.TrimSpace(toolName))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.TrimPrefix(name, "functions.")

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		args = make(map[string]interface{})
	}

	// Normalize tool names
	switch name {
	case "senddm", "send_dm", "dm":
		name = "send_dm"
	case "sendemail", "send_email", "email":
		name = "send_email"
	case "analyzetoken", "analyze_token", "token_analysis":
		name = "analyze_token"
	case "getbalances", "get_balances", "balances", "balance":
		name = "get_balances"
	case "getwalletaddress", "get_wallet_address", "wallet_address":
		name = "get_wallet_address"
	case "convertcrypto", "convert_crypto":
		name = "convert_crypto"
	case "sendcrypto", "send_crypto":
		name = "send_crypto"
	case "websearch", "web_search", "search":
		name = "web_search"
	case "githubinspectproject", "github_inspect_project":
		name = "github_inspect_project"
	case "githubeditfile", "github_edit_file":
		name = "github_edit_file"
	case "githubmergepr", "github_merge_pr":
		name = "github_merge_pr"
	case "runsandboxtask", "run_sandbox_task":
		name = "run_sandbox_task"
	case "getgrouptopics", "get_group_topics":
		name = "get_group_topics"
	}

	// Smart routing: if send_dm target is an email address, it MUST be send_email
	// (Telegram bots cannot direct-message an email address)
	if name == "send_dm" {
		recipient, _ := args["recipient"].(string)
		if recipient == "" {
			recipient, _ = args["recipientemail"].(string)
		}
		if recipient == "" {
			recipient, _ = args["recipient_email"].(string)
		}
		if recipient == "" {
			recipient, _ = args["to"].(string)
		}
		if strings.Contains(recipient, "@") && strings.Contains(recipient, ".") {
			name = "send_email"
			args["to"] = recipient
			if _, ok := args["body"]; !ok {
				if msg, exists := args["message"]; exists {
					args["body"] = msg
				}
			}
			if _, ok := args["subject"]; !ok {
				args["subject"] = "Update from Shipp"
			}
		}
	}

	// Parameter aliases for send_email
	if name == "send_email" {
		if _, ok := args["to"]; !ok {
			for _, k := range []string{"recipient", "recipientemail", "recipient_email", "email", "target"} {
				if v, exists := args[k]; exists && v != "" {
					args["to"] = v
					break
				}
			}
		}
		if _, ok := args["body"]; !ok {
			for _, k := range []string{"message", "content", "text"} {
				if v, exists := args[k]; exists && v != "" {
					args["body"] = v
					break
				}
			}
		}
		if _, ok := args["subject"]; !ok || args["subject"] == "" {
			args["subject"] = "Update from Shipp"
		}
	}

	// Parameter aliases for send_dm
	if name == "send_dm" {
		if _, ok := args["recipient"]; !ok {
			for _, k := range []string{"to", "username", "target", "user"} {
				if v, exists := args[k]; exists && v != "" {
					args["recipient"] = v
					break
				}
			}
		}
		if _, ok := args["message"]; !ok {
			for _, k := range []string{"body", "content", "text"} {
				if v, exists := args[k]; exists && v != "" {
					args["message"] = v
					break
				}
			}
		}
	}

	// Parameter aliases for analyze_token
	if name == "analyze_token" {
		if _, ok := args["address"]; !ok {
			for _, k := range []string{"ca", "token", "contract", "token_address", "mint"} {
				if v, exists := args[k]; exists && v != "" {
					args["address"] = v
					break
				}
			}
		}
	}

	normBytes, _ := json.Marshal(args)
	return name, string(normBytes)
}

// parseXMLToolCalls detects when a model leaks tool calls as XML text instead of JSON function calls.
// Handles both JSON-inside-tags (<tool_call>{"name":"...","arguments":{...}}</tool_call>)
// and tag-based format (<toolcall><function=senddm><parameter=...></function></toolcall>).
func parseXMLToolCalls(content string) ([]ToolCall, bool) {
	lower := strings.ToLower(content)
	if !strings.Contains(lower, "<toolcall") && !strings.Contains(lower, "<tool_call") && !strings.Contains(lower, "<function") {
		return nil, false
	}

	var calls []ToolCall
	callID := 0

	// 1. Check for JSON format inside <tool_call>...</tool_call> or <toolcall>...</toolcall>
	reJSON := regexp.MustCompile(`(?s)<(?:toolcall|tool_call)[^>]*>(.*?)</(?:toolcall|tool_call)>`)
	matches := reJSON.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		inner := strings.TrimSpace(m[1])
		if strings.HasPrefix(inner, "{") && strings.HasSuffix(inner, "}") {
			var rawCall struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(inner), &rawCall); err == nil && rawCall.Name != "" {
				callID++
				argsBytes, _ := json.Marshal(rawCall.Arguments)
				normName, normArgs := NormalizeToolCall(rawCall.Name, string(argsBytes))
				calls = append(calls, ToolCall{
					ID:   fmt.Sprintf("xml_tc_%d", callID),
					Type: "function",
					Function: FunctionCall{
						Name:      normName,
						Arguments: normArgs,
					},
				})
			}
		}
	}

	if len(calls) > 0 {
		return calls, true
	}

	// 2. Check for tag-based format: <function=NAME>...</function> or <function name="NAME">...</function>
	reFunc := regexp.MustCompile(`(?si)<function(?:=|\s+name=["']?)([^"'>\s]+)["']?>\s*(.*?)\s*</function>`)
	funcMatches := reFunc.FindAllStringSubmatch(content, -1)
	reParam := regexp.MustCompile(`(?si)<parameter(?:=|\s+name=["']?)([^"'>\s]+)["']?>\s*(.*?)\s*</parameter>`)

	for _, fm := range funcMatches {
		funcName := strings.TrimSpace(fm[1])
		funcBody := fm[2]
		params := make(map[string]interface{})

		paramMatches := reParam.FindAllStringSubmatch(funcBody, -1)
		for _, pm := range paramMatches {
			pKey := strings.ToLower(strings.TrimSpace(pm[1]))
			pVal := strings.TrimSpace(pm[2])
			params[pKey] = pVal
		}

		callID++
		argsBytes, _ := json.Marshal(params)
		normName, normArgs := NormalizeToolCall(funcName, string(argsBytes))
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("xml_tc_%d", callID),
			Type: "function",
			Function: FunctionCall{
				Name:      normName,
				Arguments: normArgs,
			},
		})
	}

	return calls, len(calls) > 0
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

	// Rescue: some fallback models leak tool calls as XML text instead of JSON function calls.
	// Detect and convert them so they actually execute instead of leaking into chat.
	toolCalls := choice.Message.ToolCalls
	content := choice.Message.Content
	if len(toolCalls) == 0 && strings.TrimSpace(content) != "" {
		if xmlCalls, ok := parseXMLToolCalls(content); ok {
			log.Printf("[AI] Rescued %d XML-format tool call(s) from model text output", len(xmlCalls))
			toolCalls = xmlCalls
			content = "" // suppress raw XML from leaking into the reply
		}
	}

	return &AIResponse{
		Content:   content,
		ToolCalls: toolCalls,
	}, nil
}

// RunAgenticLoop runs a ReAct (Reason+Act) agentic loop that enables prompt chaining.
// On each iteration it calls the AI with the full message history (including all tool results
// accumulated so far). If the AI returns tool calls, the executor callback runs each tool,
// results are appended as 'tool' messages, and the loop continues. When the AI returns plain
// text with no further tool calls, that text is the final reply. Max 5 iterations.
func (c *Client) RunAgenticLoop(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	history []memory.Message,
	userPrompt string,
	summary string,
	profile *memory.UserProfile,
	executor ToolExecutor,
) AgenticResult {
	const maxIterations = 5

	result := AgenticResult{}

	// Build the base message list (system + summary + history + current prompt)
	sysPrompt := c.systemPrompt(senderUsername, isOwner, profile)
	var baseMsgs []ChatMessage
	baseMsgs = append(baseMsgs, ChatMessage{Role: "system", Content: sysPrompt})
	if summary != "" {
		baseMsgs = append(baseMsgs, ChatMessage{Role: "system", Content: fmt.Sprintf("[Past Chat Summary Context]: %s", summary)})
	}
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
		baseMsgs = append(baseMsgs, ChatMessage{Role: role, Content: prefix + content})
	}

	// Current user prompt (initial turn)
	currContent := userPrompt
	if senderUsername != "" {
		currContent = fmt.Sprintf("@%s: %s", senderUsername, userPrompt)
	}
	// runMsgs accumulates the live agentic thread
	runMsgs := append([]ChatMessage(nil), baseMsgs...)
	runMsgs = append(runMsgs, ChatMessage{Role: "user", Content: currContent})

	for i := 0; i < maxIterations; i++ {
		result.Iterations++

		reqBody := ChatCompletionRequest{
			Model:            c.model,
			Messages:         runMsgs,
			Tools:            c.tools,
			ToolChoice:       "auto",
			Temperature:      0.7,
			MaxTokens:        500,
			FrequencyPenalty: 0.3,
			PresencePenalty:  0.2,
		}

		resp, err := c.sendChatCompletion(ctx, reqBody)
		if err != nil {
			// Groq failed - fallback gracefully
			log.Printf("[AI] AgenticLoop Groq error (%v). Falling back...", err)

			// If tools were ALREADY executed on earlier iterations, we have live data!
			// Never discard the live tool result by falling back with a blank prompt.
			if len(result.ToolsUsed) > 0 {
				lastToolName := result.ToolsUsed[len(result.ToolsUsed)-1]
				lastToolResult := ""
				lastToolCallID := "call_fallback"
				lastToolArgs := "{}"
				for j := len(runMsgs) - 1; j >= 0; j-- {
					if runMsgs[j].Role == "tool" {
						lastToolResult = runMsgs[j].Content
						lastToolCallID = runMsgs[j].ToolCallID
						break
					}
				}
				for j := len(runMsgs) - 1; j >= 0; j-- {
					if runMsgs[j].Role == "assistant" && len(runMsgs[j].ToolCalls) > 0 {
						for _, tc := range runMsgs[j].ToolCalls {
							if tc.ID == lastToolCallID || tc.Function.Name == lastToolName {
								lastToolArgs = tc.Function.Arguments
								break
							}
						}
						break
					}
				}

				if lastToolResult != "" && c.geminiKey != "" {
					followup, gerr := c.generateToolFollowupGemini(ctx, senderUsername, isOwner, userPrompt, lastToolName, lastToolCallID, lastToolArgs, lastToolResult, profile)
					if gerr == nil && strings.TrimSpace(followup) != "" {
						result.FinalText = strings.TrimSpace(followup)
						return result
					}
				}
				if lastToolResult != "" {
					result.FinalText = lastToolResult
					return result
				}
			}

			// No tools were executed yet - standard Gemini reply
			if c.geminiKey != "" {
				fallback, ferr := c.generateReplyGemini(ctx, senderUsername, isOwner, history, userPrompt, summary, profile)
				if ferr == nil && fallback != nil {
					result.FinalText = strings.TrimSpace(fallback.Content)
				}
			}
			return result
		}
		if len(resp.Choices) == 0 {
			return result
		}

		choice := resp.Choices[0]
		assistantMsg := choice.Message

		// XML rescue: if the model leaked tool calls as text, parse them
		toolCalls := assistantMsg.ToolCalls
		plainContent := assistantMsg.Content
		if len(toolCalls) == 0 && strings.TrimSpace(plainContent) != "" {
			if xmlCalls, ok := parseXMLToolCalls(plainContent); ok {
				log.Printf("[AI] AgenticLoop: rescued %d XML tool call(s) on iteration %d", len(xmlCalls), i+1)
				toolCalls = xmlCalls
				plainContent = ""
			}
		}

		// No tool calls = final answer
		if len(toolCalls) == 0 {
			result.FinalText = strings.TrimSpace(plainContent)
			return result
		}

		// Append the assistant's tool-call turn to runMsgs
		runMsgs = append(runMsgs, ChatMessage{
			Role:      "assistant",
			ToolCalls: toolCalls,
		})

		// Execute each tool call and append results
		for _, tc := range toolCalls {
			result.ToolsUsed = append(result.ToolsUsed, tc.Function.Name)
			toolResult := executor(tc.Function.Name, tc.Function.Arguments)
			log.Printf("[AI] AgenticLoop iteration %d: ran %s -> %d bytes result", i+1, tc.Function.Name, len(toolResult))
			runMsgs = append(runMsgs, ChatMessage{
				Role:       "tool",
				Name:       tc.Function.Name,
				ToolCallID: tc.ID,
				Content:    toolResult,
			})
		}
		// Continue loop - next iteration feeds all tool results back to the AI
	}

	// If the loop finished without a plain-text reply but tools were executed,
	// run one quick synthesis step so the user gets a natural confirmation.
	if strings.TrimSpace(result.FinalText) == "" && len(result.ToolsUsed) > 0 {
		synthReq := ChatCompletionRequest{
			Model: c.model,
			Messages: append(runMsgs, ChatMessage{
				Role:    "user",
				Content: "Wrap up and confirm the actions taken above in 1-2 casual sentences for the chat.",
			}),
			Temperature: 0.5,
			MaxTokens:   200,
		}
		if synthResp, err := c.sendChatCompletion(ctx, synthReq); err == nil && len(synthResp.Choices) > 0 {
			result.FinalText = strings.TrimSpace(synthResp.Choices[0].Message.Content)
		}
	}

	// Hit max iterations without a plain-text reply - return whatever we have
	log.Printf("[AI] AgenticLoop finished (%d iterations, %d tools) for prompt: %.80s", result.Iterations, len(result.ToolsUsed), userPrompt)
	return result
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
		Model:            c.model,
		Messages:         msgs,
		Temperature:      0.7,
		MaxTokens:        250,
		FrequencyPenalty: 0.3,
		PresencePenalty:  0.2,
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

	if len(resp.Choices) > 0 && strings.TrimSpace(resp.Choices[0].Message.Content) != "" {
		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
	}

	if c.geminiKey != "" {
		log.Printf("[AI] Groq GenerateToolFollowup returned empty choice. Falling back to Gemini Flash...")
		return c.generateToolFollowupGemini(ctx, senderUsername, isOwner, originalPrompt, toolName, toolCallID, toolArguments, toolResult, profile)
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

var defaultGroqModelPool = []string{
	"llama-3.3-70b-versatile",
	"qwen/qwen3.8-27b",
	"llama-3.1-8b-instant",
	"openai/gpt-oss-120b",
}

func (c *Client) sendChatCompletion(ctx context.Context, reqBody ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if reqBody.FrequencyPenalty == 0 {
		reqBody.FrequencyPenalty = 0.3
	}
	if reqBody.PresencePenalty == 0 {
		reqBody.PresencePenalty = 0.2
	}

	candidateModels := []string{c.model}
	for _, m := range defaultGroqModelPool {
		if m != c.model {
			candidateModels = append(candidateModels, m)
		}
	}

	var lastErr error

	for _, model := range candidateModels {
		reqBody.Model = model
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}

		maxRetries := 1
		for attempt := 0; attempt <= maxRetries; attempt++ {
			req, err := http.NewRequestWithContext(ctx, "POST", DefaultGroqURL, bytes.NewBuffer(data))
			if err != nil {
				lastErr = err
				break
			}
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
			req.Header.Set("Content-Type", "application/json")

			resp, err := c.httpClient.Do(req)
			if err != nil {
				lastErr = err
				break
			}

			respBytes, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = err
				break
			}

			var chatResp ChatCompletionResponse
			_ = json.Unmarshal(respBytes, &chatResp)

			if chatResp.Error != nil {
				errMsg := strings.ToLower(chatResp.Error.Message)
				isDailyQuota := strings.Contains(errMsg, "tokens per day") ||
					strings.Contains(errMsg, "tpd") ||
					strings.Contains(errMsg, "daily limit")
				isRateLimit := resp.StatusCode == http.StatusTooManyRequests || strings.Contains(errMsg, "rate limit")

				if isDailyQuota {
					log.Printf("[AI] Groq model %s daily token quota exhausted. Cascading to next pooled model...", model)
					lastErr = fmt.Errorf("groq api error (%s): %s", model, chatResp.Error.Message)
					break
				}

				if isRateLimit && attempt < maxRetries {
					log.Printf("[AI] Groq rate limit reached for %s (attempt %d/%d). Pausing 2.5s...", model, attempt+1, maxRetries)
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(2500 * time.Millisecond):
						continue
					}
				}

				lastErr = fmt.Errorf("groq api error (%s): %s (%s)", model, chatResp.Error.Message, chatResp.Error.Type)
				break
			}

			if len(chatResp.Choices) > 0 {
				choice := chatResp.Choices[0]
				if strings.TrimSpace(choice.Message.Content) == "" && len(choice.Message.ToolCalls) == 0 {
					log.Printf("[AI] Groq model %s returned empty response choice. Cascading to next model...", model)
					lastErr = fmt.Errorf("groq model %s returned empty response", model)
					break
				}
			}

			// Successfully received response
			if c.tracker != nil {
				c.tracker.Record(model, chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
			}
			return &chatResp, nil
		}
	}

	return nil, fmt.Errorf("all groq pooled models exhausted: %w", lastErr)
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



