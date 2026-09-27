package ai

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDateQuery(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	tests := []struct {
		input string
		want  string
	}{
		{"", today},
		{"today", today},
		{"yesterday", yesterday},
		{"2026-09-27", "2026-09-27"},
		{"27-09-2026", "2026-09-27"},
		{"27/09/2026", "2026-09-27"},
		{"270926", "2026-09-27"},
	}

	for _, tt := range tests {
		got, err := ParseDateQuery(tt.input)
		if err != nil {
			t.Errorf("ParseDateQuery(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDateQuery(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestTokenTrackerPersistenceAndReports(t *testing.T) {
	tmpFile := "test_tokens_usage.json"
	defer os.Remove(tmpFile)

	tracker1 := NewTokenTracker(tmpFile)
	tracker1.Record("qwen/qwen3.8-27b", 100, 50)
	tracker1.Record("gemini-flash-latest", 200, 80)

	// Report today
	repToday := tracker1.FormatReport()
	if !strings.Contains(repToday, "total tokens: 430") {
		t.Errorf("expected 430 total tokens in today report, got: %s", repToday)
	}
	if !strings.Contains(repToday, "qwen/qwen3.8-27b") || !strings.Contains(repToday, "gemini-flash-latest") {
		t.Errorf("expected models in report, got: %s", repToday)
	}

	// Create new tracker instance pointing to same file - should restore data!
	tracker2 := NewTokenTracker(tmpFile)
	repRestored := tracker2.FormatReport("today")
	if !strings.Contains(repRestored, "total tokens: 430") {
		t.Errorf("expected restored tracker to have 430 tokens, got: %s", repRestored)
	}

	// History report
	repHistory := tracker2.FormatReport("history")
	if !strings.Contains(repHistory, "430 tokens") {
		t.Errorf("expected history report to list 430 tokens, got: %s", repHistory)
	}

	// Yesterday report (none recorded yet)
	repYesterday := tracker2.FormatReport("yesterday")
	if !strings.Contains(repYesterday, "no requests recorded on this date") {
		t.Errorf("expected no requests on yesterday, got: %s", repYesterday)
	}
}
