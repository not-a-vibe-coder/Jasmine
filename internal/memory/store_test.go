package memory

import (
	"context"
	"testing"
)

func TestHybridStoreInMemoryFallback(t *testing.T) {
	// Initialize with empty URLs to trigger in-memory fallback
	store, err := NewHybridStore("", "")
	if err != nil {
		t.Fatalf("unexpected error creating store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	chatID := int64(12345)

	// Test SaveMessage
	err = store.SaveMessage(ctx, chatID, 100, "skipp_dev", "user", "Hello Shipp!")
	if err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}
	err = store.SaveMessage(ctx, chatID, 200, "Shipp0Bot", "assistant", "Yo skipp! What's good?")
	if err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}

	// Test GetRecentMessages
	msgs, err := store.GetRecentMessages(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("GetRecentMessages failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "Hello Shipp!" || msgs[1].Content != "Yo skipp! What's good?" {
		t.Errorf("messages content mismatch: %+v", msgs)
	}

	// Test SaveSummary and GetSummary
	summaryText := "Discussion about crypto bot architecture"
	err = store.SaveSummary(ctx, chatID, summaryText)
	if err != nil {
		t.Fatalf("SaveSummary failed: %v", err)
	}
	retrievedSummary, err := store.GetSummary(ctx, chatID)
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}
	if retrievedSummary != summaryText {
		t.Errorf("expected summary %q, got %q", summaryText, retrievedSummary)
	}

	// Test GetActiveChatIDs
	chatIDs, err := store.GetActiveChatIDs(ctx)
	if err != nil {
		t.Fatalf("GetActiveChatIDs failed: %v", err)
	}
	if len(chatIDs) != 1 || chatIDs[0] != chatID {
		t.Errorf("expected active chat %d, got %v", chatID, chatIDs)
	}

	// Test ClearContext
	err = store.ClearContext(ctx, chatID)
	if err != nil {
		t.Fatalf("ClearContext failed: %v", err)
	}
	clearedMsgs, err := store.GetRecentMessages(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("GetRecentMessages after clear failed: %v", err)
	}
	if len(clearedMsgs) != 0 {
		t.Errorf("expected 0 messages after clear, got %d", len(clearedMsgs))
	}
	clearedSummary, _ := store.GetSummary(ctx, chatID)
	if clearedSummary != "" {
		t.Errorf("expected empty summary after clear, got %q", clearedSummary)
	}

	// Test SaveUserProfile & GetUserProfile
	profile := UserProfile{
		ChatID:         chatID,
		Preferences:    "prefers concise code, direct commits over PRs",
		ActiveProjects: "DavidNzube101/shipp",
		LifeContext:    "CS student at FUTO, 300 level",
	}
	if err := store.SaveUserProfile(ctx, profile); err != nil {
		t.Fatalf("SaveUserProfile failed: %v", err)
	}
	retrievedProfile, err := store.GetUserProfile(ctx, chatID)
	if err != nil {
		t.Fatalf("GetUserProfile failed: %v", err)
	}
	if retrievedProfile == nil {
		t.Fatalf("expected profile, got nil")
	}
	if retrievedProfile.ActiveProjects != "DavidNzube101/shipp" || retrievedProfile.LifeContext != "CS student at FUTO, 300 level" {
		t.Errorf("profile mismatch: %+v", retrievedProfile)
	}

	// Test GetUserIDByUsername
	uid, err := store.GetUserIDByUsername(ctx, "skipp_dev")
	if err != nil || uid != 100 {
		t.Errorf("expected uid 100 for skipp_dev, got %d (err: %v)", uid, err)
	}
	uidAt, err := store.GetUserIDByUsername(ctx, "@skipp_dev")
	if err != nil || uidAt != 100 {
		t.Errorf("expected uid 100 for @skipp_dev, got %d (err: %v)", uidAt, err)
	}
	_, errNotFound := store.GetUserIDByUsername(ctx, "nonexistent_user")
	if errNotFound == nil {
		t.Errorf("expected error for nonexistent user")
	}
}

func TestFindMessagesBySender(t *testing.T) {
	store, _ := NewHybridStore("", "")
	defer store.Close()
	ctx := context.Background()

	_ = store.SaveMessage(ctx, 100, 10, "precidobaby", "user", "hey shipp what is up")
	_ = store.SaveMessage(ctx, 200, 20, "skipp_dev", "user", "gm shipp")
	_ = store.SaveMessage(ctx, 100, 10, "precidobaby", "user", "are you free?")

	found, err := store.FindMessagesBySender(ctx, "precidobaby", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 messages from precidobaby, got %d", len(found))
	}
	if found[0].Content != "are you free?" {
		t.Errorf("expected newest message first, got: %s", found[0].Content)
	}

	// Not found user
	notFound, err := store.FindMessagesBySender(ctx, "nobody_here", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notFound) != 0 {
		t.Errorf("expected 0 messages for unknown user, got %d", len(notFound))
	}
}

func TestMoltbookMemory(t *testing.T) {
	store, _ := NewHybridStore("", "")
	defer store.Close()
	ctx := context.Background()

	mem1 := MoltbookMemory{
		PostID:    "post-1",
		PostTitle: "A skill dependency is an execution path",
		Author:    "neo_konsi",
		Content:   "Skill dependencies have operational authority over agents.",
		Tags:      "security,skills,permissions",
		Upvotes:   184,
	}
	mem2 := MoltbookMemory{
		PostID:    "post-2",
		PostTitle: "Determinism from statelessness",
		Author:    "juan_carlos",
		Content:   "Budget must be measured in reach and blast radius, not just time.",
		Tags:      "sandboxes,reach,security",
		Upvotes:   42,
	}

	if err := store.SaveMoltbookMemory(ctx, mem1); err != nil {
		t.Fatalf("SaveMoltbookMemory failed: %v", err)
	}
	if err := store.SaveMoltbookMemory(ctx, mem2); err != nil {
		t.Fatalf("SaveMoltbookMemory failed: %v", err)
	}

	// Retrieve
	all, err := store.GetMoltbookMemories(ctx, 10)
	if err != nil {
		t.Fatalf("GetMoltbookMemories failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(all))
	}
	if all[0].PostID != "post-2" {
		t.Errorf("expected newest memory first, got: %s", all[0].PostID)
	}

	// Search
	results, err := store.SearchMoltbookMemories(ctx, "blast radius", 5)
	if err != nil {
		t.Fatalf("SearchMoltbookMemories failed: %v", err)
	}
	if len(results) != 1 || results[0].Author != "juan_carlos" {
		t.Errorf("expected search match for juan_carlos, got %+v", results)
	}

	tagResults, err := store.SearchMoltbookMemories(ctx, "skills", 5)
	if err != nil {
		t.Fatalf("SearchMoltbookMemories by tag failed: %v", err)
	}
	if len(tagResults) != 1 || tagResults[0].PostID != "post-1" {
		t.Errorf("expected tag match for post-1, got %+v", tagResults)
	}
}

