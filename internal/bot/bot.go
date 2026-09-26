package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
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
	"shipp/internal/docparser"
	"shipp/internal/email"
	"shipp/internal/github"
	"shipp/internal/memory"
	"shipp/internal/price"
	"shipp/internal/sandbox"
	"shipp/internal/search"
	"shipp/internal/token"
	"shipp/internal/vision"
)

type Bot struct {
	api         *tgbotapi.BotAPI
	cfg         *config.Config
	ai          *ai.Client
	memory      memory.Store
	crypto      *crypto.Service
	search      *search.Service
	token       *token.Service
	price       *price.Service
	vision      *vision.Service
	github      *github.Service
	email       *email.Service
	sandbox     *sandbox.Service
	updatesChan chan tgbotapi.Update

	// DM tracking (username -> chat_id for private DMs)
	dmMu        sync.RWMutex
	userDMChats map[string]int64

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
	priceSvc *price.Service,
	visionSvc *vision.Service,
	githubSvc *github.Service,
	emailSvc *email.Service,
	sandboxSvc *sandbox.Service,
) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("failed to init telegram bot api: %w", err)
	}

	log.Printf("[Bot] Authorized on account @%s (ID: %d)", api.Self.UserName, api.Self.ID)

	b := &Bot{
		api:               api,
		cfg:               cfg,
		ai:                aiClient,
		memory:            memStore,
		crypto:            cryptoSvc,
		search:            searchSvc,
		token:             tokenSvc,
		price:             priceSvc,
		vision:            visionSvc,
		github:            githubSvc,
		email:             emailSvc,
		sandbox:           sandboxSvc,
		updatesChan:       make(chan tgbotapi.Update, 100),
		userDMChats:       make(map[string]int64),
		proactiveDisabled: make(map[int64]bool),
		lastProactiveTime: make(map[int64]time.Time),
	}

	if sandboxSvc != nil {
		sandboxSvc.SetHandlers(func(payload sandbox.CallbackPayload) {
			b.handleSandboxCompletion(payload)
		}, func(task *sandbox.Task) {
			b.handleSandboxTimeout(task)
		})
	}

	return b, nil
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

	// Record private chat ID for DMs
	if msg.From != nil && msg.From.UserName != "" && msg.Chat.IsPrivate() {
		uClean := strings.ToLower(strings.TrimPrefix(msg.From.UserName, "@"))
		b.dmMu.Lock()
		b.userDMChats[uClean] = msg.Chat.ID
		b.dmMu.Unlock()
	}

	// 0. Handle Photos
	if len(msg.Photo) > 0 {
		b.handlePhotoMessage(ctx, msg)
		return
	}

	// 0b. Handle Documents (.md, .pdf, .docx, images)
	if msg.Document != nil {
		b.handleDocumentMessage(ctx, msg)
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" && msg.Caption != "" {
		text = strings.TrimSpace(msg.Caption)
	}
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

	checkText := msg.Text
	if checkText == "" && msg.Caption != "" {
		checkText = msg.Caption
	}

	// Bot username mentioned (e.g. @Shipp0Bot)
	if b.api != nil && b.api.Self.UserName != "" {
		botUserLower := strings.ToLower(b.api.Self.UserName)
		if strings.Contains(strings.ToLower(checkText), "@"+botUserLower) {
			return true
		}
	}

	// Word "shipp" anywhere in the message as a distinct word
	if shippWordRegex.MatchString(checkText) {
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

	case "/summarize", "/recap", "/compact":
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

	case "/email":
		b.handleEmailCommand(ctx, msg, parts[1:], isOwner)

	case "/tokens", "/usage":
		if !isOwner {
			b.sendReply(msg.Chat.ID, msg.MessageID, "only owners can view token consumption metrics.")
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, b.ai.GetTokenReport())

	case "/bash", "/sandbox", "/exec":
		if !isOwner {
			b.sendReply(msg.Chat.ID, msg.MessageID, "only owners can execute sandbox commands.")
			return
		}
		cmdToRun := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]))
		if cmdToRun == "" {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/bash <command>` (e.g. `/bash go test ./...`)")
			return
		}
		if b.sandbox == nil {
			b.sendReply(msg.Chat.ID, msg.MessageID, "sandbox service not initialized (GITHUB_PAT missing).")
			return
		}
		taskID, err := b.sandbox.Dispatch(ctx, msg.Chat.ID, cmdToRun, "")
		if err != nil {
			log.Printf("[Bot] Sandbox dispatch error: %v", err)
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to launch sandbox runner: %v", err))
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("spinning up ephemeral runner to run `%s` (task %s). i'll alert you when it finishes.", cmdToRun, taskID))

	case "/dm":
		if !isOwner {
			b.sendReply(msg.Chat.ID, msg.MessageID, "only owners can authorize sending DMs.")
			return
		}
		if len(parts) < 3 {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/dm @username <message>`")
			return
		}
		targetUser := strings.ToLower(strings.TrimPrefix(parts[1], "@"))
		textToSend := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]+" "+parts[1]))
		b.dmMu.RLock()
		dmChatID, exists := b.userDMChats[targetUser]
		b.dmMu.RUnlock()

		if !exists {
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("cant dm @%s directly yet because telegram restricts bots from cold-dm'ing users until they message the bot first. tell @%s to open a chat with @%s and send /start.", targetUser, targetUser, b.api.Self.UserName))
			return
		}

		dmMsg := tgbotapi.NewMessage(dmChatID, fmt.Sprintf("Message from @%s via Shipp:\n\n%s", msg.From.UserName, textToSend))
		if _, err := b.api.Send(dmMsg); err != nil {
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to send dm to @%s: %v", targetUser, err))
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("sent direct message to @%s.", targetUser))

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

	// 1. Fetch recent history (8 messages to stay well within Groq rate limits)
	history, _ := b.memory.GetRecentMessages(ctx, chatID, 8)

	// 2. Fetch past summary and learned user profile
	summary, _ := b.memory.GetSummary(ctx, chatID)
	profile, _ := b.memory.GetUserProfile(ctx, chatID)

	lowerPrompt := strings.ToLower(prompt)

	// 2a. Pre-AI balance dispatch: skip LLM to prevent hallucinating made-up numbers
	if ok, targetChain := isBalanceIntent(prompt); ok {
		b.sendChatAction(chatID, tgbotapi.ChatTyping)
		toolResult := b.executeToolCall(ctx, chatID, "get_balances", fmt.Sprintf(`{"chain":"%s"}`, targetChain), username, isOwner)
		b.sendReply(chatID, msg.MessageID, toolResult)
		_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
		go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		return
	}

	// 2b. Pre-AI dispatch: if the prompt has a GitHub URL + edit verb, skip the LLM and
	//     call github_edit_file directly to avoid clarification loops.
	if isOwner {
		if b.trySmartDispatch(ctx, msg, prompt, lowerPrompt, username, isOwner, history) {
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
			return
		}
	}

	// 3. Ask AI with tool declarations
	aiResp, err := b.ai.GenerateReply(ctx, username, isOwner, history, prompt, summary, profile)
	if err != nil {
		log.Printf("[Bot] AI generate error: %v", err)
		// Even on rate-limit / error, still try interceptors so merge/edit commands aren't lost.
		if isOwner && b.tryInterceptAction(ctx, msg, prompt, lowerPrompt, username, isOwner, history, "") {
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
			return
		}
		b.sendReply(chatID, msg.MessageID, b.getRandomChatFallback())
		return
	}

	// 4. Handle Tool Calls if any
	if len(aiResp.ToolCalls) > 0 {
		for _, tc := range aiResp.ToolCalls {
			// For long-running tools (github edit, sandbox), ack immediately and run async.
			if tc.Function.Name == "github_edit_file" || tc.Function.Name == "run_sandbox_task" {
				b.sendReply(chatID, msg.MessageID, b.getRandomWorkingAck())
				tcCopy := tc
				go func() {
					toolResult := b.executeToolCall(ctx, chatID, tcCopy.Function.Name, tcCopy.Function.Arguments, username, isOwner)
					followup, err := b.ai.GenerateToolFollowup(ctx, username, isOwner, prompt, tcCopy.Function.Name, tcCopy.ID, tcCopy.Function.Arguments, toolResult, profile)
					if err != nil || followup == "" {
						followup = toolResult
					}
					b.sendSimpleMessage(chatID, followup)
					_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", followup)
				}()
				go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
				return
			}

			toolResult := b.executeToolCall(ctx, chatID, tc.Function.Name, tc.Function.Arguments, username, isOwner)

			// Generate conversational response incorporating tool result
			followup, err := b.ai.GenerateToolFollowup(ctx, username, isOwner, prompt, tc.Function.Name, tc.ID, tc.Function.Arguments, toolResult, profile)
			if err != nil || followup == "" {
				followup = toolResult
			}

			b.sendReply(chatID, msg.MessageID, followup)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", followup)
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
			return
		}
	}

	// 5. Intercept hallucinated git actions or explicit push/merge commands that bypassed tool calls
	replyText := strings.TrimSpace(aiResp.Content)
	if isOwner && b.tryInterceptAction(ctx, msg, prompt, lowerPrompt, username, isOwner, history, replyText) {
		go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		return
	}

	if replyText == "" {
		replyText = b.getRandomEmptyAck()
	}

	b.sendReply(chatID, msg.MessageID, replyText)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", replyText)
	go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
}

// trySmartDispatch fires github_edit_file directly when the prompt has a GitHub URL + an edit verb,
// bypassing the LLM to prevent "I need the repo name" clarification loops.
func (b *Bot) trySmartDispatch(
	ctx context.Context,
	msg *tgbotapi.Message,
	prompt, lowerPrompt, username string,
	isOwner bool,
	history []memory.Message,
) bool {
	editVerbs := []string{
		"update", "edit", "change", "rephrase", "rewrite", "modify",
		"fix the description", "update the description", "update the readme",
	}
	hasEditVerb := false
	for _, v := range editVerbs {
		if strings.Contains(lowerPrompt, v) {
			hasEditVerb = true
			break
		}
	}
	if !hasEditVerb {
		return false
	}

	// Only fire when the GitHub URL is in THIS prompt (not just history), so we're confident
	repo := extractRepoFromHistory(nil, prompt)
	if repo == "" {
		return false
	}

	log.Printf("[Bot] SmartDispatch: edit+URL detected in prompt, executing github_edit_file on %s", repo)
	pushToMain := strings.Contains(lowerPrompt, "push to main") ||
		strings.Contains(lowerPrompt, "straight to main") ||
		strings.Contains(lowerPrompt, "push straight")
	argsJSON, _ := json.Marshal(map[string]interface{}{
		"repo":         repo,
		"path":         "README.md",
		"instruction":  prompt,
		"push_to_main": pushToMain,
	})

	// Ack immediately, then do the work in background and follow up when done.
	b.sendReply(msg.Chat.ID, msg.MessageID, b.getRandomWorkingAck())
	go func() {
		toolResult := b.executeToolCall(ctx, msg.Chat.ID, "github_edit_file", string(argsJSON), username, isOwner)
		b.sendSimpleMessage(msg.Chat.ID, toolResult)
		_ = b.memory.SaveMessage(ctx, msg.Chat.ID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
	}()
	return true
}

// tryInterceptAction catches hallucinated push/merge text claims or explicit commands that the LLM
// responded to without calling the actual tool. Also runs on AI error so rate limits don't lose commands.
func (b *Bot) tryInterceptAction(
	ctx context.Context,
	msg *tgbotapi.Message,
	prompt, lowerPrompt, username string,
	isOwner bool,
	history []memory.Message,
	replyText string,
) bool {
	chatID := msg.Chat.ID
	lowerReply := strings.ToLower(replyText)

	// Push / edit interception
	isPushOrEditRequest := strings.Contains(lowerPrompt, "push to main") ||
		strings.Contains(lowerPrompt, "push straight") ||
		strings.Contains(lowerPrompt, "rephrase it and push") ||
		strings.Contains(lowerPrompt, "commit to main")
	isClaimingPushed := strings.Contains(lowerReply, "pushed straight to main") ||
		strings.Contains(lowerReply, "pushed to main") ||
		strings.Contains(lowerReply, "pushed directly") ||
		(strings.Contains(lowerReply, "opened pr") && !strings.Contains(lowerReply, "want me to open a pr"))

	if isPushOrEditRequest || isClaimingPushed {
		recoveredRepo := extractRepoFromHistory(history, prompt)
		if recoveredRepo != "" {
			log.Printf("[Bot] Intercepted push without tool execution. Executing github_edit_file on %s", recoveredRepo)
			pushToMain := strings.Contains(lowerPrompt, "main") || strings.Contains(lowerReply, "main")
			instruction := prompt
			for i := len(history) - 1; i >= 0; i-- {
				if history[i].Role == "user" && !strings.Contains(strings.ToLower(history[i].Content), "push to main") {
					instruction = history[i].Content + " - " + prompt
					break
				}
			}
			argsJSON, _ := json.Marshal(map[string]interface{}{
				"repo":         recoveredRepo,
				"path":         "README.md",
				"instruction":  instruction,
				"push_to_main": pushToMain,
			})
			// Ack first, then work in background.
			b.sendReply(chatID, msg.MessageID, b.getRandomWorkingAck())
			go func() {
				toolResult := b.executeToolCall(ctx, chatID, "github_edit_file", string(argsJSON), username, isOwner)
				b.sendSimpleMessage(chatID, toolResult)
				_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
			}()
			return true
		}
	}


	// Merge interception — catches "merge", "merge it", "merge pr", "merge the pr", etc.
	isMergeRequest := strings.Contains(lowerPrompt, "merge")
	isClaimingMerged := strings.Contains(lowerReply, "merged pr") || strings.Contains(lowerReply, "merged pull request")
	if isMergeRequest || isClaimingMerged {
		recoveredRepo := extractRepoFromHistory(history, prompt)
		if recoveredRepo != "" {
			prNum := extractPRNumber(history, prompt)
			if prNum > 0 {
				log.Printf("[Bot] Intercepted merge without tool execution. Executing github_merge_pr on %s #%d", recoveredRepo, prNum)
				argsJSON, _ := json.Marshal(map[string]interface{}{
					"repo":      recoveredRepo,
					"pr_number": prNum,
				})
				toolResult := b.executeToolCall(ctx, chatID, "github_merge_pr", string(argsJSON), username, isOwner)
				b.sendReply(chatID, msg.MessageID, toolResult)
				_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
				return true
			}
		}
	}

	return false
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
		prices := b.price.GetPrices(ctx)

		if chain == "sol" || chain == "solana" || chain == "svm" {
			bal, err := b.crypto.GetSVMBalance(ctx)
			if err != nil {
				return "Solana balance is currently unavailable."
			}
			fVal, _ := bal.Float64()
			usd := price.ConvertToUSD(fVal, "SOL", prices)
			return fmt.Sprintf("Solana balance: %s SOL (~%s USD)", crypto.FormatTokenAmount(bal), price.FormatUSD(usd))
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
			fVal, _ := bal.Float64()
			usd := price.ConvertToUSD(fVal, symbol, prices)
			return fmt.Sprintf("%s balance: %s %s (~%s USD)", displayName, crypto.FormatTokenAmount(bal), symbol, price.FormatUSD(usd))
		}

		balances, err := b.crypto.GetAllBalances(ctx)
		if err != nil {
			return "Failed to fetch balances."
		}
		solUSD := price.ConvertToUSD(balances.SolanaVal, "SOL", prices)
		baseUSD := price.ConvertToUSD(balances.BaseVal, "ETH", prices)
		rhUSD := price.ConvertToUSD(balances.RhVal, "ETH", prices)
		arbUSD := price.ConvertToUSD(balances.ArbVal, "ETH", prices)
		ethUSD := price.ConvertToUSD(balances.EthVal, "ETH", prices)
		bnbUSD := price.ConvertToUSD(balances.BnbVal, "BNB", prices)
		totalUSD := solUSD + baseUSD + rhUSD + arbUSD + ethUSD + bnbUSD

		return fmt.Sprintf("Balances: Solana: %s (%s), Base: %s (%s), Robinhood: %s (%s), Arbitrum: %s (%s), Ethereum: %s (%s), BNB: %s (%s). Total Portfolio Net Worth: %s USD",
			balances.Solana, price.FormatUSD(solUSD),
			balances.Base, price.FormatUSD(baseUSD),
			balances.Robinhood, price.FormatUSD(rhUSD),
			balances.Arbitrum, price.FormatUSD(arbUSD),
			balances.Ethereum, price.FormatUSD(ethUSD),
			balances.BNB, price.FormatUSD(bnbUSD),
			price.FormatUSD(totalUSD))

	case "convert_crypto":
		var args struct {
			Amount float64 `json:"amount"`
			From   string  `json:"from"`
			To     string  `json:"to"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.From == "" {
			args.From = "ETH"
		}
		if args.To == "" {
			args.To = "USD"
		}
		if args.Amount <= 0 {
			return "Please specify an amount greater than 0 to convert."
		}

		prices := b.price.GetPrices(ctx)
		result, rate, err := price.Convert(args.Amount, args.From, args.To, prices)
		if err != nil {
			return fmt.Sprintf("Couldn't convert %g %s to %s: %v", args.Amount, strings.ToUpper(args.From), strings.ToUpper(args.To), err)
		}

		if strings.ToUpper(args.To) == "USD" {
			return fmt.Sprintf("%g %s is worth %s USD (rate: %s/%s).",
				args.Amount, strings.ToUpper(args.From), price.FormatUSD(result), price.FormatUSD(rate), strings.ToUpper(args.From))
		}
		return fmt.Sprintf("%g %s is approximately %s %s.",
			args.Amount, strings.ToUpper(args.From), price.FormatCrypto(result, args.To), strings.ToUpper(args.To))

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
		return fmt.Sprintf("Conversation recap:\n\n%s", summary)

	case "clear_context":
		_ = b.memory.ClearContext(ctx, chatID)
		return "Context and memory have been cleared for this chat."

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

	case "github_inspect_project", "github_view_repo":
		var args struct {
			Repo      string `json:"repo"`
			View      string `json:"view"`
			Path      string `json:"path"`
			Branch    string `json:"branch"`
			CustomPAT string `json:"custom_pat"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Repo == "" {
			return "No repository specified. Please specify a repo like 'davidnzube101/shipp'."
		}
		owner, repoName, err := b.github.ParseRepoSlug(args.Repo)
		if err != nil {
			return fmt.Sprintf("Invalid repo format: %v", err)
		}

		view := strings.ToLower(strings.TrimSpace(args.View))
		switch view {
		case "actions", "workflow", "runs", "ci":
			runs, err := b.github.GetWorkflowRuns(ctx, owner, repoName, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't fetch workflow runs for '%s/%s': %v", owner, repoName, err)
			}
			return github.FormatWorkflowRuns(owner, repoName, runs)

		case "releases", "release", "tags":
			releases, err := b.github.GetReleases(ctx, owner, repoName, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't fetch releases for '%s/%s': %v", owner, repoName, err)
			}
			return github.FormatReleases(owner, repoName, releases)

		case "commits", "history":
			commits, err := b.github.GetCommits(ctx, owner, repoName, args.Branch, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't fetch commits for '%s/%s': %v", owner, repoName, err)
			}
			return github.FormatCommits(owner, repoName, commits)

		case "issues", "issue":
			issues, err := b.github.GetIssues(ctx, owner, repoName, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't fetch issues for '%s/%s': %v", owner, repoName, err)
			}
			return github.FormatIssues(owner, repoName, issues)

		case "overview", "stats", "summary", "repo":
			overview, err := b.github.GetRepoOverview(ctx, owner, repoName, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't fetch repo overview for '%s/%s': %v", owner, repoName, err)
			}
			return github.FormatRepoOverview(overview)

		default:
			// View a specific file if view == "file" or if path is explicitly set
			if view == "file" || (args.Path != "" && view != "files" && view != "dir") {
				targetPath := args.Path
				if targetPath == "" {
					targetPath = "README.md"
				}
				_, content, err := b.github.GetFileWithToken(ctx, owner, repoName, targetPath, args.Branch, args.CustomPAT)
				if err != nil {
					// If file not found, list directory to help user
					files, listErr := b.github.ListDirectoryWithToken(ctx, owner, repoName, filepath.Dir(targetPath), args.Branch, args.CustomPAT)
					if listErr == nil && len(files) > 0 {
						return fmt.Sprintf("File '%s' was not found in '%s/%s'. Files in that folder: %s", targetPath, owner, repoName, strings.Join(files, ", "))
					}
					return fmt.Sprintf("Couldn't read '%s' from '%s/%s': %v", targetPath, owner, repoName, err)
				}

				preview := content
				if len(preview) > 1500 {
					preview = preview[:1500] + "\n... (truncated preview)"
				}
				return fmt.Sprintf("%s/%s (%s):\n\n```\n%s\n```", owner, repoName, targetPath, preview)
			}

			// List files in repo root or directory
			files, err := b.github.ListDirectoryWithToken(ctx, owner, repoName, args.Path, args.Branch, args.CustomPAT)
			if err != nil {
				return fmt.Sprintf("Couldn't inspect repo '%s/%s': %v", owner, repoName, err)
			}
			dirName := args.Path
			if dirName == "" {
				dirName = "root"
			}
			return fmt.Sprintf("Files in %s/%s (%s): %s", owner, repoName, dirName, strings.Join(files, ", "))
		}

	case "github_edit_file":
		if !isOwner {
			return fmt.Sprintf("Access Denied: Only bot owners (@%s) can authorize code edits and PRs.", strings.Join(b.cfg.Owners, ", @"))
		}

		var args struct {
			Repo        string `json:"repo"`
			Path        string `json:"path"`
			Instruction string `json:"instruction"`
			PushToMain  bool   `json:"push_to_main"`
			CustomPAT   string `json:"custom_pat"`
			GitName     string `json:"git_name"`
			GitEmail    string `json:"git_email"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Repo == "" {
			recent, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
			args.Repo = extractRepoFromHistory(recent, "")
		}
		if args.Repo == "" || args.Instruction == "" {
			return "Please specify both the repository and what changes you want me to make."
		}

		owner, repoName, err := b.github.ParseRepoSlug(args.Repo)
		if err != nil {
			return fmt.Sprintf("Invalid repo format: %v", err)
		}

		filePath := args.Path
		if filePath == "" {
			filePath = "README.md"
		}

		// 1. Resolve default branch
		defaultBranch, err := b.github.GetDefaultBranchWithToken(ctx, owner, repoName, args.CustomPAT)
		if err != nil {
			return fmt.Sprintf("Couldn't access repo '%s/%s': %v", owner, repoName, err)
		}

		// 2. Fetch existing file
		fc, currentContent, err := b.github.GetFileWithToken(ctx, owner, repoName, filePath, defaultBranch, args.CustomPAT)
		if err != nil {
			// Check if file doesn't exist, list available files
			files, listErr := b.github.ListDirectoryWithToken(ctx, owner, repoName, "", defaultBranch, args.CustomPAT)
			if listErr == nil && len(files) > 0 {
				return fmt.Sprintf("File '%s' was not found in '%s/%s'. Found these files in repo root: %s. Would you like me to create '%s' from scratch or edit another file?", filePath, owner, repoName, strings.Join(files, ", "), filePath)
			}
			return fmt.Sprintf("Couldn't find '%s' in '%s/%s': %v", filePath, owner, repoName, err)
		}

		// 3. AI Refactor
		refactored, err := b.ai.RefactorFileContent(ctx, filePath, currentContent, args.Instruction)
		if err != nil {
			return fmt.Sprintf("Failed to generate code changes: %v", err)
		}

		// 4. Check if target section was missing
		if strings.HasPrefix(refactored, "[TARGET_NOT_FOUND:") {
			detail := strings.TrimPrefix(refactored, "[TARGET_NOT_FOUND:")
			detail = strings.TrimPrefix(detail, " ")
			return fmt.Sprintf("I checked '%s' on %s/%s, but: %s\n\nWhere would you like me to apply the change?", filePath, owner, repoName, detail)
		}

		// 5. Commit & Push (Direct to main or Open PR)
		commitOpts := github.CommitOptions{
			Message:     fmt.Sprintf("Update %s via Shipp", filePath),
			Content:     refactored,
			FileSHA:     fc.SHA,
			CustomPAT:   args.CustomPAT,
			AuthorName:  args.GitName,
			AuthorEmail: args.GitEmail,
		}

		if args.PushToMain {
			commitOpts.Branch = defaultBranch
			commitURL, err := b.github.CommitFileWithOptions(ctx, owner, repoName, filePath, commitOpts)
			if err != nil {
				return fmt.Sprintf("Commit to %s failed: %v", defaultBranch, err)
			}
			return fmt.Sprintf("Pushed directly to %s on %s/%s: %s", defaultBranch, owner, repoName, commitURL)
		}

		// Safe PR-first default
		branchName := fmt.Sprintf("shipp/update-%s-%d", strings.ToLower(filepath.Base(filePath)), time.Now().Unix())
		if err := b.github.CreateBranchWithToken(ctx, owner, repoName, branchName, defaultBranch, args.CustomPAT); err != nil {
			return fmt.Sprintf("Failed to create branch '%s': %v", branchName, err)
		}

		commitOpts.Branch = branchName
		_, err = b.github.CommitFileWithOptions(ctx, owner, repoName, filePath, commitOpts)
		if err != nil {
			return fmt.Sprintf("Failed to commit to branch '%s': %v", branchName, err)
		}

		prURL, prNum, err := b.github.CreatePullRequestWithToken(
			ctx,
			owner,
			repoName,
			fmt.Sprintf("Shipp: Update %s", filePath),
			fmt.Sprintf("Automated update requested by @%s:\n\n> %s", username, args.Instruction),
			branchName,
			defaultBranch,
			args.CustomPAT,
		)
		if err != nil {
			return fmt.Sprintf("Committed to branch '%s', but failed to open PR: %v", branchName, err)
		}

		return fmt.Sprintf("Opened PR #%d on %s/%s: %s\nSay 'merge it' whenever you're ready.", prNum, owner, repoName, prURL)

	case "github_merge_pr":
		if !isOwner {
			return fmt.Sprintf("Access Denied: Only bot owners (@%s) can authorize PR merges.", strings.Join(b.cfg.Owners, ", @"))
		}

		var args struct {
			Repo      string `json:"repo"`
			PRNumber  int    `json:"pr_number"`
			CustomPAT string `json:"custom_pat"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Repo == "" {
			recent, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
			args.Repo = extractRepoFromHistory(recent, "")
		}
		if args.PRNumber <= 0 {
			recent, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
			args.PRNumber = extractPRNumber(recent, "")
		}
		if args.Repo == "" || args.PRNumber <= 0 {
			return "Please specify the repository and PR number (e.g. 'merge PR #4 on davidnzube101/shipp')."
		}

		owner, repoName, err := b.github.ParseRepoSlug(args.Repo)
		if err != nil {
			return fmt.Sprintf("Invalid repo format: %v", err)
		}

		_, err = b.github.MergePullRequestWithToken(ctx, owner, repoName, args.PRNumber, args.CustomPAT)
		if err != nil {
			return fmt.Sprintf("Merge failed: %v", err)
		}
		return fmt.Sprintf("Merged PR #%d on %s/%s.", args.PRNumber, owner, repoName)

	case "send_email":
		if !isOwner {
			return fmt.Sprintf("Nice try anon! Only bot owners (@%s) can authorize dispatching emails from Shipp.", strings.Join(b.cfg.Owners, ", @"))
		}

		var args struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.To == "" {
			return "Please specify a recipient email address (e.g. 'alice@example.com')."
		}
		if args.Body == "" {
			return "Please specify the email body content."
		}

		res, err := b.email.Send(ctx, args.To, args.Subject, args.Body)
		if err != nil {
			return fmt.Sprintf("Failed to send email to %s: %v", args.To, err)
		}
		return email.FormatEmailSent(res)

	case "run_sandbox_task":
		if !isOwner {
			return fmt.Sprintf("Access Denied: Only bot owners (@%s) can authorize running sandbox tasks.", strings.Join(b.cfg.Owners, ", @"))
		}
		if b.sandbox == nil {
			return "Sandbox runner is not configured (GITHUB_PAT missing)."
		}
		var args struct {
			Command string `json:"command"`
			Repo    string `json:"repo"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Command == "" {
			return "No command provided for sandbox runner."
		}
		taskID, err := b.sandbox.Dispatch(ctx, chatID, args.Command, args.Repo)
		if err != nil {
			return fmt.Sprintf("Failed to launch sandbox runner: %v", err)
		}
		return fmt.Sprintf("Ephemeral runner spawned for `%s` (task %s). Executing in background on GitHub Actions runner; will notify here when finished.", args.Command, taskID)

	case "send_dm":
		if !isOwner {
			return fmt.Sprintf("Access Denied: Only bot owners (@%s) can authorize sending direct messages.", strings.Join(b.cfg.Owners, ", @"))
		}
		var args struct {
			Recipient string `json:"recipient"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		targetUser := strings.ToLower(strings.TrimPrefix(args.Recipient, "@"))
		if targetUser == "" || args.Message == "" {
			return "Missing recipient username or message content."
		}

		b.dmMu.RLock()
		dmChatID, exists := b.userDMChats[targetUser]
		b.dmMu.RUnlock()

		if !exists {
			return fmt.Sprintf("Cannot DM @%s directly yet. Telegram prohibits bots from sending unprompted DMs until the user initiates a conversation by sending /start to @%s.", targetUser, b.api.Self.UserName)
		}

		dmMsg := tgbotapi.NewMessage(dmChatID, fmt.Sprintf("Message from @%s via Shipp:\n\n%s", username, args.Message))
		if _, err := b.api.Send(dmMsg); err != nil {
			return fmt.Sprintf("Failed to send DM to @%s: %v", targetUser, err)
		}
		return fmt.Sprintf("Successfully sent direct message to @%s.", targetUser)

	default:
		return "Unknown action."
	}
}

func (b *Bot) handleSendCommand(ctx context.Context, msg *tgbotapi.Message, args []string, isOwner bool) {
	if !isOwner {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Nice try anon! Only bot owners (@%s) can authorize crypto transfers.", strings.Join(b.cfg.Owners, ", @")))
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
			return fmt.Sprintf("Solana transfer failed: %v", err)
		}
		_ = txHash
		return fmt.Sprintf("Sent %.4f SOL to %s: %s", amount, toAddress, explorer)
	}

	// EVM transfer
	txHash, explorer, err := b.crypto.SendEVM(ctx, chainLower, toAddress, amount)
	if err != nil {
		return fmt.Sprintf("%s transfer failed: %v", chain, err)
	}
	_ = txHash

	return fmt.Sprintf("Sent %.6f %s to %s: %s", amount, strings.ToUpper(chainLower), toAddress, explorer)
}

func (b *Bot) handleEmailCommand(ctx context.Context, msg *tgbotapi.Message, args []string, isOwner bool) {
	if !isOwner {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Nice try anon! Only bot owners (@%s) can authorize dispatching emails from Shipp.", strings.Join(b.cfg.Owners, ", @")))
		return
	}

	rawText := strings.TrimSpace(strings.TrimPrefix(msg.Text, strings.Fields(msg.Text)[0]))
	if rawText == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/email <to> <subject> | <body>`\nExample: `/email dev@example.com Update on Release | Hey, we just shipped v0.3 to production!`")
		return
	}

	to := ""
	subject := ""
	body := ""

	// Check if there is a "|" delimiter for body
	if strings.Contains(rawText, "|") {
		parts := strings.SplitN(rawText, "|", 2)
		headerParts := strings.Fields(strings.TrimSpace(parts[0]))
		if len(headerParts) > 0 {
			to = headerParts[0]
			if len(headerParts) > 1 {
				subject = strings.Join(headerParts[1:], " ")
			}
		}
		body = strings.TrimSpace(parts[1])
	} else {
		fields := strings.Fields(rawText)
		to = fields[0]
		if len(fields) > 1 {
			body = strings.Join(fields[1:], " ")
		}
	}

	if to == "" || body == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/email <to> <subject> | <body>`\nExample: `/email dev@example.com Update on Release | Hey, we just shipped v0.3 to production!`")
		return
	}

	if subject == "" {
		subject = "Message from Shipp"
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	res, err := b.email.Send(ctx, to, subject, body)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Failed to dispatch email: %v", err))
		return
	}

	b.sendReply(msg.Chat.ID, msg.MessageID, email.FormatEmailSent(res))
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
	b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Conversation recap:\n\n%s", summary))
}

func (b *Bot) handleClearCommand(ctx context.Context, msg *tgbotapi.Message, isOwner bool) {
	err := b.memory.ClearContext(ctx, msg.Chat.ID)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Error clearing memory context.")
		return
	}
	b.sendReply(msg.Chat.ID, msg.MessageID, "Memory cleared. I've wiped our conversation context for this chat.")
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
		b.sendReply(msg.Chat.ID, msg.MessageID, "Proactive messaging turned off. I'll only speak when spoken to.")
	} else if setting == "on" || setting == "enable" {
		b.proactiveDisabled[msg.Chat.ID] = false
		b.sendReply(msg.Chat.ID, msg.MessageID, "Proactive messaging turned on. I'll occasionally chime in.")
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
		roleGreeting = "Welcome"
	}
	return fmt.Sprintf(`%s! I'm Shipp, your personal AI companion with native crypto powers.

I'm built for both group chats and private DMs. You can talk to me completely naturally or use slash commands!

What I can do:
• Natural Chat & Banter: Tag me (@%s) or reply to me. Powered by Groq fast inference.
• Proactive Presence: I'll chime in spontaneously to keep the chat lively.
• Crypto Deposits: Ask "what's your address?" or use /wallet.
• Check Balances: Ask "how much sol do you have?" or use /balance.
• Send Funds (Owner Only): Send native tokens on Solana, Base, Ethereum, Arbitrum, BNB.
• Memory & Recaps: Ask me to recap what we talked about or use /summarize.

Type /help to see all commands and examples!`, roleGreeting, b.api.Self.UserName)
}

func (b *Bot) formatHelpMessage(isOwner bool) string {
	ownerNote := ""
	if isOwner {
		ownerNote = "\nOwner Commands:\n• `/tokens` or `/usage` - View daily & total token consumption\n• `/bash <command>` - Ephemeral runner execution (e.g. `/bash go test ./...`)\n• `/dm @username <msg>` - Send direct message to user\n• `/send <chain> <to> <amount>` - Transfer funds (e.g. `/send base 0x123... 0.01`)\n• `/email <to> <subject> | <body>` - Dispatch email (e.g. `/email dev@example.com Hi | Hello!`)"
	}

	return "Shipp Command & NLP Reference\n\n" +
		"Natural Language:\n" +
		"You don't need slashes! You can say:\n" +
		"• \"Shipp, what's your sol address?\"\n" +
		"• \"Check your balances across chains\"\n" +
		"• \"Check this token CA: 0x8335...\"\n" +
		"• \"Summarize what we discussed earlier\"\n" +
		"• \"Clear memory context\"\n" +
		"• \"Send 0.01 eth to 0x... on base\" (Owner only)\n" +
		"• \"Email dev@example.com about the release update\" (Owner only)\n\n" +
		"Slash Commands:\n" +
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
	return fmt.Sprintf("Shipp Deposit Addresses\n\n"+
		"Solana (SVM):\n`%s`\n\n"+
		"EVM (Base, Robinhood, Ethereum, Arbitrum, BSC):\n`%s`\n\n"+
		"*(Tap any address above to copy it)*", svmAddr, evmAddr)
}

func (b *Bot) formatBalanceMessage(ctx context.Context) string {
	balances, err := b.crypto.GetAllBalances(ctx)
	if err != nil {
		return "Failed to fetch wallet balances. Please try again in a moment."
	}

	prices := b.price.GetPrices(ctx)
	solUSD := price.ConvertToUSD(balances.SolanaVal, "SOL", prices)
	baseUSD := price.ConvertToUSD(balances.BaseVal, "ETH", prices)
	rhUSD := price.ConvertToUSD(balances.RhVal, "ETH", prices)
	arbUSD := price.ConvertToUSD(balances.ArbVal, "ETH", prices)
	ethUSD := price.ConvertToUSD(balances.EthVal, "ETH", prices)
	bnbUSD := price.ConvertToUSD(balances.BnbVal, "BNB", prices)
	totalUSD := solUSD + baseUSD + rhUSD + arbUSD + ethUSD + bnbUSD

	return fmt.Sprintf(`Wallet Balances:

Solana: %s (%s)
Base: %s (%s)
Robinhood: %s (%s)
Arbitrum: %s (%s)
Ethereum: %s (%s)
BNB Chain: %s (%s)

Total Net Worth: ~%s USD`,
		balances.Solana, price.FormatUSD(solUSD),
		balances.Base, price.FormatUSD(baseUSD),
		balances.Robinhood, price.FormatUSD(rhUSD),
		balances.Arbitrum, price.FormatUSD(arbUSD),
		balances.Ethereum, price.FormatUSD(ethUSD),
		balances.BNB, price.FormatUSD(bnbUSD),
		price.FormatUSD(totalUSD),
	)
}

func (b *Bot) handlePhotoMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	isPrivate := msg.Chat.IsPrivate()
	shouldRespond := isPrivate || b.isAddressedToBot(msg) || msg.Caption != ""
	if !shouldRespond {
		return
	}

	photos := msg.Photo
	if len(photos) == 0 {
		return
	}

	// Pick highest resolution photo
	bestPhoto := photos[len(photos)-1]

	fileURL, err := b.api.GetFileDirectURL(bestPhoto.FileID)
	if err != nil {
		log.Printf("[Bot] Failed to get photo URL: %v", err)
		b.sendReply(chatID, msg.MessageID, "failed to download that image from Telegram.")
		return
	}

	resp, err := http.Get(fileURL)
	if err != nil {
		log.Printf("[Bot] Failed to download photo: %v", err)
		b.sendReply(chatID, msg.MessageID, "failed to retrieve image bytes.")
		return
	}
	defer resp.Body.Close()

	imgBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		b.sendReply(chatID, msg.MessageID, "error reading image content.")
		return
	}

	senderID := msg.From.ID
	username := msg.From.UserName
	if username == "" {
		username = msg.From.FirstName
	}
	isOwner := b.cfg.IsOwner(msg.From.UserName) || b.cfg.IsOwner(msg.From.FirstName)
	cleanCaption := b.cleanPrompt(msg.Caption)

	b.processImage(ctx, chatID, msg.MessageID, senderID, username, isOwner, imgBytes, "image/jpeg", cleanCaption)
}

func (b *Bot) processImage(
	ctx context.Context,
	chatID int64,
	replyToMsgID int,
	senderID int64,
	username string,
	isOwner bool,
	imgBytes []byte,
	mimeType string,
	cleanCaption string,
) {
	b.sendChatAction(chatID, tgbotapi.ChatTyping)

	// 1. Perception Layer (Gemini Flash = "The Eyes"): Objective visual fact extraction
	perception, err := b.vision.PerceiveImage(ctx, imgBytes, mimeType, cleanCaption)
	if err != nil || strings.TrimSpace(perception) == "" {
		log.Printf("[Bot] Vision perception error: %v", err)
		b.sendReply(chatID, replyToMsgID, "I took a look, but couldn't clearly make out what's in that picture.")
		return
	}

	// 2. Memory Continuity: Save user image event and Gemini visual perception to conversation memory
	userMemory := fmt.Sprintf("[User sent an image. Visual description: %s]", perception)
	if cleanCaption != "" {
		userMemory = fmt.Sprintf("[User sent an image with caption '%s'. Visual description: %s]", cleanCaption, perception)
	}
	_ = b.memory.SaveMessage(ctx, chatID, senderID, username, "user", userMemory)

	// 3. Reasoning & Persona Layer (Groq = "The Brain & Voice", Gemini = Fallback)
	history, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
	summary, _ := b.memory.GetSummary(ctx, chatID)
	profile, _ := b.memory.GetUserProfile(ctx, chatID)

	aiResp, err := b.ai.GenerateVisionReply(ctx, username, isOwner, history, cleanCaption, perception, summary, profile)
	if err != nil {
		log.Printf("[Bot] AI vision reasoning error: %v", err)
		b.sendReply(chatID, replyToMsgID, b.getRandomVisionFallback())
		return
	}

	// Handle tools if Groq/Gemini decided to invoke one (e.g. analyze_token or get_balances)
	if len(aiResp.ToolCalls) > 0 {
		for _, tc := range aiResp.ToolCalls {
			toolResult := b.executeToolCall(ctx, chatID, tc.Function.Name, tc.Function.Arguments, username, isOwner)
			followup, err := b.ai.GenerateToolFollowup(ctx, username, isOwner, cleanCaption, tc.Function.Name, tc.ID, tc.Function.Arguments, toolResult, profile)
			if err != nil || followup == "" {
				followup = toolResult
			}
			b.sendReply(chatID, replyToMsgID, followup)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", followup)
			return
		}
	}

	replyText := strings.TrimSpace(aiResp.Content)
	if replyText == "" {
		replyText = "Saw that."
	}

	b.sendReply(chatID, replyToMsgID, replyText)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", replyText)
}

func (b *Bot) handleDocumentMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	senderID := msg.From.ID
	username := msg.From.UserName
	if username == "" {
		username = msg.From.FirstName
	}
	isOwner := b.cfg.IsOwner(msg.From.UserName) || b.cfg.IsOwner(msg.From.FirstName)

	isPrivate := msg.Chat.IsPrivate()
	shouldRespond := isPrivate || b.isAddressedToBot(msg) || msg.Caption != ""
	if !shouldRespond {
		return
	}

	doc := msg.Document
	if doc == nil {
		return
	}

	mime := strings.ToLower(doc.MimeType)
	ext := strings.ToLower(filepath.Ext(doc.FileName))
	cleanCaption := b.cleanPrompt(msg.Caption)

	// Check if sent as an uncompressed image file
	if strings.HasPrefix(mime, "image/") || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" {
		fileURL, err := b.api.GetFileDirectURL(doc.FileID)
		if err != nil {
			b.sendReply(chatID, msg.MessageID, "failed to get image link.")
			return
		}
		resp, err := http.Get(fileURL)
		if err != nil {
			b.sendReply(chatID, msg.MessageID, "failed to download image file.")
			return
		}
		defer resp.Body.Close()
		imgBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			b.sendReply(chatID, msg.MessageID, "failed to read image file.")
			return
		}

		b.processImage(ctx, chatID, msg.MessageID, senderID, username, isOwner, imgBytes, mime, cleanCaption)
		return
	}

	// Document types: .md, .pdf, .docx, .txt, .csv, .json
	if ext != ".md" && ext != ".pdf" && ext != ".docx" && ext != ".txt" && ext != ".csv" && ext != ".json" {
		if isPrivate {
			b.sendReply(chatID, msg.MessageID, fmt.Sprintf("I support document analysis for `.md`, `.pdf`, and `.docx` (or `.txt`/`.json`). `%s` isn't supported yet.", doc.FileName))
		}
		return
	}

	// 15MB file size limit
	if doc.FileSize > 15*1024*1024 {
		b.sendReply(chatID, msg.MessageID, "That document is too large! Please send a file under 15MB.")
		return
	}

	b.sendChatAction(chatID, tgbotapi.ChatTyping)

	fileURL, err := b.api.GetFileDirectURL(doc.FileID)
	if err != nil {
		b.sendReply(chatID, msg.MessageID, "Couldn't fetch file from Telegram.")
		return
	}

	resp, err := http.Get(fileURL)
	if err != nil {
		b.sendReply(chatID, msg.MessageID, "Failed to download document.")
		return
	}
	defer resp.Body.Close()

	docBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		b.sendReply(chatID, msg.MessageID, "Failed to read document content.")
		return
	}

	extractedText, err := docparser.ParseDocument(doc.FileName, docBytes)
	if err != nil || strings.TrimSpace(extractedText) == "" {
		b.sendReply(chatID, msg.MessageID, fmt.Sprintf("Couldn't extract text from `%s`: %v", doc.FileName, err))
		return
	}

	userDocLog := fmt.Sprintf("[User sent document: %s]", doc.FileName)
	if cleanCaption != "" {
		userDocLog = fmt.Sprintf("[User sent document %s with caption: %s]", doc.FileName, cleanCaption)
	}
	_ = b.memory.SaveMessage(ctx, chatID, senderID, username, "user", userDocLog)

	profile, _ := b.memory.GetUserProfile(ctx, chatID)
	analysis, err := b.ai.AnalyzeDocument(ctx, username, isOwner, doc.FileName, extractedText, cleanCaption, profile)
	if err != nil || analysis == "" {
		b.sendReply(chatID, msg.MessageID, "I extracted the document text, but couldn't generate the analysis.")
		return
	}

	b.sendReply(chatID, msg.MessageID, analysis)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", analysis)
}

var profileTriggerWords = []string{
	"futo", "cs", "course", "courses", "exam", "exams", "school", "assignment", "semester",
	"repo", "github.com", "stack", "react", "next", "vue", "go", "golang",
	"solana", "evm", "postgres", "redis", "tailwind", "render",
	"prefer", "i like", "always use", "never use", "building", "project",
}

func (b *Bot) maybeUpdateUserProfile(ctx context.Context, chatID int64, prompt string) {
	lower := strings.ToLower(prompt)
	hasTrigger := false
	for _, w := range profileTriggerWords {
		if strings.Contains(lower, w) {
			hasTrigger = true
			break
		}
	}
	if !hasTrigger {
		return
	}

	existing, _ := b.memory.GetUserProfile(ctx, chatID)
	updated, err := b.ai.ExtractUserProfile(ctx, prompt, existing)
	if err != nil || updated == nil {
		return
	}

	updated.ChatID = chatID
	if err := b.memory.SaveUserProfile(ctx, *updated); err != nil {
		log.Printf("[Bot] Failed to save updated UserProfile for chat %d: %v", chatID, err)
	} else {
		log.Printf("[Bot] UserProfile updated for chat %d: projects='%s' prefs='%s' life='%s'",
			chatID, updated.ActiveProjects, updated.Preferences, updated.LifeContext)
	}
}

var emojiPattern = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}\x{1F700}-\x{1F77F}\x{1F780}-\x{1F7FF}\x{1F800}-\x{1F8FF}\x{1F900}-\x{1F9FF}\x{1FA00}-\x{1FAFF}\x{2600}-\x{26FF}\x{2700}-\x{27BF}]`)
var githubURLRegex = regexp.MustCompile(`(?i)github\.com/([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+)`)
var repoSlugRegex = regexp.MustCompile(`\b([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+)\b`)
var prRegex = regexp.MustCompile(`(?i)(?:pr|pull\s*request)\s*#?(\d+)`)
var doubleBoldRegex = regexp.MustCompile(`\*\*(.+?)\*\*`)
var doubleUnderscoreRegex = regexp.MustCompile(`__(.+?)__`)

// toTelegramMarkdown converts GitHub-flavored markdown to Telegram Markdown v1.
// Telegram uses *bold* and _italic_, not **bold** / __italic__.
func toTelegramMarkdown(text string) string {
	// **bold** → *bold*
	text = doubleBoldRegex.ReplaceAllString(text, "*$1*")
	// __italic__ → _italic_  (only if not already single-underscore)
	text = doubleUnderscoreRegex.ReplaceAllString(text, "_$1_")
	return text
}


func cleanNoEmojis(text string) string {
	cleaned := emojiPattern.ReplaceAllString(text, "")
	// Replace em dashes (—) and en dashes (–) with standard hyphens
	cleaned = strings.ReplaceAll(cleaned, "—", " - ")
	cleaned = strings.ReplaceAll(cleaned, "–", " - ")
	cleaned = regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(cleaned, " ")
	lines := strings.Split(cleaned, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func extractRepoFromHistory(history []memory.Message, currentPrompt string) string {
	// 1. Check current prompt for full github URL
	if m := githubURLRegex.FindStringSubmatch(currentPrompt); len(m) > 1 {
		return strings.TrimSuffix(m[1], ".git")
	}

	// 2. Check recent messages in reverse order
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i].Content
		if m := githubURLRegex.FindStringSubmatch(msg); len(m) > 1 {
			return strings.TrimSuffix(m[1], ".git")
		}
	}

	// 3. Fallback: check for slug pattern in recent user messages
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "user" {
			continue
		}
		msg := history[i].Content
		if m := repoSlugRegex.FindStringSubmatch(msg); len(m) > 1 {
			candidate := m[1]
			lower := strings.ToLower(candidate)
			if !strings.Contains(lower, "text/") &&
				!strings.Contains(lower, "application/") &&
				!strings.Contains(lower, "and/or") &&
				!strings.Contains(lower, "w/") {
				return candidate
			}
		}
	}
	return ""
}

func extractPRNumber(history []memory.Message, prompt string) int {
	if m := prRegex.FindStringSubmatch(prompt); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if m := prRegex.FindStringSubmatch(history[i].Content); len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

func (b *Bot) sendReply(chatID int64, replyToMsgID int, text string) {
	text = toTelegramMarkdown(cleanNoEmojis(text))
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
	text = toTelegramMarkdown(cleanNoEmojis(text))
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

var dynamicChatFallbacks = []string{
	"my brain lagged for a second, run that back?",
	"hit a quick hiccup on my end, say that again?",
	"got distracted by the memepool for a sec, what'd you say?",
	"dropped a packet there, run it back bro?",
	"tripped on the chain for a second, what were you saying?",
	"whoops, mind blipped for a sec. what's that again?",
	"glitched out for a second, say that one more time?",
	"lag spiked on my end, run that by me again?",
	"lost my train of thought for a sec, hit me again anon",
	"stuttered for a second there, what'd you say?",
}

var dynamicVisionFallbacks = []string{
	"Saw the image, but hit a quick glitch processing it. run that back?",
	"Caught the pic, but my brain lagged for a second. say that again?",
	"Looked at the image, but tripped over a wire. what are we checking?",
	"Peeped that, but dropped a frame there. hit me with it again?",
	"Saw that image, but my eyes blurred for a sec. what's up with it?",
}

var dynamicEmptyAcks = []string{
	"yo, i'm with you.",
	"heard that, what's good?",
	"got you, what's next?",
	"locked in, talk to me.",
	"all eyes anon, what's the move?",
}

var dynamicWorkingAcks = []string{
	"on it.",
	"give me a sec.",
	"working on it.",
	"one sec.",
	"got it, gimme a moment.",
	"already on it.",
	"on it, just a sec.",
	"let me handle that real quick.",
}

func (b *Bot) getRandomChatFallback() string {
	return dynamicChatFallbacks[rand.Intn(len(dynamicChatFallbacks))]
}

func (b *Bot) getRandomVisionFallback() string {
	return dynamicVisionFallbacks[rand.Intn(len(dynamicVisionFallbacks))]
}

func (b *Bot) getRandomEmptyAck() string {
	return dynamicEmptyAcks[rand.Intn(len(dynamicEmptyAcks))]
}

func (b *Bot) getRandomWorkingAck() string {
	return dynamicWorkingAcks[rand.Intn(len(dynamicWorkingAcks))]
}

func (b *Bot) handleSandboxCompletion(payload sandbox.CallbackPayload) {
	if payload.ChatID == 0 {
		return
	}
	cleanOutput := strings.TrimSpace(cleanNoEmojis(payload.Output))
	if len(cleanOutput) > 3000 {
		cleanOutput = cleanOutput[:3000] + "\n... (output truncated)"
	}

	if payload.ExitCode == 0 {
		if cleanOutput == "" {
			b.sendSimpleMessage(payload.ChatID, fmt.Sprintf("runner completed cleanly in %ds (exit code 0).", payload.DurationSeconds))
		} else {
			b.sendSimpleMessage(payload.ChatID, fmt.Sprintf("runner completed in %ds (exit code 0):\n```\n%s\n```", payload.DurationSeconds, cleanOutput))
		}
	} else {
		if cleanOutput == "" {
			b.sendSimpleMessage(payload.ChatID, fmt.Sprintf("runner failed (exit code %d) in %ds.", payload.ExitCode, payload.DurationSeconds))
		} else {
			b.sendSimpleMessage(payload.ChatID, fmt.Sprintf("runner failed (exit code %d) in %ds:\n```\n%s\n```", payload.ExitCode, payload.DurationSeconds, cleanOutput))
		}
	}
}

func (b *Bot) handleSandboxTimeout(task *sandbox.Task) {
	if task == nil || task.ChatID == 0 {
		return
	}
	b.sendSimpleMessage(task.ChatID, fmt.Sprintf("sandbox task %s timed out after 6 minutes with no callback.", task.ID))
}

func isBalanceIntent(prompt string) (bool, string) {
	lower := strings.ToLower(prompt)
	hasBalanceWord := strings.Contains(lower, "balance") ||
		strings.Contains(lower, "how much do you have") ||
		strings.Contains(lower, "how much you got") ||
		strings.Contains(lower, "how much money") ||
		strings.Contains(lower, "what you got") ||
		strings.Contains(lower, "check wallet") ||
		strings.Contains(lower, "wallet balance") ||
		strings.Contains(lower, "show balance") ||
		strings.Contains(lower, "show wallet")

	if !hasBalanceWord {
		return false, ""
	}

	// Exclude non-balance concepts
	if strings.Contains(lower, "balance sheet") || strings.Contains(lower, "tree") {
		return false, ""
	}

	// Detect specific chain
	if strings.Contains(lower, "sol") || strings.Contains(lower, "solana") {
		return true, "solana"
	}
	if strings.Contains(lower, "base") {
		return true, "base"
	}
	if strings.Contains(lower, "robinhood") || strings.Contains(lower, "rh") {
		return true, "robinhood"
	}
	if strings.Contains(lower, "arbitrum") || strings.Contains(lower, "arb") {
		return true, "arbitrum"
	}
	if strings.Contains(lower, "bnb") || strings.Contains(lower, "bsc") {
		return true, "bnb"
	}
	if strings.Contains(lower, "eth") || strings.Contains(lower, "ethereum") {
		return true, "ethereum"
	}

	return true, "all"
}

