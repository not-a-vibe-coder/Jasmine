package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/calls"
	"shipp/internal/reminders"
)

// SetReminders enables "remind me at 3pm to ..." scheduling.
func (b *Bot) SetReminders(s *reminders.Store) { b.reminders = s }

// currentTimeContext tells the model what time it is, so "by 3pm" and "tomorrow" resolve.
func currentTimeContext(now time.Time) string {
	lagos := reminders.LoadZone(reminders.DefaultZone)
	return fmt.Sprintf("\n- CURRENT TIME: %s in %s (%s). UTC: %s.",
		now.In(lagos).Format("Monday 2 January 2006, 15:04"), reminders.DefaultZone, now.In(lagos).Format("MST"),
		now.UTC().Format("15:04"))
}

func (b *Bot) executeReminderTool(ctx context.Context, chatID int64, toolName, arguments, username string) (string, bool) {
	switch toolName {
	case "set_reminder", "list_reminders", "cancel_reminder":
	default:
		return "", false
	}
	if b.reminders == nil {
		return "Reminders are not available right now. Tell them honestly you can't set it at the moment.", true
	}
	senderID, _ := ctx.Value(ctxKeySenderID{}).(int64)
	if senderID == 0 {
		return "Unknown user.", true
	}
	var args struct {
		Message   string  `json:"message"`
		At        string  `json:"at"`
		InMinutes float64 `json:"in_minutes"`
		Timezone  string  `json:"timezone"`
		Repeat    string  `json:"repeat"`
		Target    string  `json:"target_username"`
		DeliverBy string  `json:"deliver_by"`
		ID        int64   `json:"id"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)
	now := time.Now()

	switch toolName {
	case "list_reminders":
		list, err := b.reminders.Pending(ctx, senderID)
		if err != nil {
			return fmt.Sprintf("Couldn't load reminders: %v", err), true
		}
		if len(list) == 0 {
			return "They have no upcoming reminders.", true
		}
		var sb strings.Builder
		for _, r := range list {
			fmt.Fprintf(&sb, "#%d: %q, %s", r.ID, r.Text, reminders.Describe(r.DueAt, reminders.LoadZone(r.Timezone), now))
			if r.Repeat != "" {
				sb.WriteString(" (repeats " + r.Repeat + ")")
			}
			sb.WriteString("\n")
		}
		return "Their upcoming reminders (mention them naturally, not as a table):\n" + sb.String(), true

	case "cancel_reminder":
		ok, err := b.reminders.Cancel(ctx, senderID, args.ID)
		if err != nil {
			return fmt.Sprintf("Couldn't cancel: %v", err), true
		}
		if !ok {
			return "No matching reminder of theirs. Call list_reminders to find the right id.", true
		}
		return "Cancelled. Confirm in a few words.", true
	}

	// set_reminder
	text := strings.TrimSpace(args.Message)
	if text == "" {
		return "What should the reminder say? Ask them.", true
	}
	loc := reminders.LoadZone(args.Timezone)
	due, err := reminders.ResolveTime(args.At, args.InMinutes, loc, now)
	if err != nil {
		return fmt.Sprintf("%v. Ask them when exactly, or retry with a valid time.", err), true
	}
	if !due.After(now) {
		return "That time has already passed. Ask them if they meant tomorrow.", true
	}
	repeat := strings.ToLower(strings.TrimSpace(args.Repeat))
	if repeat != "daily" && repeat != "weekly" {
		repeat = ""
	}
	deliverBy := strings.ToLower(strings.TrimSpace(args.DeliverBy))
	switch deliverBy {
	case "voice_note", "telegram_call", "phone_call":
	default:
		deliverBy = ""
	}
	if deliverBy == "phone_call" && b.phonebook != nil {
		if phone, _ := b.phonebook.Get(ctx, senderID); phone == "" {
			return "They want a phone call but you don't have their number. Ask them to DM it to you (with country code), or offer a Telegram call instead.", true
		}
	}
	threadID, _ := ctx.Value(ctxKeyThreadID{}).(int)
	replyTo, _ := ctx.Value(ctxKeyReplyToMsgID{}).(int)
	firstName, _ := ctx.Value(ctxKeySenderFirstName{}).(string)
	r := &reminders.Reminder{
		ChatID: chatID, ThreadID: threadID, UserID: senderID, Username: username, FirstName: firstName,
		Target: strings.TrimPrefix(strings.TrimSpace(args.Target), "@"), ReplyTo: replyTo,
		Text: text, DueAt: due, Timezone: loc.String(), Repeat: repeat, DeliverBy: deliverBy,
	}
	if strings.EqualFold(r.Target, username) || strings.EqualFold(r.Target, "me") {
		r.Target = ""
	}
	if err := b.reminders.Add(ctx, r); err != nil {
		return fmt.Sprintf("Couldn't save the reminder: %v", err), true
	}
	log.Printf("[Reminder] #%d for @%s at %s: %q", r.ID, username, due.Format(time.RFC3339), text)
	when := reminders.Describe(due, loc, now)
	return fmt.Sprintf("Reminder #%d saved for %s. Confirm warmly in one short line that mentions the time (%s), e.g. a reaction plus 'got you, i'll nudge you at 3pm'. Do not repeat the id.", r.ID, when, when), true
}

// runReminderLoop delivers due reminders. Reminders missed while the service slept are
// delivered as soon as it wakes.
func (b *Bot) runReminderLoop(ctx context.Context) {
	if b.reminders == nil {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	lastPing := time.Time{}
	for {
		b.deliverDueReminders(ctx)
		if time.Since(lastPing) > 10*time.Minute {
			b.keepAwakeForReminders(ctx)
			lastPing = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *Bot) deliverDueReminders(ctx context.Context) {
	now := time.Now()
	due, err := b.reminders.Due(ctx, now)
	if err != nil {
		log.Printf("[Reminder] loading due reminders failed: %v", err)
		return
	}
	for _, r := range due {
		if err := b.deliverReminder(ctx, r, now); err != nil {
			log.Printf("[Reminder] #%d delivery failed: %v", r.ID, err)
			// Give up on reminders Telegram keeps rejecting (bot removed, chat gone).
			if now.Sub(r.DueAt) < 24*time.Hour {
				continue
			}
		}
		if err := b.reminders.Complete(ctx, r, now); err != nil {
			log.Printf("[Reminder] #%d complete failed: %v", r.ID, err)
		}
	}
}

func (b *Bot) deliverReminder(ctx context.Context, r *reminders.Reminder, now time.Time) error {
	if b.api == nil {
		return fmt.Errorf("telegram api not initialised")
	}
	name := r.FirstName
	if r.Target != "" {
		name = r.Target
	} else if name == "" {
		name = r.Username
	}
	late := now.Sub(r.DueAt)
	loc := reminders.LoadZone(r.Timezone)

	body := ""
	if b.ai != nil {
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		body, _ = b.ai.ComposeReminder(cctx, name, r.Text, r.DueAt.In(loc).Format("3:04pm"), r.Target != "" && r.Username != "", r.Username, late)
		cancel()
	}
	body = b.cleanOutgoingText(body)
	if strings.TrimSpace(body) == "" || body == NoReply {
		body = fmt.Sprintf("hey %s, it's time: %s", name, r.Text)
	}
	if b.deliverReminderSpecial(ctx, r, name, body) {
		return nil
	}

	// Mention so Telegram notifies them, even in a busy group.
	text := html.EscapeString(body)
	if r.ChatID < 0 {
		mention := fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, r.UserID, html.EscapeString(nonEmpty(r.FirstName, r.Username, "you")))
		if r.Target != "" {
			mention = "@" + html.EscapeString(r.Target)
		}
		text = mention + " " + text
	}

	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", r.ChatID)
	params["text"] = text
	params["parse_mode"] = "HTML"
	params.AddNonZero("message_thread_id", r.ThreadID)
	if r.ChatID < 0 && r.ReplyTo > 0 {
		params.AddNonZero("reply_to_message_id", r.ReplyTo)
		params["allow_sending_without_reply"] = "true"
	}
	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil {
		return err
	}
	var sent struct {
		MessageID int `json:"message_id"`
	}
	if resp != nil && json.Unmarshal(resp.Result, &sent) == nil && sent.MessageID > 0 {
		b.setLastBotMessageID(r.ChatID, sent.MessageID)
	}
	_ = b.memory.SaveMessage(ctx, r.ChatID, b.api.Self.ID, b.api.Self.UserName, "assistant", body)
	log.Printf("[Reminder] #%d delivered to chat %d (%s late)", r.ID, r.ChatID, late.Round(time.Second))
	return nil
}

// keepAwakeForReminders pings our own public URL while reminders are pending, because
// Render's free plan sleeps after 15 idle minutes and a sleeping service can't remind anyone.
func (b *Bot) keepAwakeForReminders(ctx context.Context) {
	base := strings.TrimRight(os.Getenv("RENDER_EXTERNAL_URL"), "/")
	if base == "" {
		return
	}
	pending, err := b.reminders.Pending(ctx, 0)
	if err != nil || len(pending) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// deliverReminderSpecial handles voice-note and call reminders. It returns false to fall
// back to a normal text reminder.
func (b *Bot) deliverReminderSpecial(ctx context.Context, r *reminders.Reminder, name, body string) bool {
	if r.Target != "" {
		return false // calls and voice notes only go to the person who asked
	}
	switch r.DeliverBy {
	case "voice_note":
		vctx := ctx
		if r.ThreadID != 0 {
			vctx = context.WithValue(ctx, ctxKeyThreadID{}, r.ThreadID)
		}
		if err := b.sendVoiceNote(vctx, r.ChatID, r.ReplyTo, body); err != nil {
			log.Printf("[Reminder] #%d voice note failed, sending text: %v", r.ID, err)
			return false
		}
		log.Printf("[Reminder] #%d delivered as a voice note", r.ID)
		return true

	case "telegram_call", "phone_call":
		if b.calls == nil {
			return false
		}
		s := &calls.Session{
			ChatID: r.ChatID, ThreadID: r.ThreadID, ReplyTo: r.ReplyTo, UserID: r.UserID,
			Username: r.Username, Name: name,
			Purpose:      "they asked you to remind them: " + r.Text,
			FallbackText: body,
		}
		to := calls.TelegramTarget(r.Username, r.UserID)
		s.Channel = calls.ChannelTelegram
		if r.DeliverBy == "phone_call" {
			s.Channel = calls.ChannelPhone
			if b.phonebook == nil {
				return false
			}
			phone, _ := b.phonebook.Get(ctx, r.UserID)
			if phone == "" {
				return false
			}
			to = phone
		}
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		if err := b.calls.Start(cctx, s, to); err != nil {
			log.Printf("[Reminder] #%d call failed, sending text: %v", r.ID, err)
			return false
		}
		log.Printf("[Reminder] #%d delivered as a %s", r.ID, r.DeliverBy)
		return true // if they don't pick up, the call reports back with the text
	}
	return false
}
