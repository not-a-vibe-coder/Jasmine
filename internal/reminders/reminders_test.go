package reminders

import (
	"context"
	"testing"
	"time"
)

func TestResolveTime(t *testing.T) {
	lagos := LoadZone("lagos time")
	if lagos.String() != "Africa/Lagos" {
		t.Fatalf("alias resolved to %s", lagos)
	}
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, lagos) // 10am Lagos

	got, err := ResolveTime("3pm", 0, lagos, now)
	if err != nil || !got.Equal(time.Date(2026, 10, 8, 15, 0, 0, 0, lagos)) {
		t.Fatalf("3pm today: %v %v", got, err)
	}
	got, _ = ResolveTime("9:30am", 0, lagos, now)
	if !got.Equal(time.Date(2026, 10, 9, 9, 30, 0, 0, lagos)) {
		t.Fatalf("past clock time should roll to tomorrow, got %v", got)
	}
	got, _ = ResolveTime("2026-10-10 18:45", 0, lagos, now)
	if !got.Equal(time.Date(2026, 10, 10, 18, 45, 0, 0, lagos)) {
		t.Fatalf("explicit date: %v", got)
	}
	got, _ = ResolveTime("", 20, lagos, now)
	if !got.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("in 20 minutes: %v", got)
	}
	if _, err := ResolveTime("whenever", 0, lagos, now); err == nil {
		t.Fatal("expected error for unreadable time")
	}
}

func TestDescribe(t *testing.T) {
	lagos := LoadZone("Africa/Lagos")
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, lagos)
	if got := Describe(time.Date(2026, 10, 8, 15, 0, 0, 0, lagos), lagos, now); got != "today at 3pm WAT" {
		t.Errorf("got %q", got)
	}
	if got := Describe(time.Date(2026, 10, 9, 8, 30, 0, 0, lagos), lagos, now); got != "tomorrow at 8:30am WAT" {
		t.Errorf("got %q", got)
	}
}

func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := New(ctx, nil)
	now := time.Now()
	once := &Reminder{ChatID: 1, UserID: 7, Text: "fetch water", DueAt: now.Add(-time.Second)}
	daily := &Reminder{ChatID: 1, UserID: 7, Text: "drink water", DueAt: now.Add(-time.Minute), Repeat: "daily", Timezone: "UTC"}
	later := &Reminder{ChatID: 1, UserID: 8, Text: "call mum", DueAt: now.Add(time.Hour)}
	for _, r := range []*Reminder{once, daily, later} {
		if err := s.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	due, _ := s.Due(ctx, now)
	if len(due) != 2 {
		t.Fatalf("want 2 due, got %d", len(due))
	}
	for _, r := range due {
		_ = s.Complete(ctx, r, now)
	}
	pending, _ := s.Pending(ctx, 7)
	if len(pending) != 1 || pending[0].Text != "drink water" || !pending[0].DueAt.After(now) {
		t.Fatalf("daily reminder should roll forward: %+v", pending)
	}
	if ok, _ := s.Cancel(ctx, 7, later.ID); ok {
		t.Fatal("must not cancel someone else's reminder")
	}
	if ok, _ := s.Cancel(ctx, 8, later.ID); !ok {
		t.Fatal("owner of the reminder should cancel it")
	}
}
