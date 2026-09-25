package bot

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shipp/internal/config"
	"shipp/internal/crypto"
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
		"", "", "", "",
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
