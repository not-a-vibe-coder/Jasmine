package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/ai"
	"shipp/internal/config"
	"shipp/internal/crypto"
	"shipp/internal/memory"
	"shipp/internal/search"
	"shipp/internal/token"
)

type Bot struct {
	api         *tgbotapi.BotAPI
	cfg         *config.Config
	ai          *ai.Client
	memory      memory.Store
	crypto      *crypto.Service
	search      *search.Service
	token       *token.Service
	updatesChan chan tgbotapi.Update

	// Proactive settings per chat
	proactiveMu       sync.RWMutex
	proactiveDisabled map[int64]bool
	lastProactiveTime map[int64]time.Time
}

func NewBot(
	cfg *config.Config,
	aiClient *ai.Client,
	memStore memory.Store,
	cryptoSvc *crypto.Service,
	searchSvc *search.Service,
	tokenSvc *token.Service,
) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("failed to init telegram bot api: %w", err)
	}

	log.Printf("[Bot] Authorized on account @%s (ID: %d)", api.Self.UserName, api.Self.ID)

	return &Bot{
		api:               api,
		cfg:               cfg,
		ai:                aiClient,
		memory:            memStore,
		crypto:            cryptoSvc,
		search:            searchSvc,
		token:             tokenSvc,
		updatesChan:       make(chan tgbotapi.Update, 100),
		proactiveDisabled: make(map[int64]bool),
		lastProactiveTime: make(map[int64]time.Time),
	}, nil
}

func (b *Bot) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var update tgbotapi.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.updatesChan <- update
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) Start(ctx context.Context) error {
	var updates tgbotapi.UpdatesChannel

	if b.cfg.WebhookURL != "" {
		log.Printf("[Bot] Configuring Webhook at %s", b.cfg.WebhookURL)
		wh, err := tgbotapi.NewWebhook(b.cfg.WebhookURL)
		if err != nil {
			return fmt.Errorf("failed to create webhook config: %w", err)
		}
		if _, err := b.api.Request(wh); err != nil {
			return fmt.Errorf("failed to register webhook: %w", err)
		}
		updates = b.updatesChan
		log.Printf("[Bot] Webhook registered successfully with Telegram")
	} else {
		log.Printf("[Bot] WebhookURL not configured. Clearing any existing webhook and using Long Polling...")
		_, _ = b.api.Request(tgbotapi.DeleteWebhookConfig{})
		u := tgbotapi.NewUpdate(0)
		u.Timeout = 30
		updates = b.api.GetUpdatesChan(u)
	}

	// Start background proactive messaging engine
	go b.runProactiveEngine(ctx)

	log.Printf("[Bot] Shipp is live and listening for updates...")

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Bot] Shutting down update loop...")
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}

			// Process each message concurrently
			go b.handleMessage(ctx, update.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	senderID := msg.From.ID
	username := msg.From.UserName
	if username == "" {
		username = msg.From.FirstName
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	isOwner := b.cfg.IsOwner(msg.From.UserName) || b.cfg.IsOwner(msg.From.FirstName)
	isPrivate := msg.Chat.IsPrivate()

	// Log message in memory store
	_ = b.memory.SaveMessage(ctx, chatID, senderID, username, "user", text)

	// 1. Check for Slash Commands
	if strings.HasPrefix(text, "/") {
		b.handleCommand(ctx, msg, isOwner)
		return
	}

	// 2. Check if bot should respond in group
	shouldRespond := isPrivate || b.isAddressedToBot(msg)
	if !shouldRespond {
		return
	}

	// Clean bot handle from prompt
	cleanPrompt := b.cleanPrompt(text)

	// Direct Token CA detection (fast path when primarily a CA paste)
	rawAddr, rawChain := token.ExtractAddressAndChain(cleanPrompt)
	if rawAddr != "" && len(strings.Fields(cleanPrompt)) <= 3 {
		b.sendChatAction(chatID, tgbotapi.ChatTyping)
		res, err := b.token.AnalyzeToken(ctx, rawAddr, rawChain)
		if err == nil && res != nil {
			var replyText string
			switch res.Status {
			case token.StatusAmbiguousChain:
				replyText = token.FormatAmbiguousChains(res.Address, res.CandidateChains)
			case token.StatusNotFound:
				replyText = token.FormatNotFound(res.Address)
			case token.StatusSuccess:
				lower := strings.ToLower(cleanPrompt)
				if strings.Contains(lower, "detailed") || strings.Contains(lower, "details") || strings.Contains(lower, "full") || strings.Contains(lower, "breakdown") || strings.Contains(lower, "more") {
					replyText = token.FormatCard(res.Metrics)
				} else {
					replyText = token.FormatNatural(res.Metrics)
				}
			default:
				replyText = res.Message
			}
			if replyText != "" {
				b.sendReply(chatID, msg.MessageID, replyText)
				_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", replyText)
				return
			}
		}
	}

	// Send typing indicator
	b.sendChatAction(chatID, tgbotapi.ChatTyping)

	// 3. Process via AI & NLP Tool Engine
	b.handleNLPAndChat(ctx, msg, cleanPrompt, username, isOwner)
}

var shippWordRegex = regexp.MustCompile(`(?i)\bshipp\b`)

func (b *Bot) isAddressedToBot(msg *tgbotapi.Message) bool {
	// Reply to bot's own message
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if msg.ReplyToMessage.From.ID == b.api.Self.ID {
			return true
		}
	}

	// Bot username mentioned (e.g. @Shipp0Bot)
	if b.api != nil && b.api.Self.UserName != "" {
		botUserLower := strings.ToLower(b.api.Self.UserName)
		if strings.Contains(strings.ToLower(msg.Text), "@"+botUserLower) {
			return true
		}
	}

	// Word "shipp" anywhere in the message as a distinct word
	if shippWordRegex.MatchString(msg.Text) {
		return true
	}

	return false
}

func (b *Bot) cleanPrompt(text string) string {
	cleaned := text
	if b.api != nil && b.api.Self.UserName != "" {
		cleaned = strings.ReplaceAll(cleaned, "@"+b.api.Self.UserName, "")
		cleaned = strings.ReplaceAll(cleaned, "@"+strings.ToLower(b.api.Self.UserName), "")
	}
	cleaned = shippWordRegex.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimFunc(cleaned, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",:!?-.", r)
	})
	if cleaned == "" {
		return text
	}
	return cleaned
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message, isOwner bool) {
	parts := strings.Fields(msg.Text)
	cmd := strings.ToLower(parts[0])
	// Strip @BotUsername from command if present (e.g. /balance@Shipp0Bot)
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/start":
		b.sendReply(msg.Chat.ID, msg.MessageID, b.formatStartMessage(msg.From.UserName, isOwner))

	case "/help":
		b.sendReply(msg.Chat.ID, msg.MessageID, b.formatHelpMessage(isOwner))

	case "/wallet", "/deposit", "/address":
		b.sendReply(msg.Chat.ID, msg.MessageID, b.formatWalletAddressMessage())

	case "/balance", "/balances":
		b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
		b.sendReply(msg.Chat.ID, msg.MessageID, b.formatBalanceMessage(ctx))

	case "/send":
		b.handleSendCommand(ctx, msg, parts[1:], isOwner)

	case "/summarize", "/recap":
		b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
		b.handleSummarizeCommand(ctx, msg)

	case "/clear":
		b.handleClearCommand(ctx, msg, isOwner)

	case "/proactive":
		b.handleProactiveCommand(msg, parts[1:], isOwner)

	case "/ca", "/token":
		b.handleTokenCommand(ctx, msg, parts[1:])

	case "/search":
		query := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]))
		if query == "" {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/search <query>` (e.g. `/search president of Uruguay`)")
			return
		}
		b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
		res, err := b.search.Search(ctx, query)
		if err != nil {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Search query failed.")
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("🔍 **Search Results for '%s':**\n\n%s", query, res))

	default:
		// Unknown slash command
	}
}

func (b *Bot) handleNLPAndChat(
	ctx context.Context,
	msg *tgbotapi.Message,
	prompt string,
	username string,
	isOwner bool,
) {
	chatID := msg.Chat.ID

	// 1. Fetch recent history (15 messages)
	history, _ := b.memory.GetRecentMessages(ctx, chatID, 15)

	// 2. Fetch past summary if available
	summary, _ := b.memory.GetSummary(ctx, chatID)

	// 3. Ask AI with tool declarations
	aiResp, err := b.ai.GenerateReply(ctx, username, isOwner, history, prompt, summary)
	if err != nil {
		log.Printf("[Bot] AI generate error: %v", err)
		b.sendReply(chatID, msg.MessageID, "my brain lagged for a second, run that back?")
		return
	}

	// 4. Handle Tool Calls if any
	if len(aiResp.ToolCalls) > 0 {
		for _, tc := range aiResp.ToolCalls {
			toolResult := b.executeToolCall(ctx, chatID, tc.Function.Name, tc.Function.Arguments, username, isOwner)

			// Generate conversational response incorporating tool result
			followup, err := b.ai.GenerateToolFollowup(ctx, username, isOwner, prompt, tc.Function.Name, tc.ID, tc.Function.Arguments, toolResult)
			if err != nil || followup == "" {
				followup = toolResult
			}

			b.sendReply(chatID, msg.MessageID, followup)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", followup)
			return
		}
	}

	// 5. Normal conversational reply
	replyText := strings.TrimSpace(aiResp.Content)
	if replyText == "" {
		replyText = "yo, i'm with you."
	}

	b.sendReply(chatID, msg.MessageID, replyText)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", replyText)
}

func (b *Bot) executeToolCall(
	ctx context.Context,
	chatID int64,
	toolName string,
	arguments string,
	username string,
	isOwner bool,
) string {
	log.Printf("[Bot] Executing NLP Tool: %s (args: %s) invoked by @%s", toolName, arguments, username)

	switch toolName {
	case "get_wallet_address":
		var args struct {
			Chain string `json:"chain"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		svmAddr, evmAddr := b.crypto.GetAddresses()
		chain := strings.ToLower(strings.TrimSpace(args.Chain))
		if chain == "sol" || chain == "solana" || chain == "svm" {
			return fmt.Sprintf("Solana deposit address: %s", svmAddr)
		} else if chain == "rh" || chain == "robinhood" {
			return fmt.Sprintf("Robinhood (RH) deposit address: %s", evmAddr)
		} else if chain != "" && chain != "all" {
			return fmt.Sprintf("EVM (%s) deposit address: %s", strings.ToUpper(chain), evmAddr)
		}
		return fmt.Sprintf("Solana (SVM): %s\nEVM (Base, Robinhood, Ethereum, Arbitrum, BSC): %s", svmAddr, evmAddr)

	case "get_balances":
		var args struct {
			Chain string `json:"chain"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		chain := strings.ToLower(strings.TrimSpace(args.Chain))
		if chain == "sol" || chain == "solana" || chain == "svm" {
			bal, err := b.crypto.GetSVMBalance(ctx)
			if err != nil {
				return "Solana balance is currently unavailable."
			}
			return fmt.Sprintf("Solana balance: %s SOL", bal.Text('f', 4))
		} else if chain != "" && chain != "all" {
			bal, err := b.crypto.GetEVMBalance(ctx, chain)
			if err != nil {
				return fmt.Sprintf("%s balance is currently unavailable.", strings.ToUpper(chain))
			}
			symbol := "ETH"
			if chain == "bnb" || chain == "bsc" {
				symbol = "BNB"
			}
			displayName := strings.ToUpper(chain)
			if chain == "rh" || chain == "robinhood" {
				displayName = "Robinhood"
			}
			return fmt.Sprintf("%s balance: %s %s", displayName, bal.Text('f', 4), symbol)
		}

		balances, err := b.crypto.GetAllBalances(ctx)
		if err != nil {
			return "Failed to fetch balances."
		}
		return fmt.Sprintf("Balances: Solana: %s, Base: %s, Robinhood: %s, Arbitrum: %s, Ethereum: %s, BNB: %s",
			balances.Solana, balances.Base, balances.Robinhood, balances.Arbitrum, balances.Ethereum, balances.BNB)

	case "send_crypto":
		var args ai.SendCryptoArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "Failed to parse send arguments. Please specify chain, recipient address, and amount."
		}

		if !isOwner {
			return fmt.Sprintf("Access Denied: Only bot owners (@%s) can authorize crypto transfers.", strings.Join(b.cfg.Owners, ", @"))
		}

		return b.executeCryptoSend(ctx, args.Chain, args.Recipient, args.Amount)

	case "summarize_context":
		messages, err := b.memory.GetRecentMessages(ctx, chatID, 40)
		if err != nil || len(messages) == 0 {
			return "Not enough conversation history to summarize yet."
		}
		summary, err := b.ai.SummarizeChat(ctx, messages)
		if err != nil {
			return "Couldn't generate summary right now."
		}
		_ = b.memory.SaveSummary(ctx, chatID, summary)
		return fmt.Sprintf("📝 **Conversation Recap:**\n\n%s", summary)

	case "clear_context":
		_ = b.memory.ClearContext(ctx, chatID)
		return "🧹 Context and memory have been wiped clean for this chat. Clean slate!"

	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		q := strings.TrimSpace(args.Query)
		if q == "" {
			return "No search query provided."
		}
		res, err := b.search.Search(ctx, q)
		if err != nil {
			return fmt.Sprintf("Search error: %v", err)
		}
		return res

	case "analyze_token":
		var args struct {
			Address string `json:"address"`
			Chain   string `json:"chain"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		addr := strings.TrimSpace(args.Address)
		if addr == "" {
			return "No token address provided. Please specify a contract address."
		}
		res, err := b.token.AnalyzeToken(ctx, addr, args.Chain)
		if err != nil {
			return fmt.Sprintf("Error analyzing token: %v", err)
		}
		switch res.Status {
		case token.StatusAmbiguousChain:
			return token.FormatAmbiguousChains(res.Address, res.CandidateChains)
		case token.StatusNotFound:
			return token.FormatNotFound(res.Address)
		case token.StatusSuccess:
			return token.FormatCard(res.Metrics)
		default:
			return res.Message
		}

	default:
		return "Unknown action."
	}
}

func (b *Bot) handleSendCommand(ctx context.Context, msg *tgbotapi.Message, args []string, isOwner bool) {
	if !isOwner {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("🔒 Nice try anon! Only bot owners (@%s) can authorize crypto transfers.", strings.Join(b.cfg.Owners, ", @")))
		return
	}

	if len(args) < 3 {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/send <chain> <to_address> <amount>`\nExample: `/send base 0x123... 0.01` or `/send solana Hzxd... 0.05`")
		return
	}

	chain := args[0]
	to := args[1]
	amount, err := strconv.ParseFloat(args[2], 64)
	if err != nil || amount <= 0 {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Invalid amount. Please provide a positive numeric amount.")
		return
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	result := b.executeCryptoSend(ctx, chain, to, amount)
	b.sendReply(msg.Chat.ID, msg.MessageID, result)
}

func (b *Bot) executeCryptoSend(ctx context.Context, chain, toAddress string, amount float64) string {
	chainLower := strings.ToLower(chain)

	if chainLower == "sol" || chainLower == "solana" || chainLower == "svm" {
		txHash, explorer, err := b.crypto.SendSVM(ctx, toAddress, amount)
		if err != nil {
			return fmt.Sprintf("❌ Solana Transfer Failed: %v", err)
		}
		return fmt.Sprintf("🚀 **Solana Transfer Successful!**\n\nAmount: `%.4f SOL`\nTo: `%s`\nTx Hash: `%s`\n[View on Solscan](%s)", amount, toAddress, txHash, explorer)
	}

	// EVM transfer
	txHash, explorer, err := b.crypto.SendEVM(ctx, chainLower, toAddress, amount)
	if err != nil {
		return fmt.Sprintf("❌ EVM Transfer Failed (%s): %v", chain, err)
	}

	return fmt.Sprintf("🚀 **EVM Transfer Successful!**\n\nChain: **%s**\nAmount: `%.6f`\nTo: `%s`\nTx Hash: `%s`\n[View Explorer](%s)", strings.ToUpper(chainLower), amount, toAddress, txHash, explorer)
}

func (b *Bot) handleSummarizeCommand(ctx context.Context, msg *tgbotapi.Message) {
	messages, err := b.memory.GetRecentMessages(ctx, msg.Chat.ID, 40)
	if err != nil || len(messages) == 0 {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Not enough messages to summarize yet.")
		return
	}

	summary, err := b.ai.SummarizeChat(ctx, messages)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Error generating summary.")
		return
	}

	_ = b.memory.SaveSummary(ctx, msg.Chat.ID, summary)
	b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("📝 **Conversation Recap:**\n\n%s", summary))
}

func (b *Bot) handleClearCommand(ctx context.Context, msg *tgbotapi.Message, isOwner bool) {
	err := b.memory.ClearContext(ctx, msg.Chat.ID)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Error clearing memory context.")
		return
	}
	b.sendReply(msg.Chat.ID, msg.MessageID, "🧹 Memory cleared! I've wiped our conversation context for this chat.")
}

func (b *Bot) handleProactiveCommand(msg *tgbotapi.Message, args []string, isOwner bool) {
	if len(args) == 0 {
		status := "enabled"
		b.proactiveMu.RLock()
		if b.proactiveDisabled[msg.Chat.ID] {
			status = "disabled"
		}
		b.proactiveMu.RUnlock()
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Proactive messaging is currently **%s** for this chat.\nUse `/proactive on` or `/proactive off` to change it.", status))
		return
	}

	setting := strings.ToLower(args[0])
	b.proactiveMu.Lock()
	defer b.proactiveMu.Unlock()

	if setting == "off" || setting == "disable" {
		b.proactiveDisabled[msg.Chat.ID] = true
		b.sendReply(msg.Chat.ID, msg.MessageID, "🤐 Proactive messaging turned **OFF**. I'll only speak when spoken to.")
	} else if setting == "on" || setting == "enable" {
		b.proactiveDisabled[msg.Chat.ID] = false
		b.sendReply(msg.Chat.ID, msg.MessageID, "⚡ Proactive messaging turned **ON**. I'll occasionally chime in with thoughts and vibes.")
	}
}

func (b *Bot) handleTokenCommand(ctx context.Context, msg *tgbotapi.Message, args []string) {
	if len(args) == 0 {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: /ca <address> or /ca <chain> <address>\nExample: /ca 0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913 or /ca solana DezXAZ8...")
		return
	}

	addr, chain := token.ExtractAddressAndChain(strings.Join(args, " "))
	if addr == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Please provide a valid token address or Solana mint.")
		return
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	res, err := b.token.AnalyzeToken(ctx, addr, chain)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Error analyzing token: %v", err))
		return
	}

	var replyText string
	switch res.Status {
	case token.StatusAmbiguousChain:
		replyText = token.FormatAmbiguousChains(res.Address, res.CandidateChains)
	case token.StatusNotFound:
		replyText = token.FormatNotFound(res.Address)
	case token.StatusSuccess:
		replyText = token.FormatCard(res.Metrics)
	default:
		replyText = res.Message
	}

	b.sendReply(msg.Chat.ID, msg.MessageID, replyText)
	_ = b.memory.SaveMessage(ctx, msg.Chat.ID, b.api.Self.ID, b.api.Self.UserName, "assistant", replyText)
}

func (b *Bot) runProactiveEngine(ctx context.Context) {
	// Random check every 25 to 50 minutes
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.triggerProactiveMessage(ctx)
		}
	}
}

func (b *Bot) triggerProactiveMessage(ctx context.Context) {
	activeChats, err := b.memory.GetActiveChatIDs(ctx)
	if err != nil || len(activeChats) == 0 {
		return
	}

	// Shuffle chats to select one
	rand.Seed(time.Now().UnixNano())
	shuffled := make([]int64, len(activeChats))
	copy(shuffled, activeChats)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	now := time.Now()
	for _, chatID := range shuffled {
		b.proactiveMu.RLock()
		disabled := b.proactiveDisabled[chatID]
		lastSent := b.lastProactiveTime[chatID]
		b.proactiveMu.RUnlock()

		if disabled {
			continue
		}

		// Don't send proactive messages if one was sent within the last 2 hours to avoid spam
		if now.Sub(lastSent) < 2*time.Hour {
			continue
		}

		// Fetch recent messages
		recent, _ := b.memory.GetRecentMessages(ctx, chatID, 10)

		// Generate spontaneous comment
		proactiveText, err := b.ai.GenerateProactiveMessage(ctx, recent)
		if err != nil || proactiveText == "" {
			continue
		}

		b.sendSimpleMessage(chatID, proactiveText)
		_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", proactiveText)

		b.proactiveMu.Lock()
		b.lastProactiveTime[chatID] = now
		b.proactiveMu.Unlock()
		break // Only one proactive message per cycle
	}
}

func (b *Bot) formatStartMessage(username string, isOwner bool) string {
	roleGreeting := "Hey"
	if isOwner {
		roleGreeting = "Welcome boss"
	}
	return fmt.Sprintf(`👋 %s! I'm **Shipp**, your personal AI companion with native crypto powers.

I'm built for both group chats and private DMs. You can talk to me completely naturally or use slash commands!

⚡ **What I can do:**
• **Natural Chat & Banter:** Tag me (@%s) or reply to me. Powered by Groq fast inference.
• **Proactive Presence:** I'll chime in spontaneously to keep the chat lively.
• **Crypto Deposits:** Ask "what's your address?" or use /wallet.
• **Check Balances:** Ask "how much sol do you have?" or use /balance.
• **Send Funds (Owner Only):** Send native tokens on Solana, Base, Ethereum, Arbitrum, BNB.
• **Memory & Recaps:** Ask me to recap what we talked about or use /summarize.

Type /help to see all commands and examples!`, roleGreeting, b.api.Self.UserName)
}

func (b *Bot) formatHelpMessage(isOwner bool) string {
	ownerNote := ""
	if isOwner {
		ownerNote = "\n👑 *Owner Commands:*\n• `/send <chain> <to> <amount>` - Transfer funds (e.g. `/send base 0x123... 0.01`)"
	}

	return "🤖 *Shipp Command & NLP Reference*\n\n" +
		"💬 *Natural Language:*\n" +
		"You don't need slashes! You can say:\n" +
		"• \"Shipp, what's your sol address?\"\n" +
		"• \"Check your balances across chains\"\n" +
		"• \"Check this token CA: 0x8335...\"\n" +
		"• \"Summarize what we discussed earlier\"\n" +
		"• \"Clear memory context\"\n" +
		"• \"Send 0.01 eth to 0x... on base\" (Owner only)\n\n" +
		"⚡ *Slash Commands:*\n" +
		"• `/ca <address>` or `/token <address>` - Analyze token metrics (MCap, Vol, LP)\n" +
		"• `/wallet` or `/deposit` - View deposit addresses\n" +
		"• `/balance` - Check live balances (Solana & EVM)\n" +
		"• `/summarize` - Recap recent conversation\n" +
		"• `/clear` - Flush context memory for this chat\n" +
		"• `/proactive on|off` - Toggle spontaneous messages" + ownerNote + "\n" +
		"• `/help` - Show this message"
}

func (b *Bot) formatWalletAddressMessage() string {
	svmAddr, evmAddr := b.crypto.GetAddresses()
	return fmt.Sprintf("💳 *Shipp Deposit Addresses*\n\n"+
		"🟣 *Solana (SVM):*\n`%s`\n\n"+
		"🔵 *EVM (Base, Robinhood, Ethereum, Arbitrum, BSC):*\n`%s`\n\n"+
		"*(Tap any address above to copy it)*", svmAddr, evmAddr)
}

func (b *Bot) formatBalanceMessage(ctx context.Context) string {
	balances, err := b.crypto.GetAllBalances(ctx)
	if err != nil {
		return "⚠️ Failed to fetch wallet balances. Please try again in a moment."
	}

	return fmt.Sprintf(`💰 **Live Wallet Balances**

🟣 **Solana:** %s
🔵 **Base:** %s
🟢 **Robinhood:** %s
🔷 **Arbitrum:** %s
💠 **Ethereum:** %s
🟡 **BNB Chain:** %s`,
		balances.Solana,
		balances.Base,
		balances.Robinhood,
		balances.Arbitrum,
		balances.Ethereum,
		balances.BNB,
	)
}

func (b *Bot) sendReply(chatID int64, replyToMsgID int, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if replyToMsgID > 0 {
		msg.ReplyToMessageID = replyToMsgID
	}
	_, err := b.api.Send(msg)
	if err != nil {
		// Fallback without parse mode if markdown fails
		msg.ParseMode = ""
		_, _ = b.api.Send(msg)
	}
}

func (b *Bot) sendSimpleMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, err := b.api.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		_, _ = b.api.Send(msg)
	}
}

func (b *Bot) sendChatAction(chatID int64, action string) {
	chatAction := tgbotapi.NewChatAction(chatID, action)
	_, _ = b.api.Send(chatAction)
}
