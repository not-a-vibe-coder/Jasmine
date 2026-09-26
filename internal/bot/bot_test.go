package bot

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/config"
	"shipp/internal/crypto"
	"shipp/internal/memory"
)

func TestCleanPrompt(t *testing.T) {
	b := &Bot{}

	tests := []struct {
		input    string
		expected string
	}{
		{"Shipp, what is bitcoin?", "what is bitcoin"},
		{"Shipp: check balance", "check balance"},
		{"shipp hello world", "hello world"},
		{"Hi shipp", "Hi"},
		{"What's up shipp", "What's up"},
		{"Shipp", "Shipp"},
		{"just a regular message", "just a regular message"},
	}

	for _, tt := range tests {
		result := b.cleanPrompt(tt.input)
		if result != tt.expected {
			t.Errorf("cleanPrompt(%q) = %q; want %q", tt.input, result, tt.expected)
		}
	}
}

func TestIsAddressedToBot(t *testing.T) {
	b := &Bot{}

	tests := []struct {
		text     string
		expected bool
	}{
		{"Hi shipp", true},
		{"What's up shipp", true},
		{"yo shipp check this out", true},
		{"Shipp, what's bitcoin?", true},
		{"we are shipping a new release today", false}, // "shipping" does not trigger
		{"their friendship is great", false},            // "friendship" does not trigger
		{"just talking to someone else", false},
	}

	for _, tt := range tests {
		msg := &tgbotapi.Message{Text: tt.text}
		result := b.isAddressedToBot(msg)
		if result != tt.expected {
			t.Errorf("isAddressedToBot(%q) = %v; want %v", tt.text, result, tt.expected)
		}
	}
}

func TestFormatWalletAddressMessage(t *testing.T) {
	cryptoSvc, err := crypto.NewService(
		"HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr", "", "", "",
		"0x0a2e799d0b57217a1066a4CDD132F01215E132b8", "",
		"", "", "", "", "",
	)
	if err != nil {
		t.Fatalf("crypto init failed: %v", err)
	}

	b := &Bot{
		crypto: cryptoSvc,
		cfg:    &config.Config{Owners: []string{"skipp_dev"}},
	}

	msg := b.formatWalletAddressMessage()
	if !strings.Contains(msg, "HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr") {
		t.Errorf("expected wallet message to contain Solana address")
	}
	if !strings.Contains(msg, "0x0a2e799d0b57217a1066a4CDD132F01215E132b8") {
		t.Errorf("expected wallet message to contain EVM address")
	}
}

func TestCleanNoEmojisAndEmDashes(t *testing.T) {
	input := "🚀 Pushed straight to main — here's the new description: ✨"
	expected := "Pushed straight to main - here's the new description:"
	res := cleanNoEmojis(input)
	if res != expected {
		t.Errorf("cleanNoEmojis(%q) = %q; want %q", input, res, expected)
	}

	// Test en dash
	inputEn := "Release v1 – latest updates"
	expectedEn := "Release v1 - latest updates"
	resEn := cleanNoEmojis(inputEn)
	if resEn != expectedEn {
		t.Errorf("cleanNoEmojis(%q) = %q; want %q", inputEn, resEn, expectedEn)
	}
}

func TestExtractRepoFromHistory(t *testing.T) {
	// 1. In prompt
	prompt := "Go to https://github.com/DavidNzube101/shipp and update the description"
	repo := extractRepoFromHistory(nil, prompt)
	if repo != "DavidNzube101/shipp" {
		t.Errorf("extractRepoFromHistory prompt = %q; want DavidNzube101/shipp", repo)
	}

	// 2. In history
	history := []memory.Message{
		{Role: "user", Content: "Go to https://github.com/DavidNzube101/shipp and check the readme"},
		{Role: "assistant", Content: "Got the repo pulled up."},
	}
	repoFromHist := extractRepoFromHistory(history, "Rephrase it and push to main straight")
	if repoFromHist != "DavidNzube101/shipp" {
		t.Errorf("extractRepoFromHistory history = %q; want DavidNzube101/shipp", repoFromHist)
	}

	// 3. Slug in history
	historySlug := []memory.Message{
		{Role: "user", Content: "check davidnzube101/shipp commits"},
		{Role: "assistant", Content: "Here is the latest commit."},
	}
	repoSlug := extractRepoFromHistory(historySlug, "update it and push to main")
	if repoSlug != "davidnzube101/shipp" {
		t.Errorf("extractRepoFromHistory slug = %q; want davidnzube101/shipp", repoSlug)
	}
}

func TestExtractPRNumber(t *testing.T) {
	prompt := "merge PR #12"
	prNum := extractPRNumber(nil, prompt)
	if prNum != 12 {
		t.Errorf("extractPRNumber(%q) = %d; want 12", prompt, prNum)
	}

	history := []memory.Message{
		{Role: "assistant", Content: "Opened PR #7 on repo. Say 'merge it' whenever you're ready."},
	}
	prFromHist := extractPRNumber(history, "merge it")
	if prFromHist != 7 {
		t.Errorf("extractPRNumber history = %d; want 7", prFromHist)
	}
}
