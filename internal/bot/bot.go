package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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
	"shipp/internal/domain"
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
	domain      *domain.Service
	updatesChan chan tgbotapi.Update

	// DM tracking (username -> chat_id for private DMs)
	dmMu        sync.RWMutex
	userDMChats map[string]int64

	// Proactive settings per chat
	proactiveMu       sync.RWMutex
	proactiveDisabled map[int64]bool
	lastProactiveTime map[int64]time.Time

	// Forum topic registry: chatID -> threadID -> topicName
	topicMu       sync.RWMutex
	topicRegistry map[int64]map[int]string

	// Group registry: chatID -> *GroupInfo
	groupMu       sync.RWMutex
	groupRegistry map[int64]*GroupInfo

	// Conversational momentum dialog tracking per group chat
	dialogMu      sync.RWMutex
	activeDialogs map[int64]*ActiveDialog
}

type ActiveDialog struct {
	LastBotReplyTime time.Time
	LastBotMessageID int
	LastBotSnippet   string
	LastUserID       int64
	LastUsername     string
}

type GroupInfo struct {
	ChatID   int64     `json:"chat_id"`
	Title    string    `json:"title"`
	Type     string    `json:"type"`
	Username string    `json:"username,omitempty"`
	LastSeen time.Time `json:"last_seen"`
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
	domainSvc *domain.Service,
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
		domain:            domainSvc,
		updatesChan:       make(chan tgbotapi.Update, 100),
		userDMChats:       make(map[string]int64),
		proactiveDisabled: make(map[int64]bool),
		lastProactiveTime: make(map[int64]time.Time),
		topicRegistry:     make(map[int64]map[int]string),
		groupRegistry:     make(map[int64]*GroupInfo),
		activeDialogs:     make(map[int64]*ActiveDialog),
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

// rawTelegramMessage is a lightweight struct used ONLY to extract fields that
// tgbotapi v5.5.1 does not expose (forum thread IDs, topic creation events).
type rawTelegramMessage struct {
	MessageThreadID int `json:"message_thread_id"`
	ForumTopicCreated *struct {
		Name string `json:"name"`
	} `json:"forum_topic_created"`
}

type rawTelegramUpdate struct {
	Message *rawTelegramMessage `json:"message"`
}

func (b *Bot) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	var update tgbotapi.Update
	if err := json.Unmarshal(body, &update); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Extract thread ID from raw JSON (not in tgbotapi v5.5.1 struct)
	var raw rawTelegramUpdate
	_ = json.Unmarshal(body, &raw)
	threadID := 0
	topicName := ""
	if raw.Message != nil {
		threadID = raw.Message.MessageThreadID
		if raw.Message.ForumTopicCreated != nil {
			topicName = raw.Message.ForumTopicCreated.Name
		}
	}

	if update.Message != nil {
		go b.handleMessageWithThread(context.Background(), update.Message, threadID, topicName)
	}
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) Start(ctx context.Context) error {
	if b.cfg.WebhookURL != "" {
		log.Printf("[Bot] Configuring Webhook at %s", b.cfg.WebhookURL)
		wh, err := tgbotapi.NewWebhook(b.cfg.WebhookURL)
		if err != nil {
			return fmt.Errorf("failed to create webhook config: %w", err)
		}
		if _, err := b.api.Request(wh); err != nil {
			return fmt.Errorf("failed to register webhook: %w", err)
		}
		log.Printf("[Bot] Webhook registered successfully with Telegram")

		// Start background proactive messaging engine
		go b.runProactiveEngine(ctx)
		log.Printf("[Bot] Shipp is live and listening for updates (webhook mode)...")

		// In webhook mode, handleMessageWithThread is called directly from WebhookHandler.
		// Just block until context is cancelled.
		<-ctx.Done()
		log.Printf("[Bot] Shutting down (webhook mode)...")
		return nil
	}

	log.Printf("[Bot] WebhookURL not configured. Clearing any existing webhook and using Long Polling...")
	_, _ = b.api.Request(tgbotapi.DeleteWebhookConfig{})
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.api.GetUpdatesChan(u)

	// Start background proactive messaging engine
	go b.runProactiveEngine(ctx)

	log.Printf("[Bot] Shipp is live and listening for updates (polling mode)...")

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
			// Long polling: no raw body available, threadID defaults to 0
			go b.handleMessageWithThread(ctx, update.Message, 0, "")
		}
	}
}


// ctxKeyThreadID is the context key used to pass the message_thread_id through the call chain
// so replies land in the correct forum thread.
type ctxKeyThreadID struct{}

// handleMessageWithThread is the primary entry point for all incoming messages.
// It extracts the forum thread ID (from webhook raw JSON) and topic name,
// registers new topics, injects the thread ID into context, then calls handleMessage.
func (b *Bot) handleMessageWithThread(ctx context.Context, msg *tgbotapi.Message, threadID int, topicName string) {
	chatID := msg.Chat.ID

	// If this is a forum_topic_created service message, register the topic
	if topicName != "" && threadID != 0 {
		b.topicMu.Lock()
		if b.topicRegistry[chatID] == nil {
			b.topicRegistry[chatID] = make(map[int]string)
		}
		b.topicRegistry[chatID][threadID] = topicName
		b.topicMu.Unlock()
		log.Printf("[Bot] Registered forum topic: chatID=%d threadID=%d name=%q", chatID, threadID, topicName)
		// Don't respond to pure service messages - just register and return
		return
	}

	// Inject thread ID into context for thread-aware replies
	if threadID != 0 {
		ctx = context.WithValue(ctx, ctxKeyThreadID{}, threadID)
	}

	b.handleMessage(ctx, msg)
}

func (b *Bot) recordGroup(chatID int64, title, chatType, username string) {
	b.groupMu.Lock()
	defer b.groupMu.Unlock()

	if title == "" {
		title = fmt.Sprintf("Group %d", chatID)
	}
	b.groupRegistry[chatID] = &GroupInfo{
		ChatID:   chatID,
		Title:    title,
		Type:     chatType,
		Username: username,
		LastSeen: time.Now(),
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	senderID := msg.From.ID
	username := msg.From.UserName
	if username == "" {
		username = msg.From.FirstName
	}

	// Record user ID for direct messaging (in Telegram, user's private chat ID is their numeric From.ID)
	if msg.From != nil && msg.From.UserName != "" {
		uClean := strings.ToLower(strings.TrimPrefix(msg.From.UserName, "@"))
		b.dmMu.Lock()
		b.userDMChats[uClean] = msg.From.ID
		b.dmMu.Unlock()
	}

	// Record group info whenever a message arrives from a group or supergroup
	if msg.Chat.IsGroup() || msg.Chat.IsSuperGroup() {
		b.recordGroup(msg.Chat.ID, msg.Chat.Title, msg.Chat.Type, msg.Chat.UserName)
	}
	if len(msg.NewChatMembers) > 0 {
		for _, member := range msg.NewChatMembers {
			if b.api != nil && member.ID == b.api.Self.ID {
				b.recordGroup(msg.Chat.ID, msg.Chat.Title, msg.Chat.Type, msg.Chat.UserName)
				break
			}
		}
	}
	if msg.GroupChatCreated || msg.SuperGroupChatCreated {
		b.recordGroup(msg.Chat.ID, msg.Chat.Title, msg.Chat.Type, msg.Chat.UserName)
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
	addressed := isPrivate || b.isAddressedToBot(msg)
	isFollowup, dialogSnippet := false, ""
	if !addressed && (msg.Chat.IsGroup() || msg.Chat.IsSuperGroup()) {
		isFollowup, dialogSnippet = b.isConversationalFollowup(msg, text)
	}

	shouldRespond := addressed || isFollowup
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
				b.recordActiveDialog(chatID, msg.MessageID, replyText, msg.From.ID, username)
				return
			}
		}
	}

	// Send typing indicator
	b.sendChatAction(chatID, tgbotapi.ChatTyping)

	// If this message is a reply to another message, include the quoted context
	promptWithContext := cleanPrompt
	if msg.ReplyToMessage != nil {
		quotedText := msg.ReplyToMessage.Text
		if quotedText == "" {
			quotedText = msg.ReplyToMessage.Caption
		}
		if quotedText != "" {
			quotedSender := ""
			if msg.ReplyToMessage.From != nil {
				quotedSender = msg.ReplyToMessage.From.UserName
				if quotedSender == "" {
					quotedSender = msg.ReplyToMessage.From.FirstName
				}
			}
			if b.api != nil && b.api.Self.UserName != "" && quotedSender == b.api.Self.UserName {
				promptWithContext = fmt.Sprintf("[Replying to your previous message: %q]\n%s", quotedText, cleanPrompt)
			} else if quotedSender != "" {
				promptWithContext = fmt.Sprintf("[Replying to @%s: %q]\n%s", quotedSender, quotedText, cleanPrompt)
			} else {
				promptWithContext = fmt.Sprintf("[Replying to: %q]\n%s", quotedText, cleanPrompt)
			}
		}
	} else if isFollowup && dialogSnippet != "" {
		promptWithContext = fmt.Sprintf("[Replying to your previous message: %q]\n%s", dialogSnippet, cleanPrompt)
	}

	// 3. Process via AI & NLP Tool Engine
	b.handleNLPAndChat(ctx, msg, promptWithContext, username, isOwner)
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

var otherMentionRegex = regexp.MustCompile(`(?i)@([a-zA-Z0-9_]{3,32})`)

var followupPrefixRegex = regexp.MustCompile(`(?i)^(?:are\s+(?:they|these|those|there|you|we|it)|is\s+(?:it|that|this|there|anyone)|can\s+(?:we|you|i|that|it)|could\s+(?:we|you|it|that)|will\s+(?:it|that|this|you|they)|would\s+(?:it|that|this|you|they)|should\s+(?:we|i|it|they)|does\s+|do\s+(?:they|we|you|any)|what(?:\s+about|\s+of|'s|\s+is|\s+are|\s+if|\s+do|\s+does|\s+else)?|how(?:\s+about|\s+do|\s+does|\s+can|\s+is|\s+much|\s+to|\s+come)?|which(?:\s+one|\s+of|\s+is|\s+are)?|why(?:\s+not|\s+is|\s+do|\s+does|\s+would)?|where(?:\s+can|\s+is|\s+are|\s+do)?|any\s+(?:of|other|recommendation|chance|idea))\b`)

var followupDirectivesRegex = regexp.MustCompile(`(?i)^(?:tell\s+me|explain|elaborate|break\s+it\s+down|go\s+ahead|do\s+(?:it|that)|show\s+me|expand|give\s+me|let'?s\s+do\s+it|proceed|continue)\b`)

func (b *Bot) isConversationalFollowup(msg *tgbotapi.Message, text string) (bool, string) {
	if msg == nil || text == "" {
		return false, ""
	}

	// Disqualification Gate 1: If user explicitly replied to another message (not bot)
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if b.api != nil && msg.ReplyToMessage.From.ID != b.api.Self.ID {
			return false, ""
		}
	}

	// Disqualification Gate 2: If message tags another user (@someone), it is directed at them
	matches := otherMentionRegex.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) > 1 {
			mentioned := m[1]
			if b.api != nil && b.api.Self.UserName != "" && strings.EqualFold(mentioned, b.api.Self.UserName) {
				continue
			}
			return false, "" // directed at someone else
		}
	}

	// Disqualification Gate 3: Slash commands not intended for bot
	if strings.HasPrefix(text, "/") {
		return false, ""
	}

	// Check Active Dialog state
	b.dialogMu.RLock()
	dialog, exists := b.activeDialogs[msg.Chat.ID]
	b.dialogMu.RUnlock()

	if !exists || dialog == nil {
		return false, ""
	}

	// 120 seconds momentum window
	if time.Since(dialog.LastBotReplyTime) > 120*time.Second {
		return false, ""
	}

	// Must be from the same user Shipp was speaking with
	if msg.From == nil || msg.From.ID != dialog.LastUserID {
		return false, ""
	}

	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	// Heuristic 1: Ends with a question mark
	if strings.HasSuffix(lower, "?") {
		return true, dialog.LastBotSnippet
	}

	// Heuristic 2: Starts with interrogative follow-up pattern (e.g. "Are they non KYC")
	if followupPrefixRegex.MatchString(lower) {
		return true, dialog.LastBotSnippet
	}

	// Heuristic 3: Conversational continuation directives ("tell me more", "explain", "do it")
	if followupDirectivesRegex.MatchString(lower) {
		return true, dialog.LastBotSnippet
	}

	// Heuristic 4: Short response containing reference pronouns ("they", "them", "those", "these", "it", "that")
	fields := strings.Fields(lower)
	if len(fields) <= 8 {
		for _, w := range fields {
			cleanWord := strings.Trim(w, ",.?!:;\"'")
			switch cleanWord {
			case "they", "them", "those", "these", "it", "that":
				return true, dialog.LastBotSnippet
			}
		}
	}

	return false, ""
}

func (b *Bot) recordActiveDialog(chatID int64, botMessageID int, botReplyText string, recipientID int64, recipientUsername string) {
	if chatID > 0 {
		return // Only track momentum in group/supergroup chats (chatID < 0)
	}
	snippet := strings.TrimSpace(stripHTMLTags(cleanNoEmojis(botReplyText)))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	b.dialogMu.Lock()
	defer b.dialogMu.Unlock()
	b.activeDialogs[chatID] = &ActiveDialog{
		LastBotReplyTime: time.Now(),
		LastBotMessageID: botMessageID,
		LastBotSnippet:   snippet,
		LastUserID:       recipientID,
		LastUsername:     recipientUsername,
	}
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

	case "/domain", "/domains":
		b.handleDomainCommand(ctx, msg, parts[1:])

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

	// 2a. Pre-AI balance dispatch: fetch ground-truth balances first to prevent hallucination,
	//     then format naturally through AI or clean natural fallback.
	if ok, targetChain := isBalanceIntent(prompt); ok {
		b.sendChatAction(chatID, tgbotapi.ChatTyping)
		toolResult := b.executeToolCall(ctx, chatID, "get_balances", fmt.Sprintf(`{"chain":"%s"}`, targetChain), username, isOwner)
		followup, err := b.ai.GenerateToolFollowup(ctx, username, isOwner, prompt, "get_balances", "call_balance", fmt.Sprintf(`{"chain":"%s"}`, targetChain), toolResult, profile)
		if err != nil || strings.TrimSpace(followup) == "" || strings.HasPrefix(strings.TrimSpace(followup), "{") {
			followup = formatEmergencyBalanceFallback(toolResult)
		}
		b.sendReply(chatID, msg.MessageID, followup)
		_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", followup)
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

		// 2c. Direct DM dispatch: if owner says "dm @user <msg>", dispatch immediately
		if ok, targetUser, dmText := parseDMIntent(prompt); ok {
			b.sendChatAction(chatID, tgbotapi.ChatTyping)
			argsJSON, _ := json.Marshal(map[string]interface{}{
				"recipient": targetUser,
				"message":   dmText,
			})
			toolResult := b.executeToolCall(ctx, chatID, "send_dm", string(argsJSON), username, isOwner)
			b.sendReply(chatID, msg.MessageID, toolResult)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
			return
		}
	}

	// 3. Run the agentic loop (ReAct: reason, act, observe, repeat)
	//    The loop calls the AI, executes any tool calls, feeds results back, and loops
	//    until the AI returns a plain-text final reply or the 5-iteration cap is hit.
	executor := func(toolName, arguments string) string {
		return b.executeToolCall(ctx, chatID, toolName, arguments, username, isOwner)
	}

	// For long-running async tools (github_edit_file, run_sandbox_task), ack immediately
	// and run the agentic loop in a goroutine so the user gets instant feedback.
	mightBeAsync := strings.Contains(lowerPrompt, "push") ||
		strings.Contains(lowerPrompt, "edit") ||
		strings.Contains(lowerPrompt, "update") ||
		strings.Contains(lowerPrompt, "rewrite") ||
		strings.Contains(lowerPrompt, "run") ||
		strings.Contains(lowerPrompt, "execute") ||
		strings.Contains(lowerPrompt, "sandbox")

	var chatContext string
	if msg.Chat.IsPrivate() {
		chatContext = fmt.Sprintf("- YOU ARE IN A DIRECT 1-ON-1 PRIVATE CHAT (DM) WITH @%s.\n- There is nobody else in this chat. Do NOT say 'who else', 'what is everyone doing', 'anyone here', or speak to an imaginary room. Speak directly to @%s.\n- Strictly do NOT ask eager follow-up questions.", username, username)
	} else {
		title := msg.Chat.Title
		if title == "" {
			title = "this group"
		}
		chatContext = fmt.Sprintf("- YOU ARE IN A TELEGRAM GROUP CHAT: %q.\n- Speak to the room or to @%s as appropriate.\n- Strictly do NOT ask eager follow-up questions.", title, username)
	}

	// Quick pre-flight: if prompt explicitly mentions a GitHub URL or sandbox,
	// send a working ack before the loop starts so the chat doesn't feel frozen.
	if isOwner && mightBeAsync && (extractRepoFromHistory(nil, prompt) != "" || strings.Contains(lowerPrompt, "sandbox") || strings.Contains(lowerPrompt, "run sandbox")) {
		b.sendReply(chatID, msg.MessageID, b.getRandomWorkingAck())
		go func() {
			agResult := b.ai.RunAgenticLoop(ctx, username, isOwner, history, prompt, summary, profile, executor, chatContext)
			finalText := agResult.FinalText
			if strings.TrimSpace(finalText) == "" {
				finalText = b.getRandomEmptyAck()
			}
			b.sendSimpleMessage(chatID, finalText)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", finalText)
			b.recordActiveDialog(chatID, msg.MessageID, finalText, msg.From.ID, username)
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		}()
		return
	}

	agResult := b.ai.RunAgenticLoop(ctx, username, isOwner, history, prompt, summary, profile, executor, chatContext)

	// 4. Intercept hallucinated git/merge actions from the final text (safety net)
	finalText := strings.TrimSpace(agResult.FinalText)
	if isOwner && b.tryInterceptAction(ctx, msg, prompt, lowerPrompt, username, isOwner, history, finalText) {
		go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		return
	}

	// 4b. Intercept owner alerts / ping requests (safety net)
	b.tryInterceptOwnerAlert(ctx, chatID, prompt, finalText, username, agResult.ToolsUsed)

	if finalText == "" {
		finalText = b.getRandomEmptyAck()
	}

	b.sendReply(chatID, msg.MessageID, finalText)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", finalText)
	b.recordActiveDialog(chatID, msg.MessageID, finalText, msg.From.ID, username)
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
		b.recordActiveDialog(msg.Chat.ID, msg.MessageID, toolResult, msg.From.ID, username)
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
				b.recordActiveDialog(chatID, msg.MessageID, toolResult, msg.From.ID, username)
				return true
			}
		}
	}

	return false
}

var ownerAlertClaimRegex = regexp.MustCompile(`(?i)(?:i(?:'ll|\s+will|\s+have)?\s+(?:ping|alert|notify|tell|message|reach\s+out\s+to|dm)\s+(?:the\s+|my\s+|our\s+)?(?:owner|oga|creator|dev|boss|skipp)|pinged\s+(?:the\s+|my\s+|our\s+)?(?:owner|oga|creator|dev|boss|skipp)|let\s+(?:the\s+)?(?:owner|oga|creator|dev|skipp)\s+know)`)
var ownerAlertPromptRegex = regexp.MustCompile(`(?i)(?:tell\s+(?:your\s+)?(?:oga|owner|creator|dev|boss|skipp)|ping\s+(?:your\s+)?(?:oga|owner|creator|dev|boss|skipp)|alert\s+(?:your\s+)?(?:oga|owner|creator|dev|boss|skipp)|let\s+(?:your\s+)?(?:oga|owner|creator|dev|skipp)\s+know)`)

func (b *Bot) tryInterceptOwnerAlert(ctx context.Context, chatID int64, prompt, replyText, username string, toolsUsed []string) bool {
	for _, t := range toolsUsed {
		if t == "notify_owner" || t == "ping_owner" || t == "alert_owner" {
			return false // Already sent via tool
		}
	}

	isClaim := ownerAlertClaimRegex.MatchString(replyText)
	isRequest := ownerAlertPromptRegex.MatchString(prompt)
	if !isClaim && !isRequest {
		return false
	}

	groupName := "private chat"
	if chatID < 0 {
		b.groupMu.RLock()
		gInfo := b.groupRegistry[chatID]
		b.groupMu.RUnlock()
		if gInfo != nil && gInfo.Title != "" {
			groupName = fmt.Sprintf("group %q", gInfo.Title)
		} else {
			groupName = "group chat"
		}
	}

	alertMsg := fmt.Sprintf("Alert from @%s in %s:\n\n%s", username, groupName, prompt)
	notified := b.alertOwners(ctx, alertMsg)
	log.Printf("[Bot] Intercepted owner alert: notified %v for message from @%s", notified, username)
	return len(notified) > 0
}

func (b *Bot) alertOwners(ctx context.Context, alertText string) []string {
	var notified []string
	for _, owner := range b.cfg.Owners {
		oClean := strings.ToLower(strings.TrimPrefix(owner, "@"))
		b.dmMu.RLock()
		dmChatID, exists := b.userDMChats[oClean]
		b.dmMu.RUnlock()

		if (!exists || dmChatID == 0) && b.memory != nil {
			if uid, err := b.memory.GetUserIDByUsername(ctx, oClean); err == nil && uid != 0 {
				dmChatID = uid
				exists = true
				b.dmMu.Lock()
				b.userDMChats[oClean] = uid
				b.dmMu.Unlock()
			}
		}

		if exists && dmChatID != 0 && b.api != nil {
			dmMsg := tgbotapi.NewMessage(dmChatID, alertText)
			if _, err := b.api.Send(dmMsg); err == nil {
				notified = append(notified, oClean)
				log.Printf("[Bot] Dispatched owner alert DM to @%s (%d)", oClean, dmChatID)
			} else {
				log.Printf("[Bot] Failed to send owner alert DM to @%s (%d): %v", oClean, dmChatID, err)
			}
		}
	}
	return notified
}


func (b *Bot) executeToolCall(
	ctx context.Context,
	chatID int64,
	toolName string,
	arguments string,
	username string,
	isOwner bool,
) string {
	toolName, arguments = ai.NormalizeToolCall(toolName, arguments)
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
			data, _ := json.Marshal(map[string]interface{}{
				"chain":   "solana",
				"address": svmAddr,
			})
			return string(data)
		} else if chain != "" && chain != "all" {
			data, _ := json.Marshal(map[string]interface{}{
				"chain":   chain,
				"address": evmAddr,
			})
			return string(data)
		}
		data, _ := json.Marshal(map[string]interface{}{
			"solana_address":       svmAddr,
			"evm_address":          evmAddr,
			"supported_evm_chains": []string{"base", "robinhood", "ethereum", "arbitrum", "bnb"},
		})
		return string(data)

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
				return `{"error": "solana balance unavailable"}`
			}
			fVal, _ := bal.Float64()
			usd := price.ConvertToUSD(fVal, "SOL", prices)
			data, _ := json.Marshal(map[string]interface{}{
				"chain":     "solana",
				"token":     "SOL",
				"amount":    crypto.FormatTokenAmount(bal),
				"usd_value": price.FormatUSD(usd),
				"is_dry":    fVal == 0,
			})
			return string(data)
		} else if chain != "" && chain != "all" {
			bal, err := b.crypto.GetEVMBalance(ctx, chain)
			if err != nil {
				return fmt.Sprintf(`{"error": "%s balance unavailable"}`, strings.ToLower(chain))
			}
			symbol := "ETH"
			if chain == "bnb" || chain == "bsc" {
				symbol = "BNB"
			}
			displayName := strings.ToLower(chain)
			if chain == "rh" || chain == "robinhood" {
				displayName = "robinhood"
			}
			fVal, _ := bal.Float64()
			usd := price.ConvertToUSD(fVal, symbol, prices)
			data, _ := json.Marshal(map[string]interface{}{
				"chain":     displayName,
				"token":     symbol,
				"amount":    crypto.FormatTokenAmount(bal),
				"usd_value": price.FormatUSD(usd),
				"is_dry":    fVal == 0,
			})
			return string(data)
		}

		balances, err := b.crypto.GetAllBalances(ctx)
		if err != nil {
			return `{"error": "failed to fetch balances"}`
		}
		solUSD := price.ConvertToUSD(balances.SolanaVal, "SOL", prices)
		baseUSD := price.ConvertToUSD(balances.BaseVal, "ETH", prices)
		rhUSD := price.ConvertToUSD(balances.RhVal, "ETH", prices)
		arbUSD := price.ConvertToUSD(balances.ArbVal, "ETH", prices)
		ethUSD := price.ConvertToUSD(balances.EthVal, "ETH", prices)
		bnbUSD := price.ConvertToUSD(balances.BnbVal, "BNB", prices)
		totalUSD := solUSD + baseUSD + rhUSD + arbUSD + ethUSD + bnbUSD

		type holding struct {
			Chain  string `json:"chain"`
			Token  string `json:"token"`
			Amount string `json:"amount"`
			USD    string `json:"usd"`
		}
		var active []holding
		var dry []string

		if balances.SolanaVal > 0 {
			active = append(active, holding{"solana", "SOL", balances.Solana, price.FormatUSD(solUSD)})
		} else {
			dry = append(dry, "solana")
		}
		if balances.BaseVal > 0 {
			active = append(active, holding{"base", "ETH", balances.Base, price.FormatUSD(baseUSD)})
		} else {
			dry = append(dry, "base")
		}
		if balances.RhVal > 0 {
			active = append(active, holding{"robinhood", "ETH", balances.Robinhood, price.FormatUSD(rhUSD)})
		} else {
			dry = append(dry, "robinhood")
		}
		if balances.ArbVal > 0 {
			active = append(active, holding{"arbitrum", "ETH", balances.Arbitrum, price.FormatUSD(arbUSD)})
		} else {
			dry = append(dry, "arbitrum")
		}
		if balances.EthVal > 0 {
			active = append(active, holding{"ethereum", "ETH", balances.Ethereum, price.FormatUSD(ethUSD)})
		} else {
			dry = append(dry, "ethereum")
		}
		if balances.BnbVal > 0 {
			active = append(active, holding{"bnb", "BNB", balances.BNB, price.FormatUSD(bnbUSD)})
		} else {
			dry = append(dry, "bnb")
		}

		resData, _ := json.Marshal(map[string]interface{}{
			"total_usd_value": price.FormatUSD(totalUSD),
			"active_holdings": active,
			"dry_chains":      dry,
		})
		return string(resData)

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
			return `{"error": "amount must be greater than 0"}`
		}

		prices := b.price.GetPrices(ctx)
		result, rate, err := price.Convert(args.Amount, args.From, args.To, prices)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		data, _ := json.Marshal(map[string]interface{}{
			"amount": args.Amount,
			"from":   strings.ToUpper(args.From),
			"to":     strings.ToUpper(args.To),
			"rate":   rate,
			"result": result,
		})
		return string(data)

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

		if !exists || dmChatID == 0 {
			if b.memory != nil {
				if uid, err := b.memory.GetUserIDByUsername(ctx, targetUser); err == nil && uid != 0 {
					dmChatID = uid
					exists = true
					b.dmMu.Lock()
					b.userDMChats[targetUser] = uid
					b.dmMu.Unlock()
				}
			}
		}

		if !exists || dmChatID == 0 {
			return fmt.Sprintf("cant dm @%s directly yet because telegram restricts bots from cold-dm'ing users until they message the bot first. tell @%s to send /start to @%s.", targetUser, targetUser, b.api.Self.UserName)
		}

		dmMsg := tgbotapi.NewMessage(dmChatID, fmt.Sprintf("Message from @%s via Shipp:\n\n%s", username, args.Message))
		if _, err := b.api.Send(dmMsg); err != nil {
			return fmt.Sprintf("Failed to send DM to @%s: %v", targetUser, err)
		}
		return fmt.Sprintf("Successfully sent direct message to @%s.", targetUser)

	case "get_group_topics":
		b.topicMu.RLock()
		topics := b.topicRegistry[chatID]
		b.topicMu.RUnlock()

		if len(topics) == 0 {
			return `{"topics": [], "note": "no forum topics registered yet in this chat"}`
		}
		type topicEntry struct {
			ThreadID int    `json:"thread_id"`
			Name     string `json:"name"`
		}
		var list []topicEntry
		for threadID, name := range topics {
			list = append(list, topicEntry{ThreadID: threadID, Name: name})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"topics": list,
			"count":  len(list),
		})
		return string(data)

	case "get_active_groups":
		b.groupMu.RLock()
		groups := make([]*GroupInfo, 0, len(b.groupRegistry))
		for _, g := range b.groupRegistry {
			groups = append(groups, g)
		}
		b.groupMu.RUnlock()

		if len(groups) == 0 {
			if chatID < 0 {
				return fmt.Sprintf(`{"groups": [{"title": "this group", "chat_id": %d, "type": "group"}], "total": 1, "note": "only recorded current group since last restart"}`, chatID)
			}
			return `{"groups": [], "total": 0, "note": "no active groups recorded yet since last bot restart"}`
		}

		data, _ := json.Marshal(map[string]interface{}{
			"groups": groups,
			"total":  len(groups),
		})
		return string(data)

	case "vercel_search_domains", "search_domains":
		if b.domain == nil {
			return `{"error": "domain registrar service not initialized"}`
		}
		var args struct {
			Domains []string `json:"domains"`
			Query   string   `json:"query"`
			Domain  string   `json:"domain"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)

		inputList := args.Domains
		if len(inputList) == 0 {
			rawQuery := args.Query
			if rawQuery == "" {
				rawQuery = args.Domain
			}
			if rawQuery != "" {
				inputList = b.domain.ParseInputDomains(rawQuery)
			}
		}
		if len(inputList) == 0 {
			return "Please specify domain names or a brand name to search (e.g. 'curtainrh.com' or 'liegeagents')."
		}

		res, err := b.domain.SearchDomains(ctx, inputList)
		if err != nil {
			return fmt.Sprintf("Domain search error: %v", err)
		}
		return b.domain.FormatResponse(res)

	case "notify_owner", "ping_owner", "alert_owner":
		var args struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		content := strings.TrimSpace(args.Message)
		if content == "" {
			content = arguments
		}

		groupName := "private chat"
		if chatID < 0 {
			b.groupMu.RLock()
			gInfo := b.groupRegistry[chatID]
			b.groupMu.RUnlock()
			if gInfo != nil && gInfo.Title != "" {
				groupName = fmt.Sprintf("group %q", gInfo.Title)
			} else {
				groupName = "group chat"
			}
		}

		dmText := fmt.Sprintf("Alert from @%s in %s:\n\n%s", username, groupName, content)
		notified := b.alertOwners(ctx, dmText)

		if len(notified) > 0 {
			return fmt.Sprintf("Delivered alert to owner (@%s): %s", strings.Join(notified, ", @"), content)
		}
		return fmt.Sprintf("Recorded alert for owner (@%s): %s", strings.Join(b.cfg.Owners, ", @"), content)

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
	b.recordActiveDialog(msg.Chat.ID, msg.MessageID, replyText, msg.From.ID, msg.From.UserName)
}

func (b *Bot) handleDomainCommand(ctx context.Context, msg *tgbotapi.Message, args []string) {
	if b.domain == nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Domain search service is not initialized.")
		return
	}
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/domain <name or domain>` (e.g. `/domain curtainrh.com` or `/domains liegeagents`)")
		return
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	res, err := b.domain.Search(ctx, query)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Domain search failed: %v", err))
		return
	}

	formatted := b.domain.FormatResponse(res)
	b.sendReply(msg.Chat.ID, msg.MessageID, formatted)
	_ = b.memory.SaveMessage(ctx, msg.Chat.ID, b.api.Self.ID, b.api.Self.UserName, "assistant", formatted)
	b.recordActiveDialog(msg.Chat.ID, msg.MessageID, formatted, msg.From.ID, msg.From.UserName)
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
		// NEVER send proactive messages to private chats! In Telegram, private DMs have chatID > 0.
		// Proactive messages are strictly for group chats (chatID < 0).
		if chatID > 0 {
			continue
		}

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
		"• `/domain <name>` or `/domains <name>` - Search Vercel domain availability & pricing\n" +
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
var leakedToolCallRegex = regexp.MustCompile(`(?si)<(?:toolcall|tool_call)[^>]*>.*?</(?:toolcall|tool_call)>`)
var leakedFunctionRegex = regexp.MustCompile(`(?si)<function(?:=|\s+name=)[^>]*>.*?</function>`)
var leakedDeclarationRegex = regexp.MustCompile(`(?si)(?:declaration|call):default_api:[a-zA-Z0-9_]+\s*\{.*?\}?`)
var eagerPromptRegex = regexp.MustCompile(`(?i)(?:,\s*|\.\s*|\s+)(?:what(?:'s|\s+is)\s+next\??|what\s+are\s+we\s+building(?:\s+next)?\??|what(?:'s|\s+is)\s+(?:the\s+)?(?:next\s+)?move\??|what\s+are\s+we\s+cooking(?:\s+next)?\??|what\s+are\s+we\s+doing(?:\s+next)?\??|how\s+can\s+i\s+help(?:\s+you)?\??|who\s+else\s+is\s+building[^?.!\n]*\??|anyone\s+(?:else\s+)?(?:actually\s+)?(?:shipping|building)[^?.!\n]*\??|are\s+we\s+all\s+just\s+staring\s+at\s+charts\??)\s*$`)

var dmIntentRegex = regexp.MustCompile(`(?i)^(?:/dm|dm|send\s+dm\s+to|dm\s+to)\s+@?([a-zA-Z0-9_]{3,32})[\s:,]+(.+)$`)

func parseDMIntent(prompt string) (bool, string, string) {
	trimmed := strings.TrimSpace(prompt)
	m := dmIntentRegex.FindStringSubmatch(trimmed)
	if len(m) == 3 {
		target := strings.TrimSpace(m[1])
		msg := strings.TrimSpace(m[2])
		if target != "" && msg != "" {
			return true, target, msg
		}
	}
	return false, "", ""
}

// toTelegramHTML converts Markdown/text to clean Telegram-compatible HTML.
// Telegram HTML avoids entity-parsing crashes caused by underscores in usernames/emails/URLs.
func toTelegramHTML(text string) string {
	if text == "" {
		return ""
	}

	// 1. Normalize bullet points (* or - at line start) to clean Unicode bullet (•)
	// This prevents "* *Bold Title*" syntax collisions.
	bulletRegex := regexp.MustCompile(`(?m)^[\t ]*[\*\-][\t ]+`)
	text = bulletRegex.ReplaceAllString(text, "• ")

	// 2. Protect multi-line code blocks
	var codeBlocks []string
	codeBlockRegex := regexp.MustCompile("(?s)```(?:[a-zA-Z0-9_+-]+)?\n?(.*?)```")
	text = codeBlockRegex.ReplaceAllStringFunc(text, func(m string) string {
		sub := codeBlockRegex.FindStringSubmatch(m)
		if len(sub) > 1 {
			content := html.EscapeString(sub[1])
			idx := len(codeBlocks)
			codeBlocks = append(codeBlocks, fmt.Sprintf("<pre><code>%s</code></pre>", content))
			return fmt.Sprintf("___CODE_BLOCK_%d___", idx)
		}
		return m
	})

	// 3. Protect inline code
	var inlineCodes []string
	inlineCodeRegex := regexp.MustCompile("`([^`\n]+)`")
	text = inlineCodeRegex.ReplaceAllStringFunc(text, func(m string) string {
		sub := inlineCodeRegex.FindStringSubmatch(m)
		if len(sub) > 1 {
			content := html.EscapeString(sub[1])
			idx := len(inlineCodes)
			inlineCodes = append(inlineCodes, fmt.Sprintf("<code>%s</code>", content))
			return fmt.Sprintf("___INLINE_CODE_%d___", idx)
		}
		return m
	})

	// 4. HTML escape remaining plain text
	text = html.EscapeString(text)

	// 5. Convert links: [text](url) -> <a href="url">text</a>
	linkRegex := regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	text = linkRegex.ReplaceAllString(text, `<a href="$2">$1</a>`)

	// 6. Convert **bold** -> <b>bold</b>
	boldRegex := regexp.MustCompile(`\*\*(.+?)\*\*`)
	text = boldRegex.ReplaceAllString(text, "<b>$1</b>")

	// 7. Convert single *bold* (markdown legacy) -> <b>bold</b>
	singleBoldRegex := regexp.MustCompile(`(?:^|[\s(])\*([^*\n\t]+?)\*(?:[\s),.:!?]|$)`)
	text = singleBoldRegex.ReplaceAllStringFunc(text, func(m string) string {
		start := strings.Index(m, "*")
		end := strings.LastIndex(m, "*")
		if start >= 0 && end > start {
			return m[:start] + "<b>" + m[start+1:end] + "</b>" + m[end+1:]
		}
		return m
	})

	// 8. Restore code blocks and inline code
	for idx, code := range inlineCodes {
		text = strings.ReplaceAll(text, fmt.Sprintf("___INLINE_CODE_%d___", idx), code)
	}
	for idx, block := range codeBlocks {
		text = strings.ReplaceAll(text, fmt.Sprintf("___CODE_BLOCK_%d___", idx), block)
	}

	return text
}

var htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

func stripHTMLTags(s string) string {
	s = htmlTagRegex.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(s)
}

// toTelegramMarkdown converts GitHub-flavored markdown to Telegram Markdown v1.
// Kept for backward-compatibility with tests.
func toTelegramMarkdown(text string) string {
	text = doubleBoldRegex.ReplaceAllString(text, "*$1*")
	text = doubleUnderscoreRegex.ReplaceAllString(text, "_$1_")
	return text
}


func deduplicateRepeatedHalf(s string) string {
	s = strings.TrimSpace(s)
	n := len(s)
	if n < 16 {
		return s
	}

	for offset := -8; offset <= 8; offset++ {
		mid := n/2 + offset
		if mid <= 4 || mid >= n-4 {
			continue
		}

		left := strings.TrimSpace(s[:mid])
		right := strings.TrimSpace(s[mid:])

		trimL := strings.Trim(left, " .!?-—–\t\r\n")
		trimR := strings.Trim(right, " .!?-—–\t\r\n")

		if len(trimL) >= 8 && strings.EqualFold(trimL, trimR) {
			trailing := ""
			if strings.HasSuffix(right, ".") || strings.HasSuffix(left, ".") {
				trailing = "."
			} else if strings.HasSuffix(right, "!") || strings.HasSuffix(left, "!") {
				trailing = "!"
			} else if strings.HasSuffix(right, "?") || strings.HasSuffix(left, "?") {
				trailing = "?"
			}
			return strings.TrimRight(trimL, " .!?-—–\t\r\n") + trailing
		}
	}
	return s
}

func deduplicateResponse(text string) string {
	text = strings.TrimSpace(text)
	if len(text) < 16 {
		return text
	}

	// 1. Check if the entire string is a duplicated half
	if deduped := deduplicateRepeatedHalf(text); deduped != text {
		return deduped
	}

	// 2. Check if a suffix after a delimiter is duplicated
	delimRegex := regexp.MustCompile(`([.!?\n]+|\s+-\s+)`)
	matches := delimRegex.FindAllStringIndex(text, -1)

	for _, loc := range matches {
		boundary := loc[1]
		if boundary >= len(text)-16 {
			continue
		}
		prefix := text[:boundary]
		suffix := strings.TrimSpace(text[boundary:])

		if dedupedSuffix := deduplicateRepeatedHalf(suffix); dedupedSuffix != suffix {
			return strings.TrimSpace(prefix + " " + dedupedSuffix)
		}
	}

	return text
}

func formatEmergencyBalanceFallback(toolResult string) string {
	var data struct {
		TotalUSD string `json:"total_usd_value"`
		Holdings []struct {
			Chain  string `json:"chain"`
			Token  string `json:"token"`
			Amount string `json:"amount"`
			USD    string `json:"usd"`
		} `json:"active_holdings"`
		Chain  string `json:"chain"`
		Amount string `json:"amount"`
		Token  string `json:"token"`
		USDVal string `json:"usd_value"`
		IsDry  bool   `json:"is_dry"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(toolResult), &data); err != nil {
		return toolResult
	}
	if data.Error != "" {
		return data.Error
	}
	if data.Chain != "" {
		if data.IsDry {
			return fmt.Sprintf("%s is dry ($0.00 right now).", data.Chain)
		}
		return fmt.Sprintf("got %s %s on %s (~%s).", data.Amount, data.Token, data.Chain, data.USDVal)
	}
	if len(data.Holdings) == 0 {
		return "wallets are dry right now, $0.00 across all chains."
	}
	if len(data.Holdings) == 1 {
		h := data.Holdings[0]
		return fmt.Sprintf("sitting on about %s on %s right now (%s).", h.USD, h.Chain, h.Amount)
	}
	var parts []string
	for _, h := range data.Holdings {
		parts = append(parts, fmt.Sprintf("%s on %s (~%s)", h.Amount, h.Chain, h.USD))
	}
	return fmt.Sprintf("sitting on about %s total: %s.", data.TotalUSD, strings.Join(parts, ", "))
}

func formatEmergencyConvertFallback(toolResult string) string {
	var data struct {
		Amount float64 `json:"amount"`
		From   string  `json:"from"`
		To     string  `json:"to"`
		Rate   float64 `json:"rate"`
		Result float64 `json:"result"`
		Error  string  `json:"error"`
	}
	if err := json.Unmarshal([]byte(toolResult), &data); err != nil {
		return toolResult
	}
	if data.Error != "" {
		return data.Error
	}
	if data.To == "USD" {
		return fmt.Sprintf("%.6f %s is about $%.2f USD.", data.Amount, data.From, data.Result)
	}
	return fmt.Sprintf("%.2f %s is about %.6f %s.", data.Amount, data.From, data.Result, data.To)
}

func formatEmergencyAddressFallback(toolResult string) string {
	var data struct {
		SolanaAddr string   `json:"solana_address"`
		EVMAddr    string   `json:"evm_address"`
		Chain      string   `json:"chain"`
		Address    string   `json:"address"`
		EVMChains  []string `json:"supported_evm_chains"`
	}
	if err := json.Unmarshal([]byte(toolResult), &data); err != nil {
		return toolResult
	}
	if data.Chain != "" {
		return fmt.Sprintf("%s deposit address: `%s`", strings.ToUpper(data.Chain), data.Address)
	}
	return fmt.Sprintf("Solana: `%s`\nEVM: `%s`", data.SolanaAddr, data.EVMAddr)
}

func cleanNoEmojis(text string) string {
	cleaned := leakedToolCallRegex.ReplaceAllString(text, "")
	cleaned = leakedFunctionRegex.ReplaceAllString(cleaned, "")
	cleaned = leakedDeclarationRegex.ReplaceAllString(cleaned, "")
	cleaned = eagerPromptRegex.ReplaceAllString(cleaned, "")
	cleaned = emojiPattern.ReplaceAllString(cleaned, "")
	// Replace em dashes (—) and en dashes (–) with standard hyphens
	cleaned = strings.ReplaceAll(cleaned, "—", " - ")
	cleaned = strings.ReplaceAll(cleaned, "–", " - ")
	cleaned = regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(cleaned, " ")
	lines := strings.Split(cleaned, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " ")
	}
	res := strings.TrimSpace(strings.Join(lines, "\n"))
	return deduplicateResponse(res)
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

// sendReply sends a reply. If ctx carries a forum thread ID it sends inside that thread.
func (b *Bot) sendReply(chatID int64, replyToMsgID int, text string) {
	b.sendReplyCtx(context.Background(), chatID, replyToMsgID, text)
}

func (b *Bot) sendReplyCtx(ctx context.Context, chatID int64, replyToMsgID int, text string) {
	htmlText := toTelegramHTML(cleanNoEmojis(text))

	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}

	if threadID != 0 {
		b.sendViaThreadAPI(ctx, chatID, threadID, replyToMsgID, htmlText)
		return
	}

	msg := tgbotapi.NewMessage(chatID, htmlText)
	msg.ParseMode = "HTML"
	if replyToMsgID > 0 {
		msg.ReplyToMessageID = replyToMsgID
	}
	_, err := b.api.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		msg.Text = stripHTMLTags(htmlText)
		_, _ = b.api.Send(msg)
	}
}

// sendSimpleMessage sends without replying to a specific message. Thread-aware via ctx.
func (b *Bot) sendSimpleMessage(chatID int64, text string) {
	b.sendSimpleMessageCtx(context.Background(), chatID, text)
}

func (b *Bot) sendSimpleMessageCtx(ctx context.Context, chatID int64, text string) {
	htmlText := toTelegramHTML(cleanNoEmojis(text))

	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}

	if threadID != 0 {
		b.sendViaThreadAPI(ctx, chatID, threadID, 0, htmlText)
		return
	}

	msg := tgbotapi.NewMessage(chatID, htmlText)
	msg.ParseMode = "HTML"
	_, err := b.api.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		msg.Text = stripHTMLTags(htmlText)
		_, _ = b.api.Send(msg)
	}
}

// sendViaThreadAPI sends a message into a specific Telegram forum thread using
// the raw MakeRequest path, since tgbotapi v5.5.1 BaseChat lacks MessageThreadID.
func (b *Bot) sendViaThreadAPI(_ context.Context, chatID int64, threadID int, replyToMsgID int, htmlText string) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", htmlText)
	params.AddNonEmpty("parse_mode", "HTML")
	params.AddNonZero("message_thread_id", threadID)
	if replyToMsgID > 0 {
		params.AddNonZero("reply_to_message_id", replyToMsgID)
	}
	_, err := b.api.MakeRequest("sendMessage", params)
	if err != nil {
		// Fallback: retry without parse mode and strip any broken HTML tags
		params["parse_mode"] = ""
		params["text"] = stripHTMLTags(htmlText)
		_, _ = b.api.MakeRequest("sendMessage", params)
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
	"heard you.",
	"locked in.",
	"got that.",
	"loud and clear.",
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

	// Exclude non-balance concepts early
	if strings.Contains(lower, "balance sheet") || strings.Contains(lower, "tree") || strings.Contains(lower, "cooking") {
		return false, ""
	}

	hasBalanceWord := strings.Contains(lower, "balance") ||
		strings.Contains(lower, "check wallet") ||
		strings.Contains(lower, "show wallet") ||
		strings.Contains(lower, "how much money") ||
		strings.Contains(lower, "how much you got") ||
		strings.Contains(lower, "what you got")

	if !hasBalanceWord && strings.Contains(lower, "how much") {
		if strings.Contains(lower, "have") || strings.Contains(lower, "got") ||
			strings.Contains(lower, "sol") || strings.Contains(lower, "eth") || strings.Contains(lower, "bnb") || strings.Contains(lower, "crypto") {
			hasBalanceWord = true
		}
	}

	if !hasBalanceWord {
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

