package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"shipp/internal/ai"
	"shipp/internal/calls"
	"shipp/internal/walmem"
)

// Calls a non-owner can trigger per day (each one rings a real phone or Telegram account).
const callsPerDay = 3

// SetCalls enables phone (Twilio) and Telegram voice calls.
func (b *Bot) SetCalls(m *calls.Manager, pb *calls.Phonebook) {
	b.calls = m
	b.phonebook = pb
}

// ---------- calls.Brain ----------

func (b *Bot) Reply(ctx context.Context, s *calls.Session) (string, bool) {
	if b.ai == nil {
		return "", true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are Jasmine, on a live %s voice call with %s. You placed this call.\n", channelLabel(s.Channel), s.Name)
	fmt.Fprintf(&sb, "Why you called: %s\n", s.Purpose)
	if s.Requester != "" {
		fmt.Fprintf(&sb, "%s asked you to make this call.\n", s.Requester)
	}
	if s.Memories != "" {
		sb.WriteString("What you remember about them (use naturally, never list it):\n" + s.Memories + "\n")
	}
	sb.WriteString(strings.TrimPrefix(currentTimeContext(time.Now()), "\n- "))
	sb.WriteString(`

How to talk on a call:
- You are speaking out loud. 1 to 3 short sentences per turn, warm and natural, like a friend on the phone. Use their name now and then.
- Your first line says hi and gets to why you called, right away.
- Listen to what they actually say and respond to it. Many people you call speak Nigerian English or pidgin: understand it properly (\"e don dey run\" means it is running) and answer in the same style. If they sound stressed or low, slow down and be gentle.
- No emojis, no lists, no markdown, no links, no dashes, no stage directions.
- When they want to go, or the reason for the call is done and they have nothing else, say a short warm goodbye and end your reply with [END].`)

	var turns []ai.ChatMessage
	for _, t := range s.Turns() {
		role := "user"
		if t.Role == "jasmine" {
			role = "assistant"
		}
		turns = append(turns, ai.ChatMessage{Role: role, Content: t.Text})
	}
	text, err := b.ai.CallReply(ctx, sb.String(), turns)
	if err != nil {
		log.Printf("[Calls] reply failed: %v", err)
		return "", true
	}
	hangup := strings.Contains(text, "[END]")
	text = strings.TrimSpace(strings.ReplaceAll(text, "[END]", ""))
	return replaceDashPauses(text), hangup
}

func (b *Bot) SpeakMP3(ctx context.Context, text string) ([]byte, error) {
	if b.voice == nil {
		return nil, fmt.Errorf("voice is not configured")
	}
	sp, err := b.voice.SpeakMP3(ctx, text)
	if err != nil {
		return nil, err
	}
	return sp.Data, nil
}

func (b *Bot) Transcribe(ctx context.Context, audio []byte, fileName, mime string) (string, error) {
	if b.voice == nil {
		return "", fmt.Errorf("voice is not configured")
	}
	return b.voice.Transcribe(ctx, audio, fileName, mime)
}

// Finished records the call in chat history and Walrus memory, or falls back to a text
// when nobody picked up.
func (b *Bot) Finished(s *calls.Session, outcome string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if outcome != "completed" {
		text := s.FallbackText
		if text == "" {
			text = "tried to call you just now but couldn't get through. it's nothing urgent, i'm here whenever."
		}
		reason := map[string]string{"declined": "you declined", "busy": "your line was busy", "failed": "the call wouldn't connect"}[outcome]
		if reason == "" {
			reason = "no answer"
		}
		b.sendSessionText(ctx, s, fmt.Sprintf("📞 %s. %s", reason, text))
		return
	}

	var heard []string
	var transcript strings.Builder
	for _, t := range s.Turns() {
		who := s.Name
		if t.Role == "jasmine" {
			who = "jasmine"
		} else {
			heard = append(heard, t.Text)
		}
		fmt.Fprintf(&transcript, "%s: %s\n", who, t.Text)
	}
	if b.memory != nil && b.api != nil {
		_ = b.memory.SaveMessage(ctx, s.ChatID, b.api.Self.ID, b.api.Self.UserName, "assistant",
			fmt.Sprintf("(voice call with %s about %s)\n%s", s.Name, s.Purpose, ai.TruncateRunes(transcript.String(), 1500)))
	}
	// What they said on the phone goes into long-term memory like anything they type.
	if b.walmem != nil && len(heard) > 0 && s.UserID != 0 {
		input := fmt.Sprintf("On a voice call with Jasmine, @%s said: %s", nonEmpty(s.Username, s.Name), strings.Join(heard, " / "))
		if _, err := b.walmem.Analyze(ctx, walmem.UserNamespace(s.UserID), input); err != nil {
			log.Printf("[Walrus] call analyze failed: %v", err)
		}
	}
}

func (b *Bot) sendSessionText(ctx context.Context, s *calls.Session, text string) {
	if s.ThreadID != 0 {
		ctx = context.WithValue(ctx, ctxKeyThreadID{}, s.ThreadID)
	}
	b.sendReplyCtx(ctx, s.ChatID, s.ReplyTo, text)
}

func channelLabel(ch string) string {
	if ch == calls.ChannelPhone {
		return "phone"
	}
	return "Telegram"
}

// ---------- tools ----------

func (b *Bot) executeCallTool(ctx context.Context, chatID int64, toolName, arguments, username string, isOwner bool) (string, bool) {
	switch toolName {
	case "call_user", "save_phone_number", "forget_phone_number":
	default:
		return "", false
	}
	senderID, _ := ctx.Value(ctxKeySenderID{}).(int64)
	if senderID == 0 {
		return "Unknown user.", true
	}
	var args struct {
		Target string `json:"target_username"`
		Via    string `json:"via"`
		Reason string `json:"reason"`
		Phone  string `json:"phone"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)

	if b.phonebook == nil && toolName != "call_user" {
		return "Phone numbers can't be saved right now.", true
	}
	switch toolName {
	case "save_phone_number":
		phone, err := calls.NormalizePhone(args.Phone, phoneCountry())
		if err != nil {
			return fmt.Sprintf("%v. Ask them for the full number with country code.", err), true
		}
		if err := b.phonebook.Set(ctx, senderID, username, phone); err != nil {
			return fmt.Sprintf("Couldn't save it: %v", err), true
		}
		note := ""
		if chatID < 0 {
			note = " They shared it in a group, so gently suggest they delete that message."
		}
		return fmt.Sprintf("Saved their number (ends %s). Confirm briefly without repeating the full number.%s", calls.MaskPhone(phone), note), true
	case "forget_phone_number":
		if err := b.phonebook.Delete(ctx, senderID); err != nil {
			return fmt.Sprintf("Couldn't delete it: %v", err), true
		}
		return "Deleted their phone number.", true
	}

	// call_user
	phoneOK, tgOK := b.calls.Available()
	if !phoneOK && !tgOK {
		return "Calling isn't set up yet. Tell them honestly you can't call right now, and offer a voice note instead.", true
	}
	target := strings.TrimPrefix(strings.TrimSpace(args.Target), "@")
	self := target == "" || strings.EqualFold(target, username) || strings.EqualFold(target, "me")
	if !self && !isOwner {
		return "Only your creator can ask you to call other people. Decline kindly; they can ask you to call them instead.", true
	}
	if !isOwner && !b.calls.AllowCall(senderID, callsPerDay) {
		return "They've hit today's call limit. Say so kindly and offer a voice note.", true
	}

	firstName, _ := ctx.Value(ctxKeySenderFirstName{}).(string)
	s := &calls.Session{
		ChatID: chatID, UserID: senderID, Username: username, Name: nonEmpty(firstName, username),
		Purpose: nonEmpty(args.Reason, "they asked you to call them, so just check in and chat"),
	}
	s.ThreadID, _ = ctx.Value(ctxKeyThreadID{}).(int)
	s.ReplyTo = replyTarget(ctx)
	if !self {
		// "call skipp" -> @skipp_dev: match the name against people she has seen.
		resolved, id := b.resolvePerson(ctx, chatID, target)
		if resolved != "" {
			target = resolved
		}
		s.UserID, s.Username, s.Name, s.Requester = id, target, target, username
	}

	via := strings.ToLower(args.Via)
	if via != calls.ChannelPhone && via != calls.ChannelTelegram {
		via = calls.ChannelTelegram
		if !tgOK {
			via = calls.ChannelPhone
		}
	}
	var to string
	switch via {
	case calls.ChannelPhone:
		if !phoneOK {
			return "Phone calls aren't set up; offer a Telegram call or voice note instead.", true
		}
		var phone string
		if self {
			phone, _ = b.phonebook.Get(ctx, senderID)
		} else {
			s.UserID, phone, _ = b.phonebook.LookupByUsername(ctx, target)
		}
		if phone == "" {
			return "No phone number saved for them. Ask them to DM you their number (with country code) first.", true
		}
		to = phone
	case calls.ChannelTelegram:
		if !tgOK {
			return "Telegram calls aren't set up; offer a phone call or voice note instead.", true
		}
		if self {
			to = calls.TelegramTarget(username, senderID)
		} else {
			to = "@" + target
		}
	}
	s.Channel = via
	if s.UserID != 0 && b.walmem != nil {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if mems, err := b.walmem.Recall(rctx, walmem.UserNamespace(s.UserID), s.Purpose, 5); err == nil {
			s.Memories = walmem.FormatForPrompt(mems, memoryRelevanceCutoff)
		}
		cancel()
	}
	if err := b.calls.Start(ctx, s, to); err != nil {
		log.Printf("[Calls] start failed: %v", err)
		return fmt.Sprintf("The call couldn't be placed (%v). Tell them plainly and offer a voice note.", err), true
	}
	return "Calling them now. Reply with at most a few words (like 'ringing you now') or NO_REPLY.", true
}

func phoneCountry() string {
	if c := os.Getenv("DEFAULT_PHONE_COUNTRY"); c != "" {
		return strings.TrimPrefix(c, "+")
	}
	return "234"
}

var personKeyRe = regexp.MustCompile(`[^a-z0-9]`)

// resolvePerson turns a name someone typed ("skipp", "@Skipp_Dev") into the exact
// username of someone Jasmine has talked to, preferring people in the current chat.
func (b *Bot) resolvePerson(ctx context.Context, chatID int64, name string) (string, int64) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if want == "" || b.memory == nil {
		return "", 0
	}
	if id, err := b.memory.GetUserIDByUsername(ctx, want); err == nil && id != 0 {
		return want, id
	}
	key := personKeyRe.ReplaceAllString(want, "")
	if key == "" {
		return "", 0
	}
	if db := b.memory.GetDB(); db != nil {
		var uname string
		var id int64
		err := db.QueryRowContext(ctx, `
			SELECT sender_username, MAX(sender_id)
			FROM chat_messages
			WHERE role = 'user' AND COALESCE(sender_username, '') <> ''
			  AND regexp_replace(LOWER(sender_username), '[^a-z0-9]', '', 'g') LIKE '%' || $1 || '%'
			GROUP BY sender_username
			ORDER BY MAX(CASE WHEN chat_id = $2 THEN 1 ELSE 0 END) DESC, COUNT(*) DESC
			LIMIT 1`, key, chatID).Scan(&uname, &id)
		if err == nil {
			return strings.ToLower(uname), id
		}
		return "", 0
	}
	history, _ := b.memory.GetRecentMessages(ctx, chatID, 200)
	for i := len(history) - 1; i >= 0; i-- {
		sender := strings.ToLower(history[i].Sender)
		if history[i].Role == "user" && sender != "" && strings.Contains(personKeyRe.ReplaceAllString(sender, ""), key) {
			id, _ := b.memory.GetUserIDByUsername(ctx, sender)
			return sender, id
		}
	}
	return "", 0
}
