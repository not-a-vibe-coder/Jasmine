package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/imagegen"
	"shipp/internal/walmem"
)

// Cosine distance above which a recalled memory is treated as unrelated to the prompt.
const memoryRelevanceCutoff = 0.8

// SetWalrusMemory enables long-term memory on Walrus. A nil client disables it.
func (b *Bot) SetWalrusMemory(c *walmem.Client) { b.walmem = c }

// SetImageGen enables the image generation tool and /imagine.
func (b *Bot) SetImageGen(s *imagegen.Service) { b.imagegen = s }

// recallMemories pulls what Jasmine remembers about this user (across every chat they
// have talked to her in) and, in groups, what the group has told her. It never blocks
// a reply for long: on timeout or error it returns whatever it has.
func (b *Bot) recallMemories(ctx context.Context, msg *tgbotapi.Message, query string) string {
	if b.walmem == nil || msg.From == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer cancel()

	type result struct {
		label string
		text  string
	}
	namespaces := []struct{ label, ns string }{
		{"About @" + senderName(msg.From) + " (from any chat, any device)", walmem.UserNamespace(msg.From.ID)},
	}
	if msg.Chat != nil && !msg.Chat.IsPrivate() {
		namespaces = append(namespaces, struct{ label, ns string }{"Shared by this group", walmem.GroupNamespace(msg.Chat.ID)})
	}

	results := make([]result, len(namespaces))
	var wg sync.WaitGroup
	for i, n := range namespaces {
		wg.Add(1)
		go func(i int, label, ns string) {
			defer wg.Done()
			mems, err := b.walmem.Recall(ctx, ns, query, 6)
			if err != nil {
				log.Printf("[Walrus] recall %s failed: %v", ns, err)
				return
			}
			results[i] = result{label, walmem.FormatForPrompt(mems, memoryRelevanceCutoff)}
		}(i, n.label, n.ns)
	}
	wg.Wait()

	var sections []string
	for _, r := range results {
		if r.text != "" {
			sections = append(sections, r.label+":\n"+r.text)
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n- LONG-TERM MEMORY (recalled from Walrus, encrypted and owned on-chain). Use these naturally when relevant, the way a friend remembers things; never list them back verbatim or say 'according to my memory':\n" + strings.Join(sections, "\n")
}

// learnFromMessage sends the user's message to the Walrus relayer, which extracts durable
// facts and stores each as an encrypted blob. Runs in the background after the reply.
func (b *Bot) learnFromMessage(msg *tgbotapi.Message, text string) {
	if b.walmem == nil || msg.From == nil {
		return
	}
	text = strings.TrimSpace(text)
	if len([]rune(text)) < 12 || strings.HasPrefix(text, "/") {
		return
	}
	name := senderName(msg.From)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		input := fmt.Sprintf("@%s said: %s", name, text)
		res, err := b.walmem.Analyze(ctx, walmem.UserNamespace(msg.From.ID), input)
		if err != nil {
			log.Printf("[Walrus] analyze for @%s failed: %v", name, err)
			return
		}
		if res.FactCount > 0 {
			log.Printf("[Walrus] stored %d new fact(s) about @%s", res.FactCount, name)
		}
		if msg.Chat != nil && !msg.Chat.IsPrivate() {
			if _, err := b.walmem.Analyze(ctx, walmem.GroupNamespace(msg.Chat.ID), input); err != nil {
				log.Printf("[Walrus] group analyze failed: %v", err)
			}
		}
	}()
}

func senderName(u *tgbotapi.User) string {
	if u == nil {
		return "someone"
	}
	if u.UserName != "" {
		return u.UserName
	}
	return u.FirstName
}

// rememberExplicit stores one fact verbatim and waits for its Walrus blob id.
func (b *Bot) rememberExplicit(ctx context.Context, userID int64, fact string) (string, error) {
	jobID, err := b.walmem.Remember(ctx, walmem.UserNamespace(userID), fact)
	if err != nil {
		return "", err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	st, err := b.walmem.WaitForBlob(waitCtx, jobID)
	if err != nil {
		return "", err
	}
	return st.BlobID, nil
}

func (b *Bot) handleMemoryCommand(ctx context.Context, msg *tgbotapi.Message) {
	if b.walmem == nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "long-term memory isn't switched on yet (MEMWAL_ACCOUNT_ID / MEMWAL_DELEGATE_KEY missing)")
		return
	}
	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	ns := walmem.UserNamespace(msg.From.ID)
	stats, err := b.walmem.Stats(ctx, ns)
	if err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "couldn't reach my memory on Walrus right now, try again in a bit")
		log.Printf("[Walrus] stats failed: %v", err)
		return
	}
	mems, _ := b.walmem.Recall(ctx, ns, "who is this person, what do they like, what are they working on", 8)

	var sb strings.Builder
	fmt.Fprintf(&sb, "here's what i remember about you, @%s\n\n", senderName(msg.From))
	if len(mems) == 0 {
		sb.WriteString("nothing yet. talk to me and i'll pick things up.\n")
	}
	for _, m := range mems {
		fmt.Fprintf(&sb, "• %s\n  %s\n", strings.TrimSpace(m.Text), walmem.BlobURL(m.BlobID))
	}
	fmt.Fprintf(&sb, "\n%d memories, %d bytes, Seal-encrypted on Walrus mainnet (account %s). /forget wipes them.",
		stats.MemoryCount, stats.StorageBytes, shortID(b.walmem.AccountID()))
	b.sendReply(msg.Chat.ID, msg.MessageID, sb.String())
}

func (b *Bot) handleForgetCommand(ctx context.Context, msg *tgbotapi.Message) {
	if b.walmem == nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "i don't have long-term memory switched on, so there's nothing to forget")
		return
	}
	n, err := b.walmem.Forget(ctx, walmem.UserNamespace(msg.From.ID))
	if err != nil {
		log.Printf("[Walrus] forget failed: %v", err)
		b.sendReply(msg.Chat.ID, msg.MessageID, "couldn't wipe your memories right now, try again shortly")
		return
	}
	b.sendReply(msg.Chat.ID, msg.MessageID, fmt.Sprintf("done. forgot %d things about you. fresh start.", n))
}

func (b *Bot) handleRememberCommand(ctx context.Context, msg *tgbotapi.Message, fact string) {
	if b.walmem == nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "long-term memory isn't switched on yet")
		return
	}
	fact = strings.TrimSpace(fact)
	if fact == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/remember <something about you>` e.g. `/remember i'm allergic to peanuts`")
		return
	}
	b.sendChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	blobID, err := b.rememberExplicit(ctx, msg.From.ID, fmt.Sprintf("@%s: %s", senderName(msg.From), fact))
	if err != nil {
		log.Printf("[Walrus] remember failed: %v", err)
		b.sendReply(msg.Chat.ID, msg.MessageID, "saving that to Walrus is taking a while. it'll land shortly.")
		return
	}
	b.sendReply(msg.Chat.ID, msg.MessageID, "got it, i'll remember that.\nstored on Walrus: "+walmem.BlobURL(blobID))
}

func (b *Bot) handleImagineCommand(ctx context.Context, msg *tgbotapi.Message, prompt string) {
	if strings.TrimSpace(prompt) == "" {
		b.sendReply(msg.Chat.ID, msg.MessageID, "Usage: `/imagine <description>` e.g. `/imagine a walrus coding in a hoodie, neon lighting`")
		return
	}
	if err := b.generateAndSendImage(ctx, msg.Chat.ID, msg.MessageID, prompt); err != nil {
		b.sendReply(msg.Chat.ID, msg.MessageID, "couldn't draw that one right now, image servers are busy. try again in a minute")
	}
}

// generateAndSendImage renders a prompt and posts it as a photo in the right chat/thread.
func (b *Bot) generateAndSendImage(ctx context.Context, chatID int64, replyToMsgID int, prompt string) error {
	if b.imagegen == nil {
		return fmt.Errorf("image generation not configured")
	}
	b.sendChatAction(chatID, tgbotapi.ChatUploadPhoto)
	genCtx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	img, err := b.imagegen.Generate(genCtx, prompt)
	if err != nil {
		log.Printf("[Image] generation failed: %v", err)
		return err
	}
	log.Printf("[Image] %s rendered %d bytes for %q", img.Provider, len(img.Data), prompt)
	return b.sendPhotoCtx(ctx, chatID, replyToMsgID, img)
}

func (b *Bot) sendPhotoCtx(ctx context.Context, chatID int64, replyToMsgID int, img *imagegen.Image) error {
	if b.api == nil {
		return fmt.Errorf("telegram api not initialised")
	}
	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}
	if threadID == 0 && replyToMsgID > 0 {
		threadID = b.lookupMsgThread(chatID, replyToMsgID)
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_thread_id", threadID)
	if replyToMsgID > 0 && chatID < 0 {
		params.AddNonZero("reply_to_message_id", replyToMsgID)
	}
	ext := "jpg"
	if strings.Contains(img.MIMEType, "png") {
		ext = "png"
	}
	file := tgbotapi.RequestFile{Name: "photo", Data: tgbotapi.FileBytes{Name: "jasmine." + ext, Bytes: img.Data}}
	resp, err := b.api.UploadFiles("sendPhoto", params, []tgbotapi.RequestFile{file})
	if err != nil {
		return err
	}
	if resp != nil && resp.Ok && resp.Result != nil {
		var sent struct {
			MessageID int `json:"message_id"`
		}
		if json.Unmarshal(resp.Result, &sent) == nil && sent.MessageID > 0 {
			b.setLastBotMessageID(chatID, sent.MessageID)
		}
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 14 {
		return id[:8] + "…" + id[len(id)-4:]
	}
	return id
}

// keepTyping refreshes Telegram's typing indicator every 4s until the returned stop func runs.
func (b *Bot) keepTyping(chatID int64) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				b.sendChatAction(chatID, tgbotapi.ChatTyping)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
