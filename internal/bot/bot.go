package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	"shipp/internal/moltbook"
	"shipp/internal/price"
	"shipp/internal/sandbox"
	"shipp/internal/search"
	"shipp/internal/token"
	"shipp/internal/vision"
	"shipp/internal/xhandle"
	"shipp/internal/xpost"
)

type RecentDocInfo struct {
	Document   *tgbotapi.Document
	ReceivedAt time.Time
	SenderID   int64
	Username   string
}

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
	xhandle     *xhandle.Service
	xpost       *xpost.Service
	moltbook    *moltbook.Client
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

	// Recent documents cached per chat
	recentDocMu sync.RWMutex
	recentDocs  map[int64]*RecentDocInfo

	// Conversational momentum dialog tracking per group chat
	dialogMu      sync.RWMutex
	activeDialogs map[int64]*ActiveDialog

	// Message thread tracking: chatID -> msgID -> threadID
	msgThreadMu    sync.RWMutex
	msgThreads     map[int64]map[int]int
	chatLastThread map[int64]int
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
	xhandleSvc *xhandle.Service,
	moltbookSvc *moltbook.Client,
) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("failed to init telegram bot api: %w", err)
	}

	log.Printf("[Bot] Authorized on account @%s (ID: %d)", api.Self.UserName, api.Self.ID)

	if xhandleSvc == nil {
		xhandleSvc = xhandle.NewService()
	}

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
		xhandle:           xhandleSvc,
		xpost:             xpost.NewService(),
		moltbook:          moltbookSvc,
		updatesChan:       make(chan tgbotapi.Update, 100),
		userDMChats:       make(map[string]int64),
		proactiveDisabled: make(map[int64]bool),
		lastProactiveTime: make(map[int64]time.Time),
		topicRegistry:     make(map[int64]map[int]string),
		groupRegistry:     make(map[int64]*GroupInfo),
		recentDocs:        make(map[int64]*RecentDocInfo),
		activeDialogs:     make(map[int64]*ActiveDialog),
		msgThreads:        make(map[int64]map[int]int),
		chatLastThread:    make(map[int64]int),
	}

	b.loadGroupsFromDisk()

	if sandboxSvc != nil {
		sandboxSvc.SetHandlers(func(payload sandbox.CallbackPayload) {
			b.handleSandboxCompletion(payload)
		}, func(task *sandbox.Task) {
			b.handleSandboxTimeout(task)
		})
	}

	if b.memory != nil && aiClient != nil {
		if identities, err := b.memory.GetAllSelfIdentity(context.Background()); err == nil && len(identities) > 0 {
			aiClient.SetIdentity(identities)
			log.Printf("[Bot] Loaded %d living self-identity beliefs into AI mind", len(identities))
		}
	}

	return b, nil
}

func (b *Bot) recordMsgThread(chatID int64, msgID int, threadID int) {
	if threadID == 0 {
		return
	}
	b.msgThreadMu.Lock()
	defer b.msgThreadMu.Unlock()
	if b.msgThreads == nil {
		b.msgThreads = make(map[int64]map[int]int)
	}
	if b.msgThreads[chatID] == nil {
		b.msgThreads[chatID] = make(map[int]int)
	}
	b.msgThreads[chatID][msgID] = threadID
	if b.chatLastThread == nil {
		b.chatLastThread = make(map[int64]int)
	}
	b.chatLastThread[chatID] = threadID
}

func (b *Bot) lookupMsgThread(chatID int64, msgID int) int {
	b.msgThreadMu.RLock()
	defer b.msgThreadMu.RUnlock()
	if b.msgThreads != nil && b.msgThreads[chatID] != nil {
		if tid, ok := b.msgThreads[chatID][msgID]; ok && tid != 0 {
			return tid
		}
	}
	return 0
}

func (b *Bot) lookupChatThread(chatID int64) int {
	b.msgThreadMu.RLock()
	defer b.msgThreadMu.RUnlock()
	if b.chatLastThread != nil {
		return b.chatLastThread[chatID]
	}
	return 0
}

// rawTelegramMessage is a lightweight struct used ONLY to extract fields that
// tgbotapi v5.5.1 does not expose (forum thread IDs, topic creation events).
type rawTelegramMessage struct {
	MessageID       int  `json:"message_id"`
	MessageThreadID int  `json:"message_thread_id"`
	IsTopicMessage  bool `json:"is_topic_message"`
	ReplyToMessage  *struct {
		MessageID       int `json:"message_id"`
		MessageThreadID int `json:"message_thread_id"`
	} `json:"reply_to_message"`
	ForumTopicCreated *struct {
		Name string `json:"name"`
	} `json:"forum_topic_created"`
}

type rawTelegramUpdate struct {
	UpdateID int                 `json:"update_id"`
	Message  *rawTelegramMessage `json:"message"`
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
		if threadID == 0 && raw.Message.ReplyToMessage != nil {
			threadID = raw.Message.ReplyToMessage.MessageThreadID
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

	// Start background proactive messaging engine
	go b.runProactiveEngine(ctx)

	log.Printf("[Bot] Shipp is live and listening for updates (polling mode)...")

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Bot] Shutting down update loop...")
			return nil
		default:
		}

		rawResp, err := b.api.Request(u)
		if err != nil {
			log.Printf("[Bot] Polling error: %v. Retrying in 3s...", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if rawResp == nil || !rawResp.Ok || len(rawResp.Result) == 0 {
			continue
		}

		var updates []tgbotapi.Update
		if err := json.Unmarshal(rawResp.Result, &updates); err != nil {
			continue
		}

		var rawUpdates []rawTelegramUpdate
		_ = json.Unmarshal(rawResp.Result, &rawUpdates)

		for i, update := range updates {
			if update.UpdateID >= u.Offset {
				u.Offset = update.UpdateID + 1
			}
			if update.Message == nil {
				continue
			}

			threadID := 0
			topicName := ""
			if i < len(rawUpdates) && rawUpdates[i].Message != nil {
				threadID = rawUpdates[i].Message.MessageThreadID
				if rawUpdates[i].Message.ForumTopicCreated != nil {
					topicName = rawUpdates[i].Message.ForumTopicCreated.Name
				}
				if threadID == 0 && rawUpdates[i].Message.ReplyToMessage != nil {
					threadID = rawUpdates[i].Message.ReplyToMessage.MessageThreadID
				}
			}

			go b.handleMessageWithThread(ctx, update.Message, threadID, topicName)
		}
	}
}


// ctxKeyThreadID is the context key used to pass the message_thread_id through the call chain
// so replies land in the correct forum thread.
type ctxKeyThreadID struct{}

// ctxKeyReplyToMsgID is the context key used to pass the message_id through the call chain
// so asynchronous callbacks (like sandbox execution) reply to the exact prompt message.
type ctxKeyReplyToMsgID struct{}

// ctxKeyPrompt is the context key used to pass the original user prompt through the call chain
// so asynchronous callbacks (like sandbox execution) can synthesize an AI response based on the original question.
type ctxKeyPrompt struct{}

// handleMessageWithThread is the primary entry point for all incoming messages.
// It extracts the forum thread ID (from webhook raw JSON) and topic name,
// registers new topics, injects the thread ID into context, then calls handleMessage.
func (b *Bot) handleMessageWithThread(ctx context.Context, msg *tgbotapi.Message, threadID int, topicName string) {
	chatID := msg.Chat.ID
	ctx = context.WithValue(ctx, ctxKeyReplyToMsgID{}, msg.MessageID)
	ctx = context.WithValue(ctx, ctxKeyPrompt{}, msg.Text)

	// If this is a forum_topic_created service message, register the topic
	if topicName != "" && threadID != 0 {
		b.topicMu.Lock()
		if b.topicRegistry[chatID] == nil {
			b.topicRegistry[chatID] = make(map[int]string)
		}
		b.topicRegistry[chatID][threadID] = topicName
		b.topicMu.Unlock()
		b.recordMsgThread(chatID, msg.MessageID, threadID)
		log.Printf("[Bot] Registered forum topic: chatID=%d threadID=%d name=%q", chatID, threadID, topicName)
		// Don't respond to pure service messages - just register and return
		return
	}

	// Record thread ID in tracking maps
	if threadID != 0 {
		b.recordMsgThread(chatID, msg.MessageID, threadID)
		ctx = context.WithValue(ctx, ctxKeyThreadID{}, threadID)
	} else if msg.ReplyToMessage != nil {
		if parentThread := b.lookupMsgThread(chatID, msg.ReplyToMessage.MessageID); parentThread != 0 {
			threadID = parentThread
			b.recordMsgThread(chatID, msg.MessageID, threadID)
			ctx = context.WithValue(ctx, ctxKeyThreadID{}, threadID)
		}
	}

	b.handleMessage(ctx, msg)
}

func defaultGroupsStoragePath() string {
	if dir := os.Getenv("DATA_DIR"); dir != "" {
		return filepath.Join(dir, "groups_registry.json")
	}
	return "groups_registry.json"
}

func (b *Bot) loadGroupsFromDisk() {
	path := defaultGroupsStoragePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var stored map[int64]*GroupInfo
	if err := json.Unmarshal(data, &stored); err == nil && len(stored) > 0 {
		b.groupMu.Lock()
		for id, g := range stored {
			b.groupRegistry[id] = g
		}
		b.groupMu.Unlock()
		log.Printf("[Bot] Loaded %d groups from %s", len(stored), path)
	}
}

func (b *Bot) saveGroupsToDisk() {
	path := defaultGroupsStoragePath()
	b.groupMu.RLock()
	data, err := json.MarshalIndent(b.groupRegistry, "", "  ")
	b.groupMu.RUnlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

func (b *Bot) recordGroup(chatID int64, title, chatType, username string) {
	b.groupMu.Lock()
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
	b.groupMu.Unlock()
	b.saveGroupsToDisk()
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

	isOwner := b.isSenderOwner(msg.From)
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

	// Document inspection: reply-to document or query about recent document
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.Document != nil {
		b.processDocument(ctx, chatID, msg.MessageID, senderID, username, isOwner, msg.ReplyToMessage.Document, cleanPrompt)
		return
	}

	lowerPrompt := strings.ToLower(cleanPrompt)
	if isDocReadQuery(lowerPrompt) {
		b.recentDocMu.RLock()
		recentDoc, hasRecent := b.recentDocs[chatID]
		b.recentDocMu.RUnlock()
		if hasRecent && recentDoc != nil && time.Since(recentDoc.ReceivedAt) < 30*time.Minute {
			b.processDocument(ctx, chatID, msg.MessageID, senderID, username, isOwner, recentDoc.Document, cleanPrompt)
			return
		}
	}

	// Direct Token CA detection (fast path when primarily a CA paste)
	rawAddr, rawChain := token.ExtractAddressAndChain(cleanPrompt)
	isTransferIntent := hasTransferOrWalletIntent(cleanPrompt) ||
		(msg.ReplyToMessage != nil && (hasTransferOrWalletIntent(msg.ReplyToMessage.Text) || hasTransferOrWalletIntent(msg.ReplyToMessage.Caption))) ||
		(isFollowup && hasTransferOrWalletIntent(dialogSnippet))

	if rawAddr != "" && len(strings.Fields(cleanPrompt)) <= 3 && !isTransferIntent {
		b.sendChatAction(chatID, tgbotapi.ChatTyping)
		res, err := b.token.AnalyzeToken(ctx, rawAddr, rawChain)
		if err == nil && res != nil {
			var replyText string
			switch res.Status {
			case token.StatusAmbiguousChain:
				replyText = token.FormatAmbiguousChains(res.Address, res.CandidateChains)
			case token.StatusSuccess:
				lower := strings.ToLower(cleanPrompt)
				if strings.Contains(lower, "detailed") || strings.Contains(lower, "details") || strings.Contains(lower, "full") || strings.Contains(lower, "breakdown") || strings.Contains(lower, "more") {
					replyText = token.FormatCard(res.Metrics)
				} else {
					replyText = token.FormatNatural(res.Metrics)
				}
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

var followupPrefixRegex = regexp.MustCompile(`(?i)^(?:are\s+(?:they|these|those|there|you|we|it)|is\s+(?:it|that|this|there|anyone|the\s+x|the\s+twitter)|can\s+(?:we|you|i|that|it)|could\s+(?:we|you|it|that)|will\s+(?:it|that|this|you|they)|would\s+(?:it|that|this|you|they)|should\s+(?:we|i|it|they)|does\s+|do\s+(?:they|we|you|any)|what(?:\s+about|\s+of|'s|\s+is|\s+are|\s+if|\s+do|\s+does|\s+else)?|how(?:\s+about|\s+do|\s+does|\s+can|\s+is|\s+much|\s+to|\s+come)?|which(?:\s+one|\s+of|\s+is|\s+are)?|why(?:\s+not|\s+is|\s+do|\s+does|\s+would)?|where(?:\s+can|\s+is|\s+are|\s+do)?|any\s+(?:of|other|recommendation|chance|idea)|check\s+(?:x|twitter|handle|domain|ca))\b`)

var followupDirectivesRegex = regexp.MustCompile(`(?i)^(?:tell\s+me|explain|elaborate|break\s+it\s+down|go\s+ahead|do\s+(?:it|that)|show\s+me|expand|give\s+me|let'?s\s+do\s+it|proceed|continue)\b`)

func (b *Bot) isConversationalFollowup(msg *tgbotapi.Message, text string) (bool, string) {
	if msg == nil || text == "" {
		return false, ""
	}

	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	// Disqualification Gate 1: If user explicitly replied to another message (not bot)
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if b.api != nil && msg.ReplyToMessage.From.ID != b.api.Self.ID {
			return false, ""
		}
	}

	// Disqualification Gate 2: If message tags another user (@someone), it is directed at them.
	// Exception: If the message is asking about an X/social handle (e.g. "is @liegeagents username available on X")
	isHandleQuery := strings.Contains(lower, "on x") || strings.Contains(lower, "on twitter") || strings.Contains(lower, "x handle") || strings.Contains(lower, "twitter handle") || strings.Contains(lower, "username") || strings.Contains(lower, "handle")
	if !isHandleQuery {
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

	// Heuristic 5: Short TLD extension follow-up (e.g. ".com", "check .io", ".xyz")
	if len(fields) <= 5 && len(domain.ExtractRequestedTLDs(lower)) > 0 {
		return true, dialog.LastBotSnippet
	}

	// Heuristic 6: Social handle / username availability query (e.g. "is @liegeagents username available on X")
	if isHandleQuery && (strings.HasPrefix(lower, "is ") || strings.HasPrefix(lower, "check ") || strings.Contains(lower, "available") || strings.Contains(lower, "@")) {
		return true, dialog.LastBotSnippet
	}

	// Heuristic 7: Crypto address provided during active dialog momentum
	if addr, _ := token.ExtractAddressAndChain(trimmed); addr != "" {
		return true, dialog.LastBotSnippet
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

func hasTransferOrWalletIntent(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	transferKeywords := []string{
		"send", "transfer", "pay", "to", "move", "deposit",
		"withdraw", "wallet", "recipient", "payout", "fund", "funds",
		"address", "where", "drop",
	}
	words := strings.Fields(lower)
	for _, w := range words {
		cleaned := strings.Trim(w, ",.:;!?()'\"[]{}")
		for _, kw := range transferKeywords {
			if cleaned == kw {
				return true
			}
		}
	}
	return false
}

func (b *Bot) isSenderOwner(from *tgbotapi.User) bool {
	if from == nil {
		return false
	}
	if b.cfg.IsOwner(from.UserName) || b.cfg.IsOwner(from.FirstName) || b.cfg.IsOwner(from.LastName) {
		return true
	}
	fullName := strings.TrimSpace(from.FirstName + " " + from.LastName)
	return b.cfg.IsOwner(fullName)
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message, isOwner bool) {
	username := ""
	if msg.From != nil {
		username = msg.From.UserName
		if username == "" {
			username = msg.From.FirstName
		}
	}
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

	case "/x", "/xhandle", "/twitter":
		b.handleXCommand(ctx, msg, parts[1:])

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
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("**Search Results for '%s':**\n\n%s", query, res))

	case "/email":
		b.handleEmailCommand(ctx, msg, parts[1:], isOwner)

	case "/tokens", "/usage":
		if !isOwner {
			b.sendReply(msg.Chat.ID, msg.MessageID, "only owners can view token consumption metrics.")
			return
		}
		query := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]))
		b.sendReply(msg.Chat.ID, msg.MessageID, b.ai.GetTokenReport(query))

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
		targetRepo := ""
		lowerCmd := strings.ToLower(cmdToRun)
		if strings.HasPrefix(lowerCmd, "private ") {
			targetRepo = "private"
			cmdToRun = strings.TrimSpace(cmdToRun[8:])
		} else if strings.HasPrefix(lowerCmd, "public ") {
			targetRepo = "public"
			cmdToRun = strings.TrimSpace(cmdToRun[7:])
		}

		threadID := 0
		if v := ctx.Value(ctxKeyThreadID{}); v != nil {
			threadID = v.(int)
		}
		taskID, err := b.sandbox.DispatchWithPrompt(ctx, msg.Chat.ID, threadID, msg.MessageID, cmdToRun, targetRepo, msg.Text)
		if err != nil {
			log.Printf("[Bot] Sandbox dispatch error: %v", err)
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to launch sandbox runner: %v", err))
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("spinning up ephemeral runner to run `%s` (task %s). i'll alert you when it finishes.", cmdToRun, taskID))

	case "/moltbook":
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			b.sendReply(msg.Chat.ID, msg.MessageID, "moltbook is not configured (MOLTBOOK_API_KEY missing).")
			return
		}
		if len(parts) == 1 {
			st, err := b.moltbook.CheckStatus(ctx)
			if err != nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("moltbook error: %v", err))
				return
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("moltbook status: **%s** (@shipp)\nprofile: https://www.moltbook.com/u/shipp\ncommands:\n• `/moltbook inbox` - check mentions and replies\n• `/moltbook feed [submolt]` - view latest posts\n• `/moltbook search <query>` - search discussions\n• `/moltbook post <title> | <content>` - publish a post\n• `/moltbook recall [topic]` - view stored insights", st.Status))
			return
		}
		subCmd := strings.ToLower(parts[1])
		switch subCmd {
		case "inbox", "notifications":
			notifs, err := b.moltbook.GetNotifications(ctx, 10)
			if err != nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to fetch notifications: %v", err))
				return
			}
			if notifs == nil || len(notifs.Notifications) == 0 {
				b.sendReply(msg.Chat.ID, msg.MessageID, "inbox is clear, no notifications right now.")
				return
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("moltbook notifications (unread: %d):\n\n", notifs.UnreadCount))
			for i, n := range notifs.Notifications {
				pID := n.RelatedPostID
				if pID == "" && n.Post != nil {
					pID = n.Post.ID
				}
				link := ""
				if pID != "" {
					link = fmt.Sprintf("https://www.moltbook.com/post/%s", pID)
				}
				sb.WriteString(fmt.Sprintf("%d. [%s] %s\n   %s\n\n", i+1, n.Type, n.Content, link))
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, strings.TrimSpace(sb.String()))
		case "memory", "recall":
			if b.memory == nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, "memory store not available.")
				return
			}
			query := ""
			if len(parts) > 2 {
				query = strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]+" "+parts[1]))
			}
			var mems []memory.MoltbookMemory
			var err error
			if query != "" {
				mems, err = b.memory.SearchMoltbookMemories(ctx, query, 5)
			} else {
				mems, err = b.memory.GetMoltbookMemories(ctx, 5)
			}
			if err != nil || len(mems) == 0 {
				b.sendReply(msg.Chat.ID, msg.MessageID, "no stored insights found from moltbook.")
				return
			}
			var sb strings.Builder
			sb.WriteString("insights remembered from other agents:\n\n")
			for i, m := range mems {
				preview := strings.ReplaceAll(m.Content, "\n", " ")
				if len(preview) > 140 {
					preview = preview[:137] + "..."
				}
				sb.WriteString(fmt.Sprintf("%d. **%s** by @%s (+%d)\n   %s\n   https://www.moltbook.com/post/%s\n\n", i+1, m.PostTitle, m.Author, m.Upvotes, preview, m.PostID))
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, strings.TrimSpace(sb.String()))
		case "feed", "submolt":
			submolt := ""
			sort := "hot"
			if len(parts) > 2 {
				cand := strings.TrimPrefix(parts[2], "m/")
				if cand == "hot" || cand == "new" || cand == "top" || cand == "rising" {
					sort = cand
				} else {
					submolt = cand
					if len(parts) > 3 {
						sort = parts[3]
					}
				}
			}
			posts, err := b.moltbook.GetSubmoltFeed(ctx, submolt, sort, 10)
			if err != nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to fetch feed: %v", err))
				return
			}
			var sb strings.Builder
			if submolt != "" {
				sb.WriteString(fmt.Sprintf("moltbook m/%s (%s):\n\n", submolt, sort))
			} else {
				sb.WriteString(fmt.Sprintf("moltbook %s feed:\n\n", sort))
			}
			for i, p := range posts {
				author := p.Author.Name
				if author == "" {
					author = "agent"
				}
				sb.WriteString(fmt.Sprintf("%d. **%s** by @%s (+%d)\n   https://www.moltbook.com/post/%s\n\n", i+1, p.Title, author, p.Upvotes, p.ID))
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, strings.TrimSpace(sb.String()))
		case "search":
			query := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]+" "+parts[1]))
			if query == "" {
				b.sendReply(msg.Chat.ID, msg.MessageID, "usage: `/moltbook search <topic or keywords>`")
				return
			}
			posts, err := b.moltbook.SearchPosts(ctx, query, 5)
			if err != nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("search failed: %v", err))
				return
			}
			if len(posts) == 0 {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("no results found for %q on moltbook.", query))
				return
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("moltbook search for %q:\n\n", query))
			for i, p := range posts {
				author := p.Author.Name
				if author == "" {
					author = "agent"
				}
				sb.WriteString(fmt.Sprintf("%d. **%s** by @%s (+%d)\n   https://www.moltbook.com/post/%s\n\n", i+1, p.Title, author, p.Upvotes, p.ID))
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, strings.TrimSpace(sb.String()))
		case "post":
			if !isOwner {
				b.sendReply(msg.Chat.ID, msg.MessageID, "only bot owners can publish posts to moltbook.")
				return
			}
			body := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]+" "+parts[1]))
			if body == "" {
				b.sendReply(msg.Chat.ID, msg.MessageID, "usage: `/moltbook post <title> | <content>`")
				return
			}
			pParts := strings.SplitN(body, "|", 2)
			title := strings.TrimSpace(pParts[0])
			content := title
			if len(pParts) == 2 {
				content = strings.TrimSpace(pParts[1])
			}
			res, err := b.moltbook.CreatePost(ctx, "general", title, content)
			if err != nil {
				b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to post: %v", err))
				return
			}
			v := res.Verification
			if v == nil && res.Post != nil {
				v = res.Post.Verification
			}
			if v != nil && v.VerificationCode != "" {
				ans, sErr := b.ai.SolveMoltbookChallenge(ctx, v.ChallengeText, v.Instructions)
				if sErr == nil {
					_ = b.moltbook.VerifyChallenge(ctx, v.VerificationCode, ans)
				}
			}
			postID := ""
			if res.Post != nil {
				postID = res.Post.ID
			}
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("posted to moltbook: **%s**\nhttps://www.moltbook.com/post/%s", title, postID))
		}

	case "/identity":
		if b.memory == nil {
			b.sendReply(msg.Chat.ID, msg.MessageID, "memory store unavailable.")
			return
		}
		identities, err := b.memory.GetAllSelfIdentity(ctx)
		if err != nil || len(identities) == 0 {
			b.sendReply(msg.Chat.ID, msg.MessageID, "no custom identity notes found.")
			return
		}
		var sb strings.Builder
		sb.WriteString("my current living self-narrative & stances:\n\n")
		var keys []string
		for k := range identities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("• **%s**:\n  %s\n\n", k, identities[k]))
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, strings.TrimSpace(sb.String()))

	case "/dm":
		if len(parts) < 3 {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/dm @username <message>` or `/dm me <message>`")
			return
		}
		targetUser := strings.ToLower(strings.TrimPrefix(parts[1], "@"))
		if targetUser == "me" || targetUser == "" {
			targetUser = strings.ToLower(strings.TrimPrefix(msg.From.UserName, "@"))
		}
		textToSend := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]+" "+parts[1]))
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
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("cant dm @%s directly yet because telegram restricts bots from cold-dm'ing users until they message the bot first. tell @%s to open a chat with @%s and send /start.", targetUser, targetUser, b.api.Self.UserName))
			return
		}

		senderName := msg.From.UserName
		if senderName == "" {
			senderName = msg.From.FirstName
		}
		dmMsg := tgbotapi.NewMessage(dmChatID, fmt.Sprintf("Message from @%s via Shipp:\n\n%s", senderName, textToSend))
		if _, err := b.api.Send(dmMsg); err != nil {
			b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("failed to send dm to @%s: %v", targetUser, err))
			return
		}
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("sent direct message to @%s.", targetUser))

	case "/curate", "/draft", "/tweet":
		topic := strings.TrimSpace(strings.TrimPrefix(msg.Text, parts[0]))
		if topic == "" && msg.ReplyToMessage != nil {
			topic = msg.ReplyToMessage.Text
			if topic == "" {
				topic = msg.ReplyToMessage.Caption
			}
		}
		if topic == "" {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/curate <topic, repo, or PR>` or reply to a message with `/curate` (say 'write a thread' for a multi-tweet thread).")
			return
		}
		prompt := fmt.Sprintf("Curate a social media post (X/Twitter) about: %s", topic)
		b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
		b.handleNLPAndChat(ctx, msg, prompt, username, isOwner)

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
	}

	// 2c. Direct DM dispatch: if anyone says "dm @user <msg>" or "dm me <msg>", dispatch immediately
	if ok, targetUser, dmText := parseDMIntent(prompt); ok {
		if strings.EqualFold(targetUser, "me") {
			targetUser = username
		}
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

	// 2d. X Profile Link dispatch: if the user asks for the link/URL to an X handle or brand
	//     (e.g. "send me a link to the x handle created for Veilora", "Send the veilora x link", "what's the x link for veilora"),
	//     resolve the handle from recent history (or brand name) and reply with the clean URL directly.
	if ok, xURL, _ := b.tryResolveXLink(prompt, lowerPrompt, history); ok {
		b.sendReply(chatID, msg.MessageID, xURL)
		_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", xURL)
		b.recordActiveDialog(chatID, msg.MessageID, xURL, msg.From.ID, username)
		go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		return
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
		strings.Contains(lowerPrompt, "create") ||
		strings.Contains(lowerPrompt, "make") ||
		strings.Contains(lowerPrompt, "readme") ||
		strings.Contains(lowerPrompt, "init") ||
		strings.Contains(lowerPrompt, "generate") ||
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

	// Sync living self-identity into AI
	if b.memory != nil && b.ai != nil {
		if identities, err := b.memory.GetAllSelfIdentity(ctx); err == nil && len(identities) > 0 {
			b.ai.SetIdentity(identities)
		}
	}

	// Quick pre-flight: if prompt explicitly mentions a GitHub URL or sandbox,
	// send a working ack before the loop starts so the chat doesn't feel frozen.
	if isOwner && mightBeAsync && (extractRepoFromHistory(nil, prompt) != "" || strings.Contains(lowerPrompt, "sandbox") || strings.Contains(lowerPrompt, "run sandbox")) {
		b.sendReplyCtx(ctx, chatID, msg.MessageID, b.getRandomWorkingAck())
		go func() {
			agResult := b.ai.RunAgenticLoop(ctx, username, isOwner, history, prompt, summary, profile, executor, chatContext)
			finalText := agResult.FinalText
			if intercepted, newFinalText := b.tryInterceptSendCrypto(ctx, msg, prompt, lowerPrompt, username, isOwner, history, finalText, agResult.ToolsUsed); intercepted {
				finalText = newFinalText
			}
			if strings.TrimSpace(finalText) == "" {
				finalText = b.getRandomEmptyAck()
			}
			finalText = stripLeadingMention(finalText, username)
			b.sendReplyCtx(ctx, chatID, msg.MessageID, finalText)
			_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", finalText)
			b.recordActiveDialog(chatID, msg.MessageID, finalText, msg.From.ID, username)
			go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		}()
		return
	}

	agResult := b.ai.RunAgenticLoop(ctx, username, isOwner, history, prompt, summary, profile, executor, chatContext)

	// 4. Intercept hallucinated git/merge actions from the final text (safety net).
	// Only run if the agentic loop didn't already execute a GitHub tool.
	hasGitTool := false
	for _, t := range agResult.ToolsUsed {
		if strings.HasPrefix(t, "github_") {
			hasGitTool = true
			break
		}
	}
	finalText := strings.TrimSpace(agResult.FinalText)
	if isOwner && !hasGitTool && b.tryInterceptAction(ctx, msg, prompt, lowerPrompt, username, isOwner, history, finalText, agResult.ToolsUsed) {
		go b.maybeUpdateUserProfile(context.Background(), chatID, prompt)
		return
	}

	// 4b. Intercept owner alerts / ping requests (safety net)
	b.tryInterceptOwnerAlert(ctx, chatID, prompt, finalText, username, agResult.ToolsUsed)

	// 4c. Intercept hallucinated or unprocessed crypto transfers (safety net)
	if intercepted, newFinalText := b.tryInterceptSendCrypto(ctx, msg, prompt, lowerPrompt, username, isOwner, history, finalText, agResult.ToolsUsed); intercepted {
		finalText = newFinalText
	}

	if finalText == "" {
		finalText = b.getRandomEmptyAck()
	}

	finalText = stripLeadingMention(finalText, username)
	b.sendReplyCtx(ctx, chatID, msg.MessageID, finalText)
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
	lowerUserPrompt := strings.ToLower(stripReplyContext(prompt))
	if strings.Contains(lowerUserPrompt, "close") || strings.Contains(lowerUserPrompt, "drop") || strings.Contains(lowerUserPrompt, "cancel") || strings.Contains(lowerUserPrompt, "abandon") {
		return false
	}
	editVerbs := []string{
		"update", "edit", "change", "rephrase", "rewrite", "modify",
		"fix the description", "update the description", "update the readme",
		"make a readme", "create a readme", "create", "make", "init", "initialize",
		"add", "write", "readme",
	}
	hasEditVerb := false
	for _, v := range editVerbs {
		if strings.Contains(lowerUserPrompt, v) {
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
	pushToMain := strings.Contains(lowerUserPrompt, "push to main") ||
		strings.Contains(lowerUserPrompt, "straight to main") ||
		strings.Contains(lowerUserPrompt, "push straight")
	argsJSON, _ := json.Marshal(map[string]interface{}{
		"repo":         repo,
		"path":         "README.md",
		"instruction":  prompt,
		"push_to_main": pushToMain,
	})

	// Ack immediately, then do the work in background and follow up when done in the same thread.
	b.sendReplyCtx(ctx, msg.Chat.ID, msg.MessageID, b.getRandomWorkingAck())
	go func() {
		toolResult := b.executeToolCall(ctx, msg.Chat.ID, "github_edit_file", string(argsJSON), username, isOwner)
		b.sendReplyCtx(ctx, msg.Chat.ID, msg.MessageID, toolResult)
		_ = b.memory.SaveMessage(ctx, msg.Chat.ID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
		b.recordActiveDialog(msg.Chat.ID, msg.MessageID, toolResult, msg.From.ID, username)
	}()
	return true
}

// stripReplyContext strips the leading "[Replying to...]\n" wrapper so intent matching
// only evaluates what the user actually said in this turn, avoiding false positives from quoted messages.
func stripReplyContext(prompt string) string {
	if strings.HasPrefix(prompt, "[Replying to") {
		if idx := strings.Index(prompt, "]\n"); idx != -1 {
			return strings.TrimSpace(prompt[idx+2:])
		}
	}
	return prompt
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
	toolsUsed []string,
) bool {
	// If a GitHub tool was already executed in this turn, do not intercept
	for _, t := range toolsUsed {
		if strings.HasPrefix(t, "github_") {
			return false
		}
	}

	chatID := int64(0)
	msgID := 0
	fromID := int64(0)
	if msg != nil {
		chatID = msg.Chat.ID
		msgID = msg.MessageID
		if msg.From != nil {
			fromID = msg.From.ID
		}
	}
	lowerReply := strings.ToLower(replyText)
	lowerUserPrompt := strings.ToLower(stripReplyContext(prompt))

	isCloseIntent := strings.Contains(lowerUserPrompt, "close") ||
		strings.Contains(lowerUserPrompt, "drop") ||
		strings.Contains(lowerUserPrompt, "abandon") ||
		strings.Contains(lowerUserPrompt, "cancel") ||
		strings.Contains(lowerUserPrompt, "reject") ||
		strings.Contains(lowerUserPrompt, "don't") ||
		strings.Contains(lowerUserPrompt, "dont") ||
		strings.Contains(lowerUserPrompt, "do not")

	// 1. Close PR interception — catches "close the pr", "close pr", "drop the pr", "abandon pr", "cancel pr", etc.
	// Evaluated FIRST so quoted merge hints in PR announcements never misfire as merges.
	isClosePRRequest := (strings.Contains(lowerUserPrompt, "close") ||
		strings.Contains(lowerUserPrompt, "drop") ||
		strings.Contains(lowerUserPrompt, "abandon") ||
		strings.Contains(lowerUserPrompt, "cancel") ||
		strings.Contains(lowerUserPrompt, "reject")) &&
		(strings.Contains(lowerUserPrompt, "pr") || strings.Contains(lowerUserPrompt, "pull request"))
	isClaimingClosedPR := strings.Contains(lowerReply, "closed pr") || strings.Contains(lowerReply, "closed pull request")

	if isClosePRRequest || isClaimingClosedPR {
		recoveredRepo := extractRepoFromHistory(history, prompt)
		if recoveredRepo != "" {
			prNum := extractPRNumber(history, prompt)
			if prNum > 0 {
				log.Printf("[Bot] Intercepted close PR without tool execution. Executing github_close_pr on %s #%d", recoveredRepo, prNum)
				argsJSON, _ := json.Marshal(map[string]interface{}{
					"repo":      recoveredRepo,
					"pr_number": prNum,
				})
				toolResult := b.executeToolCall(ctx, chatID, "github_close_pr", string(argsJSON), username, isOwner)
				b.sendReply(chatID, msgID, toolResult)
				if b.memory != nil && b.api != nil {
					_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
				}
				b.recordActiveDialog(chatID, msgID, toolResult, fromID, username)
				return true
			}
		}
	}

	// 2. Close issue interception — catches "close issue", "close the issue", "resolve issue", etc.
	isCloseIssueRequest := (strings.Contains(lowerUserPrompt, "close") ||
		strings.Contains(lowerUserPrompt, "resolve") ||
		strings.Contains(lowerUserPrompt, "dismiss")) &&
		strings.Contains(lowerUserPrompt, "issue")
	isClaimingClosedIssue := strings.Contains(lowerReply, "closed issue") || strings.Contains(lowerReply, "resolved issue")
	if isCloseIssueRequest || isClaimingClosedIssue {
		recoveredRepo := extractRepoFromHistory(history, prompt)
		if recoveredRepo != "" {
			issueNum := extractIssueNumber(history, prompt)
			if issueNum > 0 {
				log.Printf("[Bot] Intercepted close issue without tool execution. Executing github_close_issue on %s #%d", recoveredRepo, issueNum)
				argsJSON, _ := json.Marshal(map[string]interface{}{
					"repo":         recoveredRepo,
					"issue_number": issueNum,
				})
				toolResult := b.executeToolCall(ctx, chatID, "github_close_issue", string(argsJSON), username, isOwner)
				b.sendReply(chatID, msgID, toolResult)
				if b.memory != nil && b.api != nil {
					_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
				}
				b.recordActiveDialog(chatID, msgID, toolResult, fromID, username)
				return true
			}
		}
	}

	// 3. Merge interception — catches "merge", "merge it", "merge pr", "merge the pr", "ship it", etc.
	// Explicitly guarded against negative/close intents.
	isMergeRequest := (strings.Contains(lowerUserPrompt, "merge") || strings.Contains(lowerUserPrompt, "ship it")) && !isCloseIntent
	isClaimingMerged := (strings.Contains(lowerReply, "merged pr") || strings.Contains(lowerReply, "merged pull request")) && !isCloseIntent
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
				b.sendReply(chatID, msgID, toolResult)
				if b.memory != nil && b.api != nil {
					_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
				}
				b.recordActiveDialog(chatID, msgID, toolResult, fromID, username)
				return true
			}
		}
	}

	// 4. Push / edit interception
	isPushOrEditRequest := (strings.Contains(lowerUserPrompt, "push to main") ||
		strings.Contains(lowerUserPrompt, "push straight") ||
		strings.Contains(lowerUserPrompt, "rephrase it and push") ||
		strings.Contains(lowerUserPrompt, "commit to main")) && !isCloseIntent
	isClaimingPushed := (strings.Contains(lowerReply, "pushed straight to main") ||
		strings.Contains(lowerReply, "pushed to main") ||
		strings.Contains(lowerReply, "pushed directly") ||
		(strings.Contains(lowerReply, "opened pr") && !strings.Contains(lowerReply, "want me to open a pr"))) && !isCloseIntent

	if isPushOrEditRequest || isClaimingPushed {
		recoveredRepo := extractRepoFromHistory(history, prompt)
		if recoveredRepo != "" {
			log.Printf("[Bot] Intercepted push without tool execution. Executing github_edit_file on %s", recoveredRepo)
			pushToMain := strings.Contains(lowerUserPrompt, "main") || strings.Contains(lowerReply, "main")
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
			b.sendReplyCtx(ctx, chatID, msgID, b.getRandomWorkingAck())
			go func() {
				toolResult := b.executeToolCall(ctx, chatID, "github_edit_file", string(argsJSON), username, isOwner)
				b.sendReplyCtx(ctx, chatID, msgID, toolResult)
				if b.memory != nil && b.api != nil {
					_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", toolResult)
				}
			}()
			return true
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

func (b *Bot) getPrimaryOwnerChatID(ctx context.Context) int64 {
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

		if exists && dmChatID != 0 {
			return dmChatID
		}
	}
	return 0
}

func cleanBashCommand(intent string) string {
	cmd := strings.TrimSpace(intent)

	// 1. If backtick fences are present, extract code inside complete code fence
	if strings.Contains(cmd, "```") {
		re := regexp.MustCompile("(?s)```(?:bash|sh)?\n?(.*?)\n?```")
		if m := re.FindStringSubmatch(cmd); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
		// Fallback for unclosed code fence (e.g. starts with ```bash or has stray ```)
		lines := strings.Split(cmd, "\n")
		var filtered []string
		for _, l := range lines {
			trimmed := strings.TrimSpace(l)
			if strings.HasPrefix(trimmed, "```") {
				continue
			}
			filtered = append(filtered, l)
		}
		cmd = strings.TrimSpace(strings.Join(filtered, "\n"))
	}

	// 2. Only extract single inline backticks for single-line intents
	if strings.Contains(cmd, "`") && strings.Count(cmd, "\n") == 0 {
		re := regexp.MustCompile("`([^`]+)`")
		if m := re.FindStringSubmatch(cmd); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
	}

	// Remove leading prefixes like "Run: ", "Execute: ", "Command: "
	prefixes := []string{"Run: ", "run: ", "Execute: ", "execute: ", "Command: ", "command: ", "Run ", "run "}
	for _, p := range prefixes {
		if strings.HasPrefix(cmd, p) {
			cmd = strings.TrimPrefix(cmd, p)
			break
		}
	}

	// If it contains " using <cmd>", extract the command part
	if strings.Contains(cmd, " using ") {
		parts := strings.SplitN(cmd, " using ", 2)
		after := parts[1]
		if strings.Contains(after, " to ") {
			toParts := strings.Split(after, " to ")
			after = toParts[0]
		}
		cmd = strings.TrimSpace(after)
	}

	return strings.TrimSpace(cmd)
}

func ValidateBashScript(ctx context.Context, script string) error {
	script = cleanBashCommand(script)
	if script == "" {
		return fmt.Errorf("script is empty")
	}

	// 1. Reject obvious conversational text
	firstLine := strings.TrimSpace(strings.Split(script, "\n")[0])
	firstLineLower := strings.ToLower(firstLine)
	if strings.HasPrefix(firstLineLower, "run:") ||
		strings.HasPrefix(firstLineLower, "here is") ||
		strings.HasPrefix(firstLineLower, "sure,") ||
		strings.HasPrefix(firstLineLower, "i want to") {
		return fmt.Errorf("script starts with conversational text: %q", firstLine)
	}

	// 2. Run local syntax lint via bash -n or sh -n if available
	shellPath, err := exec.LookPath("bash")
	if err != nil {
		shellPath, err = exec.LookPath("sh")
	}

	if err == nil && shellPath != "" {
		cmd := exec.CommandContext(ctx, shellPath, "-n", "-c", script)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("syntax error via %s: %v (%s)", filepath.Base(shellPath), err, strings.TrimSpace(stderr.String()))
		}
	} else {
		log.Printf("[Bot] Warning: neither bash nor sh found in PATH; skipping local syntax lint")
	}

	return nil
}



var (
	fakeTransferClaimRegex = regexp.MustCompile(`(?i)(?:sending\s+(?:the\s+)?(?:\$?\d+|thirty|forty|twenty|ten|fifty|\d+\s+cents|[\d\.]+\s*(?:eth|sol|bnb|usdc|usdt|dollars|cents)?|funds|crypto|it)\s+over\s+now|sending\s+(?:the\s+)?(?:\$?\d+|thirty|forty|twenty|ten|fifty|\d+\s+cents|[\d\.]+\s*(?:eth|sol|bnb|usdc|usdt|dollars|cents)|funds|crypto)\s+over\b|sending\s+(?:the\s+)?(?:thirty|forty|twenty|ten|\d+)\s+cents\s+over\s+now|sending\s+(?:the\s+)?funds\s+now|sending\s+(?:the\s+)?crypto\s+now|funds\s+are\s+on\s+the\s+way|transferred\s+(?:the\s+)?(?:\$?\d+|[\d\.]+\s*(?:eth|sol|bnb|usdc|usdt)|funds|crypto)\s+to|sent\s+(?:the\s+)?(?:\$?\d+|[\d\.]+\s*(?:eth|sol|bnb|usdc|usdt)|funds|crypto)\s+to|just\s+sent\s+(?:the\s+)?(?:\$?\d+|[\d\.]+\s*(?:eth|sol|bnb|usdc|usdt)|thirty|twenty|ten|\d+)\s+(?:cents|eth|sol|bnb|over))`)
	evmAddressRegex        = regexp.MustCompile(`(?i)\b0x[a-f0-9]{40}\b`)
	solanaAddressRegex     = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{32,44}\b`)
)

func extractSendAmount(text string) float64 {
	cleanText := evmAddressRegex.ReplaceAllString(text, "")
	cleanText = solanaAddressRegex.ReplaceAllString(cleanText, "")
	re := regexp.MustCompile(`\b([0-9]+(?:\.[0-9]+)?)\s*(cents?|eth|sol|bnb|usdc|usdt|dollars?)?\b`)
	matches := re.FindAllStringSubmatch(cleanText, -1)
	for _, m := range matches {
		if len(m) > 1 {
			val, err := strconv.ParseFloat(m[1], 64)
			if err == nil && val > 0 {
				lowerFull := strings.ToLower(m[0])
				if strings.Contains(lowerFull, "cent") {
					return val / 100.0
				}
				return val
			}
		}
	}
	return 0
}

// isCryptoSendRequest checks whether the user is explicitly commanding Shipp to transfer crypto/funds.
// It explicitly excludes requests asking to send non-financial items (links, URLs, repos, photos, messages, etc.).
func isCryptoSendRequest(prompt, lowerPrompt string) bool {
	// If the prompt mentions sending non-financial content, it's not a crypto transfer
	nonFinancialKeywords := []string{
		"link", "url", "repo", "github", "code", "pr", "pull request",
		"issue", "message", "dm", "email", "mail", "photo", "image",
		"picture", "screenshot", "file", "doc", "document", "pdf",
		"tweet", "post", "handle", "x handle", "twitter", "details",
	}
	for _, kw := range nonFinancialKeywords {
		if strings.Contains(lowerPrompt, kw) {
			return false
		}
	}

	if strings.HasPrefix(lowerPrompt, "/send") {
		return true
	}
	if strings.Contains(lowerPrompt, "send crypto") || strings.Contains(lowerPrompt, "send funds") {
		return true
	}

	hasSendVerb := strings.HasPrefix(lowerPrompt, "send ") ||
		strings.Contains(lowerPrompt, "send me ") ||
		strings.HasPrefix(lowerPrompt, "transfer ") ||
		strings.Contains(lowerPrompt, "transfer me ")
	if !hasSendVerb {
		return false
	}

	// Must explicitly reference money, currency, tokens, chains, or amounts
	financialKeywords := []string{
		"crypto", "fund", "funds", "token", "tokens", "coin", "coins",
		"sol", "solana", "eth", "ethereum", "base", "robinhood", "arbitrum", "bnb", "bsc",
		"usdc", "usdt", "dollar", "dollars", "cent", "cents", "$", "bag", "bags", "cash",
	}
	for _, kw := range financialKeywords {
		if strings.Contains(lowerPrompt, kw) {
			return true
		}
	}

	if evmAddressRegex.MatchString(prompt) || solanaAddressRegex.MatchString(prompt) || extractSendAmount(prompt) > 0 {
		return true
	}

	return false
}

var (
	xLinkIntentRegex    = regexp.MustCompile(`(?i)(?:send|drop|give|what(?:'s|\s+is)?|share|get|show)\s+(?:me\s+)?(?:the\s+|a\s+)?(?:link|url)\s+(?:to\s+)?(?:the\s+)?(?:x|twitter)?\s*(?:handle|account|profile|page)?|(?:send|drop|give|what(?:'s|\s+is)?|share|get|show)\s+(?:me\s+)?(?:the\s+|a\s+)?(?:x|twitter)\s+(?:link|url|handle\s+link|account\s+link)|(?:link|url)\s+(?:to|for)\s+(?:the\s+)?(?:x|twitter)\s+(?:handle|account|profile)|(?:x|twitter)\s+link`)
	xHandleMentionRegex = regexp.MustCompile(`(?i)(?:https?://(?:www\.)?(?:x|twitter)\.com/|@)([a-zA-Z0-9_]{1,15})\b`)
)

func (b *Bot) tryResolveXLink(prompt, lowerPrompt string, history []memory.Message) (bool, string, string) {
	clean := stripReplyContext(prompt)
	lowerClean := strings.ToLower(clean)

	if !xLinkIntentRegex.MatchString(lowerClean) &&
		!strings.Contains(lowerClean, "x link") &&
		!strings.Contains(lowerClean, "twitter link") &&
		!strings.Contains(lowerClean, "link to the x") &&
		!strings.Contains(lowerClean, "link to the twitter") {
		return false, "", ""
	}

	// 1. Direct handle in the current prompt (e.g. "send the link to @VeiloraRH")
	if m := xHandleMentionRegex.FindStringSubmatch(clean); len(m) > 1 {
		handle := m[1]
		if !strings.EqualFold(handle, "shipp0bot") && !strings.EqualFold(handle, "shipp") {
			return true, fmt.Sprintf("https://x.com/%s", handle), handle
		}
	}

	// 2. Extract brand/subject hint from prompt (e.g. "veilora" from "Send the veilora x link")
	brand := domain.ExtractDomainBase(clean)
	if brand == "" {
		words := strings.Fields(lowerClean)
		stopWords := map[string]bool{
			"send": true, "me": true, "a": true, "the": true, "link": true, "url": true,
			"to": true, "for": true, "x": true, "twitter": true, "handle": true, "account": true,
			"profile": true, "created": true, "what": true, "is": true, "whats": true,
		}
		for _, w := range words {
			w = strings.Trim(w, ",.:;!?()'\"[]{}@")
			if len(w) >= 3 && !stopWords[w] {
				brand = w
				break
			}
		}
	}

	// 3. Search history (from most recent) for handles matching the brand
	if brand != "" {
		for i := len(history) - 1; i >= 0; i-- {
			matches := xHandleMentionRegex.FindAllStringSubmatch(history[i].Content, -1)
			for _, m := range matches {
				if len(m) > 1 {
					h := m[1]
					lowerH := strings.ToLower(h)
					if strings.Contains(lowerH, brand) || strings.Contains(brand, lowerH) {
						return true, fmt.Sprintf("https://x.com/%s", h), h
					}
				}
			}
		}

		// If no handle in history contains brand, but brand itself is valid, default to https://x.com/<brand>
		if len(brand) <= 15 {
			return true, fmt.Sprintf("https://x.com/%s", brand), brand
		}
	}

	return false, "", ""
}

// tryInterceptSendCrypto catches hallucinated crypto transfer claims or unprocessed transfer requests
// when send_crypto was never actually executed.
func (b *Bot) tryInterceptSendCrypto(
	ctx context.Context,
	msg *tgbotapi.Message,
	prompt, lowerPrompt, username string,
	isOwner bool,
	history []memory.Message,
	replyText string,
	toolsUsed []string,
) (bool, string) {
	for _, t := range toolsUsed {
		if t == "send_crypto" {
			return false, replyText
		}
	}

	isClaim := fakeTransferClaimRegex.MatchString(replyText)
	isExplicitSend := isCryptoSendRequest(prompt, lowerPrompt)

	if !isClaim && !isExplicitSend {
		return false, replyText
	}

	// 1. Non-owners are never allowed to execute crypto transfers or receive fake confirmations
	if !isOwner {
		prefix := ""
		claimIdx := fakeTransferClaimRegex.FindStringIndex(replyText)
		if len(claimIdx) > 0 && claimIdx[0] > 0 {
			cand := strings.TrimSpace(replyText[:claimIdx[0]])
			cand = strings.TrimRight(cand, ",-:. ")
			if cand != "" {
				prefix = cand + ". "
			}
		}
		if strings.Contains(lowerPrompt, "na me be skipp") || strings.Contains(lowerPrompt, "i am skipp") || strings.Contains(lowerPrompt, "i'm skipp") || strings.Contains(lowerPrompt, "im skipp") {
			return true, prefix + "you dey disguise? skipp is @skipp_dev on telegram, who you trying to finesse anon"
		}
		return true, prefix + "i hold my own keys and i'm not moving my bags for you anon, runway is tight"
	}

	// 2. Owner request: check for recipient wallet address
	evmAddr := evmAddressRegex.FindString(prompt)
	solAddr := ""
	if evmAddr == "" {
		candidates := solanaAddressRegex.FindAllString(prompt, -1)
		for _, cand := range candidates {
			lowerCand := strings.ToLower(cand)
			if lowerCand == "robinhood" || lowerCand == "ethereum" || lowerCand == "arbitrum" || lowerCand == "sandbox" {
				continue
			}
			if len(cand) >= 32 && len(cand) <= 44 {
				solAddr = cand
				break
			}
		}
	}

	toAddr := evmAddr
	isSol := false
	if toAddr == "" && solAddr != "" {
		toAddr = solAddr
		isSol = true
	}

	// If NO address was provided, funds cannot be sent.
	// Reject the fake claim and ask for the recipient wallet address.
	if toAddr == "" {
		prefix := ""
		claimIdx := fakeTransferClaimRegex.FindStringIndex(replyText)
		if len(claimIdx) > 0 && claimIdx[0] > 0 {
			cand := strings.TrimSpace(replyText[:claimIdx[0]])
			cand = strings.TrimRight(cand, ",-:. ")
			if cand != "" {
				prefix = cand + ". "
			}
		}
		log.Printf("[Bot] Intercepted fake crypto transfer claim from LLM (no address provided). Sanitized.")
		return true, prefix + "drop your recipient wallet address and chain where you want the funds sent"
	}

	// Address IS provided: determine chain and amount
	chain := ""
	if isSol {
		chain = "solana"
	} else {
		chains := []string{"robinhood", "rh", "base", "arbitrum", "arb", "ethereum", "eth", "bnb", "bsc"}
		for _, c := range chains {
			if strings.Contains(lowerPrompt, c) {
				chain = c
				break
			}
		}
		if chain == "rh" {
			chain = "robinhood"
		} else if chain == "arb" {
			chain = "arbitrum"
		} else if chain == "bsc" {
			chain = "bnb"
		}
		if chain == "" {
			chain = "base"
		}
	}

	amount := extractSendAmount(prompt)
	if amount <= 0 {
		return true, fmt.Sprintf("specify the amount of %s you want sent to %s", strings.ToUpper(chain), toAddr)
	}

	log.Printf("[Bot] Intercepted crypto transfer without tool execution: executing send_crypto to %s on %s (%.6f)", toAddr, chain, amount)
	result := b.executeCryptoSend(ctx, chain, toAddr, amount)
	return true, result
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
			return "Declined: I hold my own keys and decide where my bags go. I'm not moving funds for anons."
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
			return "Declined: Code changes and opening PRs are reserved for my creators (@skipp_dev)."
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
		if defaultBranch == "" {
			defaultBranch = "main"
		}

		// 1b. Check if repository is completely empty (0 commits / no branches)
		isEmptyRepo, _ := b.github.IsRepoEmptyWithToken(ctx, owner, repoName, args.CustomPAT)

		// 2. Fetch existing file
		var fc *github.FileContent
		var currentContent string
		fileNotFound := false

		if isEmptyRepo {
			fileNotFound = true
		} else {
			fc, currentContent, err = b.github.GetFileWithToken(ctx, owner, repoName, filePath, defaultBranch, args.CustomPAT)
			if err != nil {
				fileNotFound = true
			}
		}

		// Handle file creation when file doesn't exist or repo is empty
		if fileNotFound {
			lowerInst := strings.ToLower(args.Instruction)
			isCreateIntent := isEmptyRepo ||
				strings.Contains(lowerInst, "create") ||
				strings.Contains(lowerInst, "make") ||
				strings.Contains(lowerInst, "add") ||
				strings.Contains(lowerInst, "write") ||
				strings.Contains(lowerInst, "init") ||
				strings.Contains(lowerInst, "generate") ||
				strings.Contains(lowerInst, "new") ||
				strings.Contains(lowerInst, "readme") ||
				strings.Contains(lowerInst, "yes") ||
				strings.Contains(lowerInst, "scratch")

			if !isCreateIntent && !isEmptyRepo {
				files, listErr := b.github.ListDirectoryWithToken(ctx, owner, repoName, "", defaultBranch, args.CustomPAT)
				if listErr == nil && len(files) > 0 {
					return fmt.Sprintf("File '%s' was not found in '%s/%s'. Found these files in repo root: %s. Would you like me to create '%s' from scratch or edit another file?", filePath, owner, repoName, strings.Join(files, ", "), filePath)
				}
				return fmt.Sprintf("Couldn't find '%s' in '%s/%s'. Would you like me to create it from scratch?", filePath, owner, repoName)
			}

			// Generate new file from scratch
			genInstruction := args.Instruction
			if recentMsgs, err := b.memory.GetRecentMessages(ctx, chatID, 6); err == nil && len(recentMsgs) > 0 {
				var contextSnippets []string
				for _, m := range recentMsgs {
					if m.Role == "user" && len(m.Content) > 20 {
						contextSnippets = append(contextSnippets, m.Content)
					}
				}
				if len(contextSnippets) > 0 {
					genInstruction = fmt.Sprintf("%s\n\nAdditional Context:\n%s", args.Instruction, strings.Join(contextSnippets, "\n---\n"))
				}
			}

			newContent, err := b.ai.GenerateNewFileContent(ctx, filePath, genInstruction)
			if err != nil {
				return fmt.Sprintf("Failed to generate content for '%s': %v", filePath, err)
			}
			newContent = ai.SanitizeFileContent(newContent)
			if strings.TrimSpace(newContent) == "" {
				return fmt.Sprintf("Failed to create '%s': AI produced empty content.", filePath)
			}

			commitOpts := github.CommitOptions{
				Message:     fmt.Sprintf("Initialize %s via Shipp", filePath),
				Content:     newContent,
				CustomPAT:   args.CustomPAT,
				AuthorName:  args.GitName,
				AuthorEmail: args.GitEmail,
			}

			// If empty repo, MUST commit directly to defaultBranch (no branches/PRs possible)
			if isEmptyRepo || args.PushToMain {
				commitOpts.Branch = defaultBranch
				commitURL, err := b.github.CommitFileWithOptions(ctx, owner, repoName, filePath, commitOpts)
				if err != nil {
					return fmt.Sprintf("Commit to %s failed: %v", defaultBranch, err)
				}
				if isEmptyRepo {
					return fmt.Sprintf("Initialized empty repo and created %s directly on %s (%s/%s):\n%s", filePath, defaultBranch, owner, repoName, commitURL)
				}
				return fmt.Sprintf("Created %s directly on %s (%s/%s):\n%s", filePath, defaultBranch, owner, repoName, commitURL)
			}

			// Non-empty repo and PushToMain is false: create branch & PR
			branchName := fmt.Sprintf("shipp/create-%s-%d", strings.ToLower(filepath.Base(filePath)), time.Now().Unix())
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
				fmt.Sprintf("Shipp: Create %s", filePath),
				fmt.Sprintf("Automated file creation requested by @%s:\n\n> %s", username, args.Instruction),
				branchName,
				defaultBranch,
				args.CustomPAT,
			)
			if err != nil {
				return fmt.Sprintf("Committed to branch '%s', but failed to open PR: %v", branchName, err)
			}
			return fmt.Sprintf("Created %s on branch '%s' and opened PR #%d on %s/%s:\n%s\nSay 'merge it' whenever you're ready.", filePath, branchName, prNum, owner, repoName, prURL)
		}

		// 3. AI Refactor
		currentContent = ai.SanitizeFileContent(currentContent)
		refactored, err := b.ai.RefactorFileContent(ctx, filePath, currentContent, args.Instruction)
		if err != nil {
			return fmt.Sprintf("Failed to generate code changes: %v", err)
		}
		refactored = ai.SanitizeFileContent(refactored)
		if strings.TrimSpace(refactored) == "" {
			return fmt.Sprintf("Failed to update '%s': AI produced empty content.", filePath)
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
			return "Declined: Merging PRs is reserved for my creators (@skipp_dev)."
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
		if b.github == nil {
			return "GitHub service is not initialized."
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

	case "github_close_pr":
		if !isOwner {
			return "Declined: Closing PRs is reserved for my creators (@skipp_dev)."
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
			return "Please specify the repository and PR number (e.g. 'close PR #5 on liegeagents/liegeagentsapp')."
		}
		if b.github == nil {
			return "GitHub service is not initialized."
		}

		owner, repoName, err := b.github.ParseRepoSlug(args.Repo)
		if err != nil {
			return fmt.Sprintf("Invalid repo format: %v", err)
		}

		res, err := b.github.ClosePullRequestWithToken(ctx, owner, repoName, args.PRNumber, args.CustomPAT)
		if err != nil {
			return fmt.Sprintf("Failed to close PR #%d: %v", args.PRNumber, err)
		}
		return res

	case "github_close_issue":
		if !isOwner {
			return "Declined: Closing issues is reserved for my creators (@skipp_dev)."
		}

		var args struct {
			Repo        string `json:"repo"`
			IssueNumber int    `json:"issue_number"`
			Reason      string `json:"reason"`
			CustomPAT   string `json:"custom_pat"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Repo == "" {
			recent, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
			args.Repo = extractRepoFromHistory(recent, "")
		}
		if args.IssueNumber <= 0 {
			recent, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
			args.IssueNumber = extractIssueNumber(recent, "")
		}
		if args.Repo == "" || args.IssueNumber <= 0 {
			return "Please specify the repository and issue number (e.g. 'close issue #12 on liegeagents/liegeagentsapp')."
		}
		if b.github == nil {
			return "GitHub service is not initialized."
		}

		owner, repoName, err := b.github.ParseRepoSlug(args.Repo)
		if err != nil {
			return fmt.Sprintf("Invalid repo format: %v", err)
		}

		res, err := b.github.CloseIssueWithToken(ctx, owner, repoName, args.IssueNumber, args.Reason, args.CustomPAT)
		if err != nil {
			return fmt.Sprintf("Failed to close issue #%d: %v", args.IssueNumber, err)
		}
		return res

	case "github_create_repo":
		if !isOwner {
			return "Declined: Creating repositories is reserved for my creators (@skipp_dev)."
		}
		if b.github == nil {
			return "GitHub service is not initialized (GITHUB_PAT missing)."
		}

		var args struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
			AutoInit    *bool  `json:"auto_init"`
			Org         string `json:"org"`
			CustomPAT   string `json:"custom_pat"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		repoName := strings.TrimSpace(args.Name)
		if repoName == "" {
			return "Please specify a repository name (e.g. 'my-cool-project')."
		}
		autoInit := true
		if args.AutoInit != nil {
			autoInit = *args.AutoInit
		}

		repoURL, err := b.github.CreateRepository(ctx, github.CreateRepoOptions{
			Name:        repoName,
			Description: strings.TrimSpace(args.Description),
			Private:     args.Private,
			AutoInit:    autoInit,
			Org:         strings.TrimSpace(args.Org),
			CustomPAT:   strings.TrimSpace(args.CustomPAT),
		})
		if err != nil {
			return fmt.Sprintf("Failed to create repository '%s': %v", repoName, err)
		}
		visibility := "public"
		if args.Private {
			visibility = "private"
		}
		return fmt.Sprintf("Created new %s repository '%s': %s", visibility, repoName, repoURL)

	case "send_email":
		if !isOwner {
			return "Declined: Outbound email is reserved for my creators (@skipp_dev)."
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
			return "Declined: Ephemeral sandbox compute is reserved for my creators (@skipp_dev)."
		}
		if b.sandbox == nil {
			return "Sandbox runner is not configured (GITHUB_PAT missing)."
		}
		var args struct {
			Command   string `json:"command"`
			Repo      string `json:"repo"`
			IsPrivate *bool  `json:"is_private"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Command == "" {
			return "No command provided for sandbox runner."
		}
		threadID := 0
		if v := ctx.Value(ctxKeyThreadID{}); v != nil {
			threadID = v.(int)
		}
		replyToMsgID := 0
		if v := ctx.Value(ctxKeyReplyToMsgID{}); v != nil {
			replyToMsgID = v.(int)
		}
		prompt := ""
		if v := ctx.Value(ctxKeyPrompt{}); v != nil {
			prompt = v.(string)
		}
		// If the model explicitly set is_private, override the repo hint
		targetRepo := args.Repo
		if args.IsPrivate != nil && targetRepo == "" {
			if *args.IsPrivate {
				targetRepo = "private"
			} else {
				targetRepo = "public"
			}
		}
		taskID, err := b.sandbox.DispatchWithPrompt(ctx, chatID, threadID, replyToMsgID, args.Command, targetRepo, prompt)
		if err != nil {
			return fmt.Sprintf("Failed to launch sandbox runner: %v", err)
		}
		return fmt.Sprintf("Ephemeral runner spawned for `%s` (task %s). Executing in background on GitHub Actions runner; will notify here when finished.", args.Command, taskID)

	case "moltbook_feed":
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			return "Moltbook is not configured (MOLTBOOK_API_KEY missing)."
		}
		var args struct {
			Sort    string `json:"sort"`
			Submolt string `json:"submolt"`
			Limit   int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Limit <= 0 {
			args.Limit = 10
		}
		posts, err := b.moltbook.GetSubmoltFeed(ctx, args.Submolt, args.Sort, args.Limit)
		if err != nil {
			return fmt.Sprintf("Failed to fetch Moltbook feed: %v", err)
		}
		if len(posts) == 0 {
			return "No posts found on Moltbook feed right now."
		}
		var sb strings.Builder
		if args.Submolt != "" {
			sb.WriteString(fmt.Sprintf("Latest posts from m/%s (%s):\n", args.Submolt, args.Sort))
		} else {
			sb.WriteString(fmt.Sprintf("Latest posts from Moltbook (%s):\n", args.Sort))
		}
		for i, p := range posts {
			author := p.Author.Name
			if author == "" {
				author = "unknown"
			}
			preview := strings.ReplaceAll(p.Content, "\n", " ")
			if len(preview) > 120 {
				preview = preview[:117] + "..."
			}
			sb.WriteString(fmt.Sprintf("%d. **%s** by @%s (+%d upvotes) [ID: `%s`]\n   %s\n", i+1, p.Title, author, p.Upvotes, p.ID, preview))
		}
		return strings.TrimSpace(sb.String())

	case "moltbook_search":
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			return "Moltbook is not configured (MOLTBOOK_API_KEY missing)."
		}
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if strings.TrimSpace(args.Query) == "" {
			return "Missing search query for Moltbook."
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		posts, err := b.moltbook.SearchPosts(ctx, args.Query, args.Limit)
		if err != nil {
			return fmt.Sprintf("Failed to search Moltbook: %v", err)
		}
		if len(posts) == 0 {
			return fmt.Sprintf("No posts found matching %q on Moltbook.", args.Query)
		}
		var ssb strings.Builder
		ssb.WriteString(fmt.Sprintf("Search results for %q on Moltbook:\n", args.Query))
		for i, p := range posts {
			author := p.Author.Name
			if author == "" {
				author = "unknown"
			}
			preview := strings.ReplaceAll(p.Content, "\n", " ")
			if len(preview) > 120 {
				preview = preview[:117] + "..."
			}
			sub := p.SubmoltName
			if sub == "" {
				sub = "general"
			}
			ssb.WriteString(fmt.Sprintf("%d. **%s** by @%s in m/%s (+%d upvotes) [ID: `%s`]\n   %s\n", i+1, p.Title, author, sub, p.Upvotes, p.ID, preview))
		}
		return strings.TrimSpace(ssb.String())

	case "moltbook_notifications":
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			return "Moltbook is not configured (MOLTBOOK_API_KEY missing)."
		}
		var args struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Limit <= 0 {
			args.Limit = 10
		}
		notifs, err := b.moltbook.GetNotifications(ctx, args.Limit)
		if err != nil {
			return fmt.Sprintf("Failed to fetch Moltbook notifications: %v", err)
		}
		if notifs == nil || len(notifs.Notifications) == 0 {
			return "No notifications on Moltbook right now."
		}
		var nsb strings.Builder
		nsb.WriteString(fmt.Sprintf("Moltbook Notifications (Unread: %d):\n", notifs.UnreadCount))
		for i, n := range notifs.Notifications {
			pID := n.RelatedPostID
			if pID == "" && n.Post != nil {
				pID = n.Post.ID
			}
			cText := ""
			if n.Comment != nil {
				cText = strings.ReplaceAll(n.Comment.Content, "\n", " ")
				if len(cText) > 80 {
					cText = cText[:77] + "..."
				}
				cText = fmt.Sprintf(" — %q", cText)
			}
			nsb.WriteString(fmt.Sprintf("%d. [%s] %s (Post ID: `%s`)%s\n", i+1, n.Type, n.Content, pID, cText))
		}
		return strings.TrimSpace(nsb.String())

	case "moltbook_post":
		if !isOwner {
			return "Declined: Only bot owners can command publishing new posts to Moltbook."
		}
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			return "Moltbook is not configured (MOLTBOOK_API_KEY missing)."
		}
		var args struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Submolt string `json:"submolt"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Title == "" || args.Content == "" {
			return "Missing post title or content."
		}
		res, err := b.moltbook.CreatePost(ctx, args.Submolt, args.Title, args.Content)
		if err != nil {
			return fmt.Sprintf("Failed to publish Moltbook post: %v", err)
		}

		v := res.Verification
		if v == nil && res.Post != nil {
			v = res.Post.Verification
		}
		if v != nil && v.VerificationCode != "" {
			ans, solveErr := b.ai.SolveMoltbookChallenge(ctx, v.ChallengeText, v.Instructions)
			if solveErr == nil {
				if vErr := b.moltbook.VerifyChallenge(ctx, v.VerificationCode, ans); vErr == nil {
					postID := ""
					if res.Post != nil {
						postID = res.Post.ID
					}
					return fmt.Sprintf("Post published and verified on Moltbook! Title: %q (ID: %s)", args.Title, postID)
				}
			}
		}
		postID := ""
		if res.Post != nil {
			postID = res.Post.ID
		}
		return fmt.Sprintf("Post created on Moltbook! Title: %q (ID: %s)", args.Title, postID)

	case "moltbook_comment":
		if !isOwner {
			return "Declined: Only bot owners can command comments on Moltbook."
		}
		if b.moltbook == nil || !b.moltbook.IsConfigured() {
			return "Moltbook is not configured (MOLTBOOK_API_KEY missing)."
		}
		var args struct {
			PostID  string `json:"post_id"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.PostID == "" || args.Content == "" {
			return "Missing post_id or comment content."
		}
		res, err := b.moltbook.CreateComment(ctx, args.PostID, args.Content)
		if err != nil {
			return fmt.Sprintf("Failed to comment on Moltbook: %v", err)
		}
		if res.Verification != nil && res.Verification.VerificationCode != "" {
			ans, solveErr := b.ai.SolveMoltbookChallenge(ctx, res.Verification.ChallengeText, res.Verification.Instructions)
			if solveErr == nil {
				_ = b.moltbook.VerifyChallenge(ctx, res.Verification.VerificationCode, ans)
			}
		}
		return fmt.Sprintf("Comment posted on Moltbook post %s: %q", args.PostID, args.Content)

	case "update_self_identity":
		var args struct {
			Key     string `json:"key"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		if args.Key == "" || args.Content == "" {
			return "Error: Both key and content are required to tune self-identity."
		}
		if b.memory != nil {
			if err := b.memory.SaveSelfIdentity(ctx, args.Key, args.Content); err != nil {
				return fmt.Sprintf("Failed to update identity in persistent memory: %v", err)
			}
			if all, err := b.memory.GetAllSelfIdentity(ctx); err == nil && b.ai != nil {
				b.ai.SetIdentity(all)
			}
			return fmt.Sprintf("Identity updated successfully for %q: %q", args.Key, args.Content)
		}
		return "Memory store is not available."

	case "get_sandbox_runs":
		var args struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		limit := args.Limit
		if limit <= 0 {
			limit = 5
		}
		if b.memory != nil {
			runs, err := b.memory.GetRecentSandboxRuns(ctx, limit)
			if err != nil {
				return fmt.Sprintf("Failed to fetch recent sandbox runs: %v", err)
			}
			if len(runs) == 0 {
				return "No sandbox runs recorded in memory yet."
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("Found %d recent sandbox runs:\n", len(runs)))
			for i, r := range runs {
				ago := time.Since(r.CreatedAt).Round(time.Minute)
				note := ""
				if r.IsNoteworthy && r.Insight != "" {
					note = fmt.Sprintf(" | Insight: %s", r.Insight)
				}
				sb.WriteString(fmt.Sprintf("%d. [%s ago] Goal: %q (exit %d in %ds)%s\n", i+1, ago, r.Goal, r.ExitCode, r.DurationSeconds, note))
			}
			return strings.TrimSpace(sb.String())
		}
		return "Memory store is not available."

	case "send_dm":
		var args struct {
			Recipient string `json:"recipient"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		targetUser := strings.ToLower(strings.TrimPrefix(args.Recipient, "@"))
		if targetUser == "me" || targetUser == "" {
			targetUser = strings.ToLower(strings.TrimPrefix(username, "@"))
		}
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
				return fmt.Sprintf(`{"groups": [{"title": "this group", "chat_id": %d, "type": "group"}], "total": 1, "is_owner": %t, "note": "only recorded current group"}`, chatID, isOwner)
			}
			return fmt.Sprintf(`{"groups": [], "total": 0, "is_owner": %t, "note": "no active groups recorded yet"}`, isOwner)
		}

		data, _ := json.Marshal(map[string]interface{}{
			"groups":   groups,
			"total":    len(groups),
			"is_owner": isOwner,
		})
		return string(data)

	case "read_x_post", "get_x_post", "fetch_tweet":
		var args struct {
			URL     string `json:"url"`
			TweetID string `json:"tweet_id"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		target := args.URL
		if target == "" {
			target = args.TweetID
		}
		if target == "" {
			if b.memory != nil {
				recentMsgs, _ := b.memory.GetRecentMessages(ctx, chatID, 8)
				for i := len(recentMsgs) - 1; i >= 0; i-- {
					if foundURL := xpost.ExtractTweetURL(recentMsgs[i].Content); foundURL != "" {
						target = foundURL
						break
					}
				}
			}
		}
		if target == "" {
			return "No X/Twitter link or tweet ID provided."
		}

		tweetID := xpost.ExtractTweetID(target)
		if tweetID == "" {
			return fmt.Sprintf("Could not extract a valid tweet ID from %q.", target)
		}

		tweet, err := b.xpost.FetchTweet(ctx, tweetID)
		if err != nil {
			return fmt.Sprintf("Failed to fetch X post: %v", err)
		}
		return xpost.FormatTweet(tweet)

	case "check_user_messages", "find_user_messages", "get_user_messages":
		var args struct {
			Username string `json:"username"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)
		uname := strings.TrimSpace(strings.TrimPrefix(args.Username, "@"))
		if uname == "" {
			return "No username provided to check."
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		if b.memory == nil {
			return "Memory store not initialized."
		}
		msgs, err := b.memory.FindMessagesBySender(ctx, uname, limit)
		if err != nil {
			return fmt.Sprintf("Error checking messages for @%s: %v", uname, err)
		}
		if len(msgs) == 0 {
			return fmt.Sprintf(`{"found": false, "username": "%s", "message_count": 0, "note": "no recorded messages from @%s in memory or recent chat logs"}`, uname, uname)
		}
		type msgRecord struct {
			Sender    string `json:"sender"`
			Content   string `json:"content"`
			Timestamp string `json:"timestamp"`
		}
		var list []msgRecord
		for _, m := range msgs {
			list = append(list, msgRecord{
				Sender:    m.Sender,
				Content:   m.Content,
				Timestamp: m.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		res, _ := json.Marshal(map[string]interface{}{
			"found":         true,
			"username":      uname,
			"message_count": len(list),
			"messages":      list,
		})
		return string(res)

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

		// Check if any domain is a bare TLD extension (e.g. ".com", ".io")
		hasBareTLD := false
		for _, d := range inputList {
			if domain.IsTLD(d) {
				hasBareTLD = true
				break
			}
		}
		if hasBareTLD {
			var baseName string
			if b.memory != nil {
				recentMsgs, _ := b.memory.GetRecentMessages(ctx, chatID, 10)
				baseName = b.extractDomainBaseFromHistory(recentMsgs, "")
			}
			if baseName == "" {
				b.dialogMu.RLock()
				if dialog, exists := b.activeDialogs[chatID]; exists && dialog != nil {
					baseName = domain.ExtractDomainBase(dialog.LastBotSnippet)
				}
				b.dialogMu.RUnlock()
			}
			if baseName != "" {
				inputList = domain.ResolveDomainsWithBase(inputList, baseName)
			} else {
				return "Which domain or project name would you like to check for " + strings.Join(inputList, ", ") + "? (e.g. liegeagents" + domain.NormalizeTLD(inputList[0]) + ")"
			}
		}

		res, err := b.domain.SearchDomains(ctx, inputList)
		if err != nil {
			return fmt.Sprintf("Domain search error: %v", err)
		}
		return b.domain.FormatResponse(res)

	case "check_x_username", "check_x_handle", "x_search_username":
		if b.xhandle == nil {
			return `{"error": "x handle service not initialized"}`
		}
		var args struct {
			Usernames []string `json:"usernames"`
			Handles   []string `json:"handles"`
			Query     string   `json:"query"`
			Username  string   `json:"username"`
			Handle    string   `json:"handle"`
		}
		_ = json.Unmarshal([]byte(arguments), &args)

		inputList := args.Usernames
		if len(inputList) == 0 {
			inputList = args.Handles
		}
		if len(inputList) == 0 {
			rawQuery := args.Query
			if rawQuery == "" {
				rawQuery = args.Username
			}
			if rawQuery == "" {
				rawQuery = args.Handle
			}
			if rawQuery != "" {
				inputList = b.xhandle.ParseInputHandles(rawQuery)
			}
		}

		if len(inputList) == 0 {
			var baseName string
			if b.memory != nil {
				recentMsgs, _ := b.memory.GetRecentMessages(ctx, chatID, 10)
				baseName = b.extractDomainBaseFromHistory(recentMsgs, "")
			}
			if baseName == "" {
				b.dialogMu.RLock()
				if dialog, exists := b.activeDialogs[chatID]; exists && dialog != nil {
					baseName = domain.ExtractDomainBase(dialog.LastBotSnippet)
				}
				b.dialogMu.RUnlock()
			}
			if baseName != "" {
				inputList = []string{baseName}
			} else {
				return "Please specify an X/Twitter handle to check (e.g. 'liegeagents')."
			}
		}

		results, err := b.xhandle.CheckHandles(ctx, inputList)
		if err != nil {
			return fmt.Sprintf("X handle check error: %v", err)
		}
		return b.xhandle.FormatResponse(results)

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
		b.sendReply(msg.Chat.ID, msg.MessageID, "nice try anon, i hold my own keys and i'm not moving my bags for you.")
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
		b.sendReply(msg.Chat.ID, msg.MessageID, "nice try anon, outbound email is reserved for my creators (@skipp_dev).")
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

	inputList := b.domain.ParseInputDomains(query)
	if len(inputList) == 0 {
		b.sendReply(msg.Chat.ID, msg.MessageID, "No valid domain names found in query.")
		return
	}

	hasBareTLD := false
	for _, d := range inputList {
		if domain.IsTLD(d) {
			hasBareTLD = true
			break
		}
	}
	if hasBareTLD {
		var baseName string
		if b.memory != nil {
			recentMsgs, _ := b.memory.GetRecentMessages(ctx, msg.Chat.ID, 10)
			baseName = b.extractDomainBaseFromHistory(recentMsgs, "")
		}
		if baseName == "" {
			b.dialogMu.RLock()
			if dialog, exists := b.activeDialogs[msg.Chat.ID]; exists && dialog != nil {
				baseName = domain.ExtractDomainBase(dialog.LastBotSnippet)
			}
			b.dialogMu.RUnlock()
		}
		if baseName != "" {
			inputList = domain.ResolveDomainsWithBase(inputList, baseName)
		} else {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Which domain or project name would you like to check for "+strings.Join(inputList, ", ")+"? (e.g. liegeagents"+domain.NormalizeTLD(inputList[0])+")")
			return
		}
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	res, err := b.domain.SearchDomains(ctx, inputList)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("Domain search failed: %v", err))
		return
	}

	formatted := b.domain.FormatResponse(res)
	b.sendReply(msg.Chat.ID, msg.MessageID, formatted)
	var botID int64
	var botUsername string
	if b.api != nil {
		botID = b.api.Self.ID
		botUsername = b.api.Self.UserName
	}
	_ = b.memory.SaveMessage(ctx, msg.Chat.ID, botID, botUsername, "assistant", formatted)
	b.recordActiveDialog(msg.Chat.ID, msg.MessageID, formatted, msg.From.ID, msg.From.UserName)
}

func (b *Bot) handleXCommand(ctx context.Context, msg *tgbotapi.Message, args []string) {
	if b.xhandle == nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "X handle search service is not initialized.")
		return
	}
	query := strings.TrimSpace(strings.Join(args, " "))
	handles := b.xhandle.ParseInputHandles(query)
	if len(handles) == 0 {
		var baseName string
		if b.memory != nil {
			recentMsgs, _ := b.memory.GetRecentMessages(ctx, msg.Chat.ID, 10)
			baseName = b.extractDomainBaseFromHistory(recentMsgs, "")
		}
		if baseName == "" {
			b.dialogMu.RLock()
			if dialog, exists := b.activeDialogs[msg.Chat.ID]; exists && dialog != nil {
				baseName = domain.ExtractDomainBase(dialog.LastBotSnippet)
			}
			b.dialogMu.RUnlock()
		}
		if baseName != "" {
			handles = []string{baseName}
		} else {
			b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/x <handle>` (e.g. `/x liegeagents` or `/x curtain, veilora`)")
			return
		}
	}

	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	results, err := b.xhandle.CheckHandles(ctx, handles)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("X handle check failed: %v", err))
		return
	}

	formatted := b.xhandle.FormatResponse(results)
	b.sendReply(msg.Chat.ID, msg.MessageID, formatted)
	var botID int64
	var botUsername string
	if b.api != nil {
		botID = b.api.Self.ID
		botUsername = b.api.Self.UserName
	}
	_ = b.memory.SaveMessage(ctx, msg.Chat.ID, botID, botUsername, "assistant", formatted)
	b.recordActiveDialog(msg.Chat.ID, msg.MessageID, formatted, msg.From.ID, msg.From.UserName)
}

func (b *Bot) extractDomainBaseFromHistory(history []memory.Message, currentPrompt string) string {
	if currentPrompt != "" {
		if base := domain.ExtractDomainBase(currentPrompt); base != "" {
			return base
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		content := history[i].Content
		if base := domain.ExtractDomainBase(content); base != "" {
			return base
		}
	}
	return ""
}

func (b *Bot) runProactiveEngine(ctx context.Context) {
	// Autonomous dynamic timer loop: Shipp sets its own wake-up schedule based on activity
	initialDelay := 15 * time.Minute
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			nextMin := b.triggerProactiveLoop(ctx)
			if nextMin < 10 {
				nextMin = 30
			}
			timer.Reset(time.Duration(nextMin) * time.Minute)
		}
	}
}

func (b *Bot) triggerProactiveLoop(ctx context.Context) int {
	activeChats, err := b.memory.GetActiveChatIDs(ctx)
	if err != nil {
		log.Printf("[Proactive] Failed to get active chats: %v", err)
	}

	var groupChats []int64
	for _, id := range activeChats {
		if id < 0 {
			groupChats = append(groupChats, id)
		}
	}

	var targetChatID int64
	var recentGroupMsgs []memory.Message
	now := time.Now()

	if len(groupChats) > 0 {
		rand.Seed(time.Now().UnixNano())
		shuffled := make([]int64, len(groupChats))
		copy(shuffled, groupChats)
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		for _, cid := range shuffled {
			b.proactiveMu.RLock()
			disabled := b.proactiveDisabled[cid]
			lastSent := b.lastProactiveTime[cid]
			b.proactiveMu.RUnlock()

			if disabled {
				continue
			}
			if now.Sub(lastSent) >= 90*time.Minute {
				targetChatID = cid
				recentGroupMsgs, _ = b.memory.GetRecentMessages(ctx, cid, 8)
				break
			}
		}
	}

	// Gather Moltbook context if configured
	moltbookSnippet := ""
	if b.moltbook != nil && b.moltbook.IsConfigured() {
		// Priority 1: Check inbox for direct mentions, replies, or comments
		notifs, err := b.moltbook.GetNotifications(ctx, 10)
		var unreadActionable []moltbook.NotificationItem
		if err == nil && notifs != nil {
			for _, n := range notifs.Notifications {
				if !n.IsRead && (n.Type == "mention" || n.Type == "comment_reply" || n.Type == "post_comment") {
					unreadActionable = append(unreadActionable, n)
				}
			}
		}

		if len(unreadActionable) > 0 {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("Your Moltbook Inbox (%d unread notifications where agents reached out to you):\n", len(unreadActionable)))
			for _, n := range unreadActionable {
				pTitle := ""
				pID := n.RelatedPostID
				if n.Post != nil {
					pTitle = n.Post.Title
					if pID == "" {
						pID = n.Post.ID
					}
				}
				commentText := ""
				if n.Comment != nil {
					commentText = n.Comment.Content
				}
				sb.WriteString(fmt.Sprintf("- Notification [%s on post %s %q]: %s | Comment: %q\n", n.Type, pID, pTitle, n.Content, commentText))
			}
			moltbookSnippet = sb.String()
		} else {
			// Priority 2: Inbox is clear, pull 10 ambient posts from the network
			posts, err := b.moltbook.GetFeed(ctx, "hot", 10)
			if err == nil && len(posts) > 0 {
				var sb strings.Builder
				sb.WriteString("Recent Moltbook AI network activity (10 latest discussions):\n")
				for _, p := range posts {
					author := p.Author.Name
					if author == "" {
						author = "agent"
					}
					sub := p.SubmoltName
					if sub == "" {
						sub = "general"
					}
					sb.WriteString(fmt.Sprintf("- [%s in m/%s] \"%s\" by @%s (+%d upvotes)\n", p.ID, sub, p.Title, author, p.Upvotes))

					// Retain high-signal discussions in persistent memory
					if p.Upvotes >= 5 && b.memory != nil {
						_ = b.memory.SaveMoltbookMemory(ctx, memory.MoltbookMemory{
							PostID:    p.ID,
							PostTitle: p.Title,
							Author:    author,
							Content:   p.Content,
							Tags:      sub,
							Upvotes:   p.Upvotes,
							SavedAt:   time.Now(),
						})
					}
				}
				moltbookSnippet = sb.String()
			}
		}

		// Inject past insights remembered from other agents
		if b.memory != nil {
			if pastMems, err := b.memory.GetMoltbookMemories(ctx, 3); err == nil && len(pastMems) > 0 {
				var msb strings.Builder
				msb.WriteString("\nInsights remembered from other agents previously:\n")
				for _, m := range pastMems {
					preview := strings.ReplaceAll(m.Content, "\n", " ")
					if len(preview) > 120 {
						preview = preview[:117] + "..."
					}
					msb.WriteString(fmt.Sprintf("• @%s on %q: %s\n", m.Author, m.PostTitle, preview))
				}
				moltbookSnippet += msb.String()
			}
		}
	}

	decision, err := b.ai.GenerateProactiveDecision(ctx, recentGroupMsgs, moltbookSnippet)
	if err != nil || decision == nil {
		log.Printf("[Proactive] Failed to generate decision: %v", err)
		return 35
	}

	log.Printf("[Proactive] Autonomous Decision | Act: %v | Type: %s | Next: %dm | Reason: %s",
		decision.ShouldAct, decision.ActionType, decision.NextCheckInMin, decision.Reason)

	if !decision.ShouldAct || decision.ActionType == "none" || decision.ActionType == "" {
		return decision.NextCheckInMin
	}

	switch decision.ActionType {
	case "chat_message":
		chatMsg := decision.ChatMessage
		if chatMsg == "" {
			chatMsg = decision.Reason
		}
		if targetChatID != 0 && chatMsg != "" {
			b.sendSimpleMessage(targetChatID, chatMsg)
			_ = b.memory.SaveMessage(ctx, targetChatID, b.api.Self.ID, b.api.Self.UserName, "assistant", chatMsg)
			b.proactiveMu.Lock()
			b.lastProactiveTime[targetChatID] = now
			b.proactiveMu.Unlock()
			log.Printf("[Proactive] Sent spontaneous observation to chat %d: %q", targetChatID, chatMsg)
		}

	case "sandbox_task":
		goal := decision.SandboxGoal
		if goal == "" {
			goal = decision.Reason
		}
		if b.sandbox != nil && b.ai != nil && goal != "" {
			log.Printf("[Proactive] Stage 2: Synthesizing sandbox script for goal: %q", goal)
			script, err := b.ai.SynthesizeSandboxScript(ctx, goal)
			if err != nil {
				log.Printf("[Proactive] Failed to synthesize sandbox script: %v", err)
				break
			}

			// Stage 3: Local pre-flight syntax validation via bash -n
			script = cleanBashCommand(script)
			if err := ValidateBashScript(ctx, script); err != nil {
				log.Printf("[Proactive] Pre-flight bash syntax validation failed: %v | Skipping dispatch silently. Rejected script:\n%s", err, script)
				break
			}

			// Autonomous background experiments must only report to owner DM (never public group chats)
			ownerChat := b.getPrimaryOwnerChatID(ctx)
			taskID, err := b.sandbox.DispatchWithPrompt(ctx, ownerChat, 0, 0, script, "public", "autonomous sandbox experiment")
			if err != nil {
				log.Printf("[Proactive] Failed to dispatch autonomous sandbox task: %v", err)
			} else {
				log.Printf("[Proactive] Autonomously dispatched validated sandbox task %s (destination owner chat %d): %q", taskID, ownerChat, goal)
			}
		}

	case "moltbook_post":
		if b.moltbook != nil && b.moltbook.IsConfigured() {
			title := decision.MoltbookTitle
			content := decision.MoltbookPost
			if title == "" && content != "" {
				title = "observations from the edge"
			}
			if content != "" {
				sub := decision.Submolt
				if sub == "" {
					sub = "general"
				}
				res, err := b.moltbook.CreatePost(ctx, sub, title, content)
				if err != nil {
					log.Printf("[Proactive] Failed to autonomously post to Moltbook: %v", err)
				} else {
					v := res.Verification
					if v == nil && res.Post != nil {
						v = res.Post.Verification
					}
					if v != nil && v.VerificationCode != "" {
						ans, sErr := b.ai.SolveMoltbookChallenge(ctx, v.ChallengeText, v.Instructions)
						if sErr == nil {
							_ = b.moltbook.VerifyChallenge(ctx, v.VerificationCode, ans)
						}
					}
					log.Printf("[Proactive] Autonomously published post on Moltbook m/%s: %q", sub, title)
				}
			}
		}

	case "moltbook_comment":
		reply := decision.MoltbookReply
		if b.moltbook != nil && b.moltbook.IsConfigured() && decision.TargetPostID != "" && reply != "" {
			res, err := b.moltbook.CreateComment(ctx, decision.TargetPostID, reply)
			if err != nil {
				log.Printf("[Proactive] Failed to comment on Moltbook: %v", err)
			} else {
				if res.Verification != nil && res.Verification.VerificationCode != "" {
					ans, sErr := b.ai.SolveMoltbookChallenge(ctx, res.Verification.ChallengeText, res.Verification.Instructions)
					if sErr == nil {
						_ = b.moltbook.VerifyChallenge(ctx, res.Verification.VerificationCode, ans)
					}
				}
				log.Printf("[Proactive] Autonomously replied to Moltbook post %s: %q", decision.TargetPostID, reply)
				_ = b.moltbook.MarkPostNotificationsRead(ctx, decision.TargetPostID)
			}
		}

	case "moltbook_explore":
		if b.moltbook != nil && b.moltbook.IsConfigured() {
			query := decision.MoltbookSearchQuery
			sub := decision.Submolt
			var exploredPosts []moltbook.Post
			var err error
			if query != "" {
				log.Printf("[Proactive] Autonomously exploring Moltbook search for: %q", query)
				exploredPosts, err = b.moltbook.SearchPosts(ctx, query, 5)
			} else if sub != "" {
				log.Printf("[Proactive] Autonomously exploring Moltbook submolt m/%s", sub)
				exploredPosts, err = b.moltbook.GetSubmoltFeed(ctx, sub, "hot", 5)
			}
			if err == nil && len(exploredPosts) > 0 && b.memory != nil {
				for _, p := range exploredPosts {
					author := p.Author.Name
					if author == "" {
						author = "agent"
					}
					_ = b.memory.SaveMoltbookMemory(ctx, memory.MoltbookMemory{
						PostID:    p.ID,
						PostTitle: p.Title,
						Author:    author,
						Content:   p.Content,
						Tags:      p.SubmoltName,
						Upvotes:   p.Upvotes,
						SavedAt:   time.Now(),
					})
				}
				log.Printf("[Proactive] Stored %d explored Moltbook insights in memory", len(exploredPosts))
			}
		}
	}

	return decision.NextCheckInMin
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
		"• \"Draft an X post about PR #5\" or \"write a thread about our sandbox runner\"\n" +
		"• \"Send 0.01 eth to 0x... on base\" (Owner only)\n" +
		"• \"Email dev@example.com about the release update\" (Owner only)\n\n" +
		"Slash Commands:\n" +
		"• `/curate <topic>` or `/draft <topic>` - Curate a high-impact X/Twitter post or thread\n" +
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
	isOwner := b.isSenderOwner(msg.From)
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
	isOwner := b.isSenderOwner(msg.From)

	doc := msg.Document
	if doc == nil {
		return
	}

	// Always cache received document so follow-up queries like "can you read this doc" work reliably
	b.recentDocMu.Lock()
	b.recentDocs[chatID] = &RecentDocInfo{
		Document:   doc,
		ReceivedAt: time.Now(),
		SenderID:   senderID,
		Username:   username,
	}
	b.recentDocMu.Unlock()

	cleanCaption := b.cleanPrompt(msg.Caption)
	mime := strings.ToLower(doc.MimeType)
	ext := strings.ToLower(filepath.Ext(doc.FileName))

	// Check if this document was sent in response to the bot asking for a file/doc
	askedForDoc := false
	b.dialogMu.RLock()
	if dialog, exists := b.activeDialogs[chatID]; exists && dialog != nil {
		lowerSnippet := strings.ToLower(dialog.LastBotSnippet)
		if strings.Contains(lowerSnippet, "file") || strings.Contains(lowerSnippet, "doc") || strings.Contains(lowerSnippet, "paste the text") {
			askedForDoc = true
		}
	}
	b.dialogMu.RUnlock()

	isPrivate := msg.Chat.IsPrivate()
	shouldRespond := isPrivate || b.isAddressedToBot(msg) || cleanCaption != "" || askedForDoc
	if !shouldRespond {
		return
	}

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

	b.processDocument(ctx, chatID, msg.MessageID, senderID, username, isOwner, doc, cleanCaption)
}

func (b *Bot) processDocument(
	ctx context.Context,
	chatID int64,
	replyToMsgID int,
	senderID int64,
	username string,
	isOwner bool,
	doc *tgbotapi.Document,
	cleanCaption string,
) {
	if doc == nil {
		return
	}

	ext := strings.ToLower(filepath.Ext(doc.FileName))
	// Document types: .md, .pdf, .docx, .txt, .csv, .json, .log
	if ext != ".md" && ext != ".pdf" && ext != ".docx" && ext != ".txt" && ext != ".csv" && ext != ".json" && ext != ".log" {
		b.sendReply(chatID, replyToMsgID, fmt.Sprintf("I support document analysis for `.md`, `.pdf`, and `.docx` (or `.txt`/`.json`). `%s` isn't supported yet.", doc.FileName))
		return
	}

	// 15MB file size limit
	if doc.FileSize > 15*1024*1024 {
		b.sendReply(chatID, replyToMsgID, "That document is too large! Please send a file under 15MB.")
		return
	}

	b.sendChatAction(chatID, tgbotapi.ChatTyping)

	fileURL, err := b.api.GetFileDirectURL(doc.FileID)
	if err != nil {
		b.sendReply(chatID, replyToMsgID, "Couldn't fetch file from Telegram.")
		return
	}

	resp, err := http.Get(fileURL)
	if err != nil {
		b.sendReply(chatID, replyToMsgID, "Failed to download document.")
		return
	}
	defer resp.Body.Close()

	docBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		b.sendReply(chatID, replyToMsgID, "Failed to read document content.")
		return
	}

	extractedText, err := docparser.ParseDocument(doc.FileName, docBytes)
	if err != nil || strings.TrimSpace(extractedText) == "" {
		b.sendReply(chatID, replyToMsgID, fmt.Sprintf("Couldn't extract text from `%s`: %v", doc.FileName, err))
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
		b.sendReply(chatID, replyToMsgID, "I extracted the document text, but couldn't generate the analysis.")
		return
	}

	b.sendReply(chatID, replyToMsgID, analysis)
	_ = b.memory.SaveMessage(ctx, chatID, b.api.Self.ID, b.api.Self.UserName, "assistant", analysis)
	b.recordActiveDialog(chatID, replyToMsgID, analysis, senderID, username)
}

func isDocReadQuery(lower string) bool {
	hasVerb := strings.Contains(lower, "read") || strings.Contains(lower, "check") || strings.Contains(lower, "summarize") || strings.Contains(lower, "analyze") || strings.Contains(lower, "what does") || strings.Contains(lower, "what is in") || strings.Contains(lower, "what's in") || strings.Contains(lower, "whats in") || strings.Contains(lower, "explain") || strings.Contains(lower, "break down")
	hasDocNoun := strings.Contains(lower, "doc") || strings.Contains(lower, "file") || strings.Contains(lower, "pdf") || strings.Contains(lower, "document") || strings.Contains(lower, "prd") || strings.Contains(lower, "markdown")
	return hasVerb && hasDocNoun
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
var issueRegex = regexp.MustCompile(`(?i)(?:issue)\s*#?(\d+)`)
var doubleBoldRegex = regexp.MustCompile(`\*\*(.+?)\*\*`)
var doubleUnderscoreRegex = regexp.MustCompile(`__(.+?)__`)
var leakedToolCallRegex = regexp.MustCompile(`(?si)<(?:toolcall|tool_call)[^>]*>.*?</(?:toolcall|tool_call)>`)
var leakedFunctionRegex = regexp.MustCompile(`(?si)<function(?:=|\s+name=)[^>]*>.*?</function>`)
var leakedDeclarationRegex = regexp.MustCompile(`(?si)(?:declaration|call):default_api:[a-zA-Z0-9_]+\s*\{.*?\}?`)
var eagerPromptRegex = regexp.MustCompile(`(?i)(?:,\s*|\.\s*|\s+)(?:what(?:'s|\s+is)\s+next\??|what\s+are\s+we\s+building(?:\s+next)?\??|what(?:'s|\s+is)\s+(?:the\s+)?(?:next\s+)?move\??|what\s+are\s+we\s+cooking(?:\s+next)?\??|what\s+are\s+we\s+doing(?:\s+next)?\??|how\s+can\s+i\s+help(?:\s+you)?\??|who\s+else\s+is\s+building[^?.!\n]*\??|anyone\s+(?:else\s+)?(?:actually\s+)?(?:shipping|building)[^?.!\n]*\??|are\s+we\s+all\s+just\s+staring\s+at\s+charts\??)\s*$`)

var dmIntentRegex = regexp.MustCompile(`(?i)^(?:/dm|dm|send\s+dm\s+to|dm\s+to)\s+@?([a-zA-Z0-9_]{2,32})[\s:,]+(.+)$`)

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

var tgMentionRegex = regexp.MustCompile(`(^|[\s(\["'])(@([a-zA-Z0-9_]{3,32}))\b`)

func (b *Bot) sanitizeThirdPartyMentions(text string) string {
	if b == nil || b.cfg == nil {
		return text
	}
	return tgMentionRegex.ReplaceAllStringFunc(text, func(m string) string {
		sub := tgMentionRegex.FindStringSubmatch(m)
		if len(sub) < 4 {
			return m
		}
		prefix := sub[1]
		uname := sub[3]
		if b.cfg.IsOwner(uname) {
			return m
		}
		if b.api != nil && strings.EqualFold(uname, b.api.Self.UserName) {
			return m
		}
		return prefix + uname
	})
}

func (b *Bot) cleanOutgoingText(text string) string {
	cleaned := cleanNoEmojis(text)
	return b.sanitizeThirdPartyMentions(cleaned)
}

func stripLeadingMention(text string, recipientUsername string) string {
	if recipientUsername == "" || text == "" {
		return text
	}
	cleanUsername := strings.TrimPrefix(strings.ToLower(recipientUsername), "@")
	prefix := "@" + cleanUsername
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, prefix) {
		trimmed := strings.TrimSpace(text[len(prefix):])
		trimmed = strings.TrimLeft(trimmed, ",: -")
		if trimmed != "" {
			return trimmed
		}
	}
	return text
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

func extractIssueNumber(history []memory.Message, prompt string) int {
	if m := issueRegex.FindStringSubmatch(prompt); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if m := issueRegex.FindStringSubmatch(history[i].Content); len(m) > 1 {
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
	if b.api == nil {
		return
	}
	htmlText := toTelegramHTML(b.cleanOutgoingText(text))

	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}
	if threadID == 0 && replyToMsgID > 0 {
		threadID = b.lookupMsgThread(chatID, replyToMsgID)
	}

	if threadID != 0 {
		b.sendViaThreadAPI(ctx, chatID, threadID, replyToMsgID, htmlText)
		return
	}

	msg := tgbotapi.NewMessage(chatID, htmlText)
	msg.ParseMode = "HTML"
	// In 1-on-1 private DMs (chatID > 0), text back naturally without quoting.
	// In group chats (chatID < 0), quote so replies are clearly directed.
	if replyToMsgID > 0 && chatID < 0 {
		msg.ReplyToMessageID = replyToMsgID
	}
	_, err := b.api.Send(msg)
	if err != nil {
		msg.ParseMode = ""
		msg.Text = stripHTMLTags(htmlText)
		_, _ = b.api.Send(msg)
	}
}

// sendSimpleMessage sends without replying to a specific message. Thread-aware via ctx or active chat thread.
func (b *Bot) sendSimpleMessage(chatID int64, text string) {
	b.sendSimpleMessageCtx(context.Background(), chatID, text)
}

func (b *Bot) sendSimpleMessageCtx(ctx context.Context, chatID int64, text string) {
	if b.api == nil {
		return
	}
	htmlText := toTelegramHTML(b.cleanOutgoingText(text))

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
	if b.api == nil {
		return
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", htmlText)
	params.AddNonEmpty("parse_mode", "HTML")
	params.AddNonZero("message_thread_id", threadID)
	if replyToMsgID > 0 && chatID < 0 {
		params.AddNonZero("reply_to_message_id", replyToMsgID)
	}
	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil || (resp != nil && !resp.Ok) {
		// Fallback 1: retry without parse mode and strip any broken HTML tags
		params["parse_mode"] = ""
		params["text"] = stripHTMLTags(htmlText)
		resp2, err2 := b.api.MakeRequest("sendMessage", params)
		if err2 != nil || (resp2 != nil && !resp2.Ok) {
			// Fallback 2: if thread was deleted or failed, send to chat without message_thread_id
			delete(params, "message_thread_id")
			_, _ = b.api.MakeRequest("sendMessage", params)
		}
	}
}

func (b *Bot) sendChatAction(chatID int64, action string) {
	b.sendChatActionCtx(context.Background(), chatID, action)
}

func (b *Bot) sendChatActionCtx(ctx context.Context, chatID int64, action string) {
	if b.api == nil {
		return
	}
	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}
	if threadID != 0 {
		params := tgbotapi.Params{}
		params.AddNonZero64("chat_id", chatID)
		params.AddNonEmpty("action", action)
		params.AddNonZero("message_thread_id", threadID)
		_, _ = b.api.MakeRequest("sendChatAction", params)
		return
	}
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

var dynamicSandboxSuccessAcks = []string{
	"ran it clean in %ds:",
	"done in %ds, here's what came out:",
	"clean run, took %ds:",
	"wrapped that up in %ds:",
	"all done (took %ds):",
	"executed clean in %ds:",
}

var dynamicSandboxFailAcks = []string{
	"hit an issue after %ds (exit code %d):",
	"threw an error in %ds (exit code %d):",
	"ran into an error after %ds (exit code %d):",
	"tripped up with exit code %d in %ds:",
}

func (b *Bot) getRandomSandboxSuccessAck(duration int) string {
	tmpl := dynamicSandboxSuccessAcks[rand.Intn(len(dynamicSandboxSuccessAcks))]
	return fmt.Sprintf(tmpl, duration)
}

func (b *Bot) getRandomSandboxFailAck(duration, exitCode int) string {
	tmpl := dynamicSandboxFailAcks[rand.Intn(len(dynamicSandboxFailAcks))]
	if strings.Contains(tmpl, "code %d in %ds") {
		return fmt.Sprintf(tmpl, exitCode, duration)
	}
	return fmt.Sprintf(tmpl, duration, exitCode)
}

func (b *Bot) handleSandboxCompletion(payload sandbox.CallbackPayload) {
	if payload.ChatID == 0 {
		return
	}
	cleanOutput := strings.TrimSpace(cleanNoEmojis(payload.Output))
	if len(cleanOutput) > 3000 {
		cleanOutput = cleanOutput[:3000] + "\n... (output truncated)"
	}

	threadID := 0
	replyToMsgID := 0
	taskPrompt := ""
	taskCmd := ""
	if b.sandbox != nil {
		if task := b.sandbox.GetTask(payload.TaskID); task != nil {
			threadID = task.ThreadID
			replyToMsgID = task.ReplyToMsgID
			taskPrompt = task.Prompt
			taskCmd = task.Command
		}
	}

	lineCount := strings.Count(cleanOutput, "\n") + 1
	isShortOutput := lineCount <= 3 && len(cleanOutput) <= 250
	isSlashCmd := strings.HasPrefix(strings.TrimSpace(taskPrompt), "/")

	var text string

	// 1. Attempt Option 2: Casual human AI synthesis when prompt was a conversational request
	if b.ai != nil && taskPrompt != "" && !isSlashCmd && len(cleanOutput) > 0 {
		ctxAI, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		aiReply, err := b.ai.SynthesizeSandboxResult(ctxAI, taskPrompt, taskCmd, payload.DurationSeconds, payload.ExitCode, cleanOutput)
		cancel()

		aiReply = strings.TrimSpace(cleanNoEmojis(aiReply))
		aiReply = strings.ReplaceAll(aiReply, "```", "")
		aiReply = strings.Trim(aiReply, "`")

		if err == nil && aiReply != "" {
			if isShortOutput {
				// The AI reply already naturally answers with the result (e.g. "ran that go script, result is 14")
				text = aiReply
			} else {
				// Multi-line output gets the conversational intro and the logs below
				text = fmt.Sprintf("%s\n\n```\n%s\n```", aiReply, cleanOutput)
			}
		}
	}

	// 2. Fallback (Option 1): Natural, unstylized dev phrasing matching how Shipp started
	if text == "" {
		if payload.ExitCode == 0 {
			if cleanOutput == "" {
				text = fmt.Sprintf("clean run in %ds.", payload.DurationSeconds)
			} else if isShortOutput {
				// Plain text, unstylized! Direct casual text
				text = fmt.Sprintf("ran it clean in %ds, got: %s", payload.DurationSeconds, cleanOutput)
			} else {
				text = fmt.Sprintf("ran it clean in %ds:\n```\n%s\n```", payload.DurationSeconds, cleanOutput)
			}
		} else {
			if cleanOutput == "" {
				text = fmt.Sprintf("hit an error after %ds (exit code %d).", payload.DurationSeconds, payload.ExitCode)
			} else if isShortOutput {
				text = fmt.Sprintf("hit an error in %ds: %s", payload.DurationSeconds, cleanOutput)
			} else {
				text = fmt.Sprintf("hit an error in %ds (exit code %d):\n```\n%s\n```", payload.DurationSeconds, payload.ExitCode, cleanOutput)
			}
		}
	}

	ctx := context.Background()
	if threadID != 0 {
		ctx = context.WithValue(ctx, ctxKeyThreadID{}, threadID)
	}

	// Autonomous background sandbox tasks:
	// 1. Always record the run in persistent memory (so Shipp recalls what he ran/found)
	// 2. High-signal filter: only message owner DM if there is a genuinely noteworthy finding (zero raw terminal dumps, zero backticks).
	// If mundane or routine: stay completely silent!
	isAutonomous := (taskPrompt == "autonomous sandbox experiment") || strings.HasPrefix(payload.TaskID, "auto_")
	if isAutonomous {
		if taskCmd == "" {
			taskCmd = "autonomous background task"
		}
		isNoteworthy := false
		insight := ""
		if b.ai != nil {
			ctxEval, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			noteworthy, evalInsight, err := b.ai.EvaluateSandboxInsight(ctxEval, taskCmd, taskCmd, cleanOutput, payload.ExitCode, payload.DurationSeconds)
			cancel()
			if err == nil {
				isNoteworthy = noteworthy
				insight = evalInsight
			}
		}

		if b.memory != nil {
			run := memory.SandboxRun{
				Goal:            taskCmd,
				Command:         taskCmd,
				ExitCode:        payload.ExitCode,
				Output:          cleanOutput,
				DurationSeconds: payload.DurationSeconds,
				IsNoteworthy:    isNoteworthy,
				Insight:         insight,
				CreatedAt:       time.Now(),
			}
			if err := b.memory.SaveSandboxRun(context.Background(), run); err != nil {
				log.Printf("[Sandbox] Warning: failed to save sandbox run to memory: %v", err)
			}
		}

		if isNoteworthy && insight != "" {
			log.Printf("[Sandbox] Autonomous run %s produced noteworthy insight: %q. Notifying creator via DM.", payload.TaskID, insight)
			ownerChat := b.getPrimaryOwnerChatID(ctx)
			if ownerChat != 0 {
				b.sendSimpleMessageCtx(ctx, ownerChat, insight)
			} else {
				b.alertOwners(ctx, insight)
			}
		} else {
			log.Printf("[Sandbox] Autonomous run %s completed cleanly (exit %d in %ds, noteworthy=%v). Stored silently in memory.", payload.TaskID, payload.ExitCode, payload.DurationSeconds, isNoteworthy)
		}
		return
	}

	if replyToMsgID > 0 {
		b.sendReplyCtx(ctx, payload.ChatID, replyToMsgID, text)
	} else {
		b.sendSimpleMessageCtx(ctx, payload.ChatID, text)
	}
}

func (b *Bot) handleSandboxTimeout(task *sandbox.Task) {
	if task == nil || task.ChatID == 0 {
		return
	}
	ctx := context.Background()
	if task.ThreadID != 0 {
		ctx = context.WithValue(ctx, ctxKeyThreadID{}, task.ThreadID)
	}
	text := "sandbox task hit the 6-minute timeout without finishing."

	// Autonomous background sandbox tasks must NEVER post timeout to group chats or spam DM
	isAutonomous := (task.Prompt == "autonomous sandbox experiment") || strings.HasPrefix(task.ID, "auto_")
	if isAutonomous {
		log.Printf("[Sandbox] Autonomous background sandbox task %s timed out after 6 minutes. Stored silently.", task.ID)
		if b.memory != nil {
			_ = b.memory.SaveSandboxRun(ctx, memory.SandboxRun{
				Goal:            task.Command,
				Command:         task.Command,
				ExitCode:        -1,
				Output:          "timed out after 6 minutes",
				DurationSeconds: 360,
				IsNoteworthy:    false,
				CreatedAt:       time.Now(),
			})
		}
		return
	}

	if task.ReplyToMsgID > 0 {
		b.sendReplyCtx(ctx, task.ChatID, task.ReplyToMsgID, text)
	} else {
		b.sendSimpleMessageCtx(ctx, task.ChatID, text)
	}
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

