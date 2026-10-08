package bot

// Expressive replies: voice notes in and out, emoji reactions, stickers and GIFs.
// Tools that post media mark the turn so Jasmine can stay quiet afterwards instead of
// adding a redundant text message, the way a person would just send the sticker.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/ai"
	"shipp/internal/voice"
)

// NoReply is what the model answers when a reaction, sticker, GIF or voice note says it all.
const NoReply = "NO_REPLY"

type ctxKeyTurn struct{}

// turnState tracks what the bot already posted while answering one message.
type turnState struct {
	mu      sync.Mutex
	voiceIn bool
	media   []string // e.g. "voice note: ...", "sticker 😂", "reacted 🔥"
}

func turnFromCtx(ctx context.Context) *turnState {
	t, _ := ctx.Value(ctxKeyTurn{}).(*turnState)
	return t
}

func (t *turnState) addMedia(what string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.media = append(t.media, what)
	t.mu.Unlock()
}

func (t *turnState) mediaSummary() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.media) == 0 {
		return ""
	}
	return "[" + strings.Join(t.media, "; ") + "]"
}

// finishTurn decides whether the final text still needs sending. It strips the NO_REPLY
// marker and suppresses empty/marker-only text when media already answered the message.
func finishTurn(t *turnState, finalText string) (string, bool) {
	text := strings.TrimSpace(strings.ReplaceAll(finalText, NoReply, ""))
	if text == "" && (t.mediaSummary() != "" || strings.Contains(finalText, NoReply)) {
		return "", false
	}
	return text, true
}

func (b *Bot) SetVoice(v *voice.Service) { b.voice = v }

// ---------- voice notes ----------

// handleVoiceMessage transcribes a voice note and handles it like a typed message, so it
// wakes Jasmine, lands in chat history and is learned into Walrus memory like any text.
func (b *Bot) handleVoiceMessage(ctx context.Context, msg *tgbotapi.Message) {
	if b.voice == nil {
		return
	}
	fileID, fileName, mimeType := "", "voice.ogg", "audio/ogg"
	switch {
	case msg.Voice != nil:
		fileID = msg.Voice.FileID
		if msg.Voice.MimeType != "" {
			mimeType = msg.Voice.MimeType
		}
	case msg.Audio != nil:
		fileID, fileName = msg.Audio.FileID, msg.Audio.FileName
		if msg.Audio.MimeType != "" {
			mimeType = msg.Audio.MimeType
		}
		if fileName == "" {
			fileName = "audio.mp3"
		}
	}
	if fileID == "" {
		return
	}
	audio, err := b.downloadTelegramFile(ctx, fileID, 20<<20)
	if err != nil {
		log.Printf("[Voice] download failed: %v", err)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	transcript, err := b.voice.Transcribe(tctx, audio, fileName, mimeType)
	cancel()
	if err != nil {
		log.Printf("[Voice] transcription failed: %v", err)
		if msg.Chat.IsPrivate() || b.isAddressedToBot(msg) {
			b.sendReply(msg.Chat.ID, msg.MessageID, "couldn't make out that voice note, mind sending it again or typing it?")
		}
		return
	}
	log.Printf("[Voice] transcribed %d bytes from @%s: %q", len(audio), senderName(msg.From), transcript)

	textMsg := *msg
	textMsg.Voice, textMsg.Audio = nil, nil
	textMsg.Text = transcript
	if msg.Caption != "" {
		textMsg.Text = msg.Caption + "\n" + transcript
	}
	turn := &turnState{voiceIn: true}
	b.handleMessage(context.WithValue(ctx, ctxKeyTurn{}, turn), &textMsg)
}

func (b *Bot) downloadTelegramFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	link, err := b.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

func (b *Bot) sendVoiceNote(ctx context.Context, chatID int64, replyTo int, text string) error {
	if b.voice == nil {
		return fmt.Errorf("voice is not configured")
	}
	b.sendChatAction(chatID, tgbotapi.ChatRecordVoice)
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	speech, err := b.voice.Speak(sctx, text)
	if err != nil {
		return err
	}
	file := tgbotapi.RequestFile{Name: "voice", Data: tgbotapi.FileBytes{Name: speech.FileName, Bytes: speech.Data}}
	if err := b.uploadMedia(ctx, "sendVoice", chatID, replyTo, nil, file); err != nil {
		return err
	}
	log.Printf("[Voice] sent %s voice note (%d bytes)", speech.Provider, len(speech.Data))
	return nil
}

// ---------- reactions ----------

// Telegram only accepts reactions from this fixed set.
var allowedReactions = map[string]bool{}

func init() {
	for _, e := range strings.Fields("👍 👎 ❤ 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂ 🤷 🤷‍♀ 😡") {
		allowedReactions[e] = true
	}
}

// normalizeReaction maps common emoji variants onto Telegram's allowed reaction set.
func normalizeReaction(e string) string {
	e = strings.TrimSpace(strings.ReplaceAll(e, "️", ""))
	aliases := map[string]string{"❤️": "❤", "😂": "🤣", "😆": "😁", "😄": "😁", "😀": "😁", "🙌": "👏", "💪": "🔥", "✅": "👌", "😮": "😱", "🥲": "😢", "💀": "🤣", "😅": "😁", "🫶": "❤"}
	if a, ok := aliases[e]; ok {
		e = a
	}
	if allowedReactions[e] {
		return e
	}
	return ""
}

func (b *Bot) reactToMessage(chatID int64, messageID int, emoji string) error {
	e := normalizeReaction(emoji)
	if e == "" {
		return fmt.Errorf("%q is not a reaction Telegram allows", emoji)
	}
	reaction, _ := json.Marshal([]map[string]string{{"type": "emoji", "emoji": e}})
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params["reaction"] = string(reaction)
	resp, err := b.api.MakeRequest("setMessageReaction", params)
	if err != nil {
		return err
	}
	if resp != nil && !resp.Ok {
		return fmt.Errorf("telegram: %s", resp.Description)
	}
	return nil
}

// ---------- stickers ----------

// DefaultStickerSets are public sets verified to exist (Telegram's own Utya duck and friends).
var DefaultStickerSets = []string{"UtyaDuck", "HotCherry", "MrCat", "PepeTheFrog"}

type stickerCache struct {
	once    sync.Once
	byEmoji map[string][]string // emoji -> file IDs
	all     []string
}

func (b *Bot) loadStickers() *stickerCache {
	b.stickers.once.Do(func() {
		b.stickers.byEmoji = map[string][]string{}
		sets := b.cfg.StickerSets
		if len(sets) == 0 {
			sets = DefaultStickerSets
		}
		for _, name := range sets {
			set, err := b.api.GetStickerSet(tgbotapi.GetStickerSetConfig{Name: name})
			if err != nil {
				log.Printf("[Sticker] set %s unavailable: %v", name, err)
				continue
			}
			for _, st := range set.Stickers {
				key := strings.ReplaceAll(st.Emoji, "️", "")
				b.stickers.byEmoji[key] = append(b.stickers.byEmoji[key], st.FileID)
				b.stickers.all = append(b.stickers.all, st.FileID)
			}
		}
		log.Printf("[Sticker] loaded %d stickers covering %d emojis", len(b.stickers.all), len(b.stickers.byEmoji))
	})
	return &b.stickers
}

func (b *Bot) sendSticker(ctx context.Context, chatID int64, replyTo int, emoji string) error {
	cache := b.loadStickers()
	ids := cache.byEmoji[strings.ReplaceAll(strings.TrimSpace(emoji), "️", "")]
	if len(ids) == 0 {
		return fmt.Errorf("no sticker for %s; available emojis: %s", emoji, cache.sampleEmojis(40))
	}
	params := b.threadParams(ctx, chatID, replyTo)
	params["sticker"] = ids[rand.Intn(len(ids))]
	resp, err := b.api.MakeRequest("sendSticker", params)
	if err != nil {
		return err
	}
	if resp != nil && !resp.Ok {
		return fmt.Errorf("telegram: %s", resp.Description)
	}
	return nil
}

func (c *stickerCache) sampleEmojis(n int) string {
	var out []string
	for e := range c.byEmoji {
		out = append(out, e)
		if len(out) == n {
			break
		}
	}
	return strings.Join(out, " ")
}

// ---------- GIFs ----------

func (b *Bot) sendGIF(ctx context.Context, chatID int64, replyTo int, query string) error {
	if b.cfg.GiphyAPIKey == "" {
		return fmt.Errorf("GIFs need GIPHY_API_KEY; send a sticker instead")
	}
	q := url.Values{}
	q.Set("api_key", b.cfg.GiphyAPIKey)
	q.Set("q", query)
	q.Set("limit", "12")
	q.Set("rating", "pg-13")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.giphy.com/v1/gifs/search?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Images struct {
				Original struct {
					MP4 string `json:"mp4"`
				} `json:"original"`
				Downsized struct {
					URL string `json:"url"`
				} `json:"downsized"`
			} `json:"images"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Data) == 0 {
		return fmt.Errorf("no GIF found for %q", query)
	}
	pick := out.Data[rand.Intn(len(out.Data))]
	media := pick.Images.Original.MP4
	if media == "" {
		media = pick.Images.Downsized.URL
	}
	params := b.threadParams(ctx, chatID, replyTo)
	params["animation"] = media
	r, err := b.api.MakeRequest("sendAnimation", params)
	if err != nil {
		return err
	}
	if r != nil && !r.Ok {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}

// ---------- shared helpers ----------

func (b *Bot) threadParams(ctx context.Context, chatID int64, replyTo int) tgbotapi.Params {
	threadID := 0
	if v := ctx.Value(ctxKeyThreadID{}); v != nil {
		threadID = v.(int)
	}
	if threadID == 0 && replyTo > 0 {
		threadID = b.lookupMsgThread(chatID, replyTo)
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_thread_id", threadID)
	if replyTo > 0 && chatID < 0 {
		params.AddNonZero("reply_to_message_id", replyTo)
	}
	return params
}

func (b *Bot) uploadMedia(ctx context.Context, method string, chatID int64, replyTo int, extra map[string]string, file tgbotapi.RequestFile) error {
	params := b.threadParams(ctx, chatID, replyTo)
	for k, v := range extra {
		params[k] = v
	}
	resp, err := b.api.UploadFiles(method, params, []tgbotapi.RequestFile{file})
	if err != nil {
		return err
	}
	if resp != nil && !resp.Ok {
		return fmt.Errorf("telegram: %s", resp.Description)
	}
	if resp != nil && resp.Result != nil {
		var sent struct {
			MessageID int `json:"message_id"`
		}
		if json.Unmarshal(resp.Result, &sent) == nil && sent.MessageID > 0 {
			b.setLastBotMessageID(chatID, sent.MessageID)
		}
	}
	return nil
}

func replyTarget(ctx context.Context) int {
	if v, ok := ctx.Value(ctxKeyReplyToMsgID{}).(int); ok {
		return v
	}
	return 0
}

// executeExpressiveTool runs the media tools. ok=false means the name isn't one of them.
func (b *Bot) executeExpressiveTool(ctx context.Context, chatID int64, toolName, arguments string) (string, bool) {
	var args struct {
		Text  string `json:"text"`
		Emoji string `json:"emoji"`
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)
	turn := turnFromCtx(ctx)
	replyTo := replyTarget(ctx)
	done := "Sent. If that says it all, reply with exactly " + NoReply + "; otherwise add one short line."

	switch toolName {
	case "send_voice_note":
		if strings.TrimSpace(args.Text) == "" {
			return "No text to speak.", true
		}
		if err := b.sendVoiceNote(ctx, chatID, replyTo, args.Text); err != nil {
			log.Printf("[Voice] send failed: %v", err)
			return fmt.Sprintf("Voice note failed (%v). Reply in text instead.", err), true
		}
		turn.addMedia("voice note: " + ai.TruncateRunes(args.Text, 200))
		return "Voice note sent. Do not repeat it in text; reply with exactly " + NoReply + " unless you must add a link or code.", true
	case "react_to_message":
		if replyTo == 0 {
			return "No message to react to.", true
		}
		if err := b.reactToMessage(chatID, replyTo, args.Emoji); err != nil {
			return fmt.Sprintf("Reaction failed (%v).", err), true
		}
		turn.addMedia("reacted " + args.Emoji)
		return "Reacted. If a reaction is enough, reply with exactly " + NoReply + "; otherwise reply normally.", true
	case "send_sticker":
		if err := b.sendSticker(ctx, chatID, replyTo, args.Emoji); err != nil {
			return fmt.Sprintf("Sticker failed (%v).", err), true
		}
		turn.addMedia("sticker " + args.Emoji)
		return done, true
	case "send_gif":
		if strings.TrimSpace(args.Query) == "" {
			return "No GIF search query.", true
		}
		if err := b.sendGIF(ctx, chatID, replyTo, args.Query); err != nil {
			return fmt.Sprintf("GIF failed (%v).", err), true
		}
		turn.addMedia("gif: " + args.Query)
		return done, true
	}
	return "", false
}
