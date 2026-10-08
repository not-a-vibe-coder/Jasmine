package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"shipp/internal/reminders"
)

func TestReminderTools(t *testing.T) {
	store, _ := reminders.New(context.Background(), nil)
	b := &Bot{}
	b.SetReminders(store)
	ctx := context.WithValue(context.Background(), ctxKeySenderID{}, int64(42))
	ctx = context.WithValue(ctx, ctxKeySenderFirstName{}, "Skipp")
	ctx = context.WithValue(ctx, ctxKeyReplyToMsgID{}, 77)

	res, ok := b.executeReminderTool(ctx, -100, "set_reminder", `{"message":"fetch water","in_minutes":30}`, "skipp_dev")
	if !ok || !strings.Contains(res, "saved") {
		t.Fatalf("set_reminder: %v %q", ok, res)
	}
	list, _ := store.Pending(ctx, 42)
	if len(list) != 1 || list[0].ChatID != -100 || list[0].ReplyTo != 77 || list[0].FirstName != "Skipp" || list[0].Timezone != "Africa/Lagos" {
		t.Fatalf("stored reminder wrong: %+v", list)
	}
	if d := time.Until(list[0].DueAt); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("due in %v, want ~30m", d)
	}

	if res, _ := b.executeReminderTool(ctx, -100, "list_reminders", `{}`, "skipp_dev"); !strings.Contains(res, "fetch water") {
		t.Fatalf("list_reminders: %q", res)
	}
	if res, _ := b.executeReminderTool(ctx, -100, "set_reminder", `{"message":"x","at":"someday"}`, "skipp_dev"); strings.Contains(res, "saved") {
		t.Fatalf("unreadable time must not save: %q", res)
	}
	if _, ok := b.executeReminderTool(ctx, -100, "send_gif", `{}`, "skipp_dev"); ok {
		t.Fatal("non-reminder tools must pass through")
	}
	if !strings.Contains(currentTimeContext(time.Now()), "Africa/Lagos") {
		t.Fatal("time context should name the default zone")
	}
}
