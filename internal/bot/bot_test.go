package bot

import (
	"strings"
	"testing"

	"shipp/internal/config"
	"shipp/internal/crypto"
)

func TestCleanPrompt(t *testing.T) {
	b := &Bot{}
	// Mock username
	b.api = nil

	tests := []struct {
		input    string
		expected string
	}{
		{"Shipp, what is bitcoin?", "what is bitcoin?"},
		{"Shipp: check balance", "check balance"},
		{"shipp hello world", "hello world"},
		{"just a regular message", "just a regular message"},
	}

	for _, tt := range tests {
		result := b.cleanPrompt(tt.input)
		if result != tt.expected {
			t.Errorf("cleanPrompt(%q) = %q; want %q", tt.input, result, tt.expected)
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
