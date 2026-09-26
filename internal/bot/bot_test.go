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

func TestIsBalanceIntent(t *testing.T) {
	tests := []struct {
		prompt    string
		wantOK    bool
		wantChain string
	}{
		{"How much you got?", true, "all"},
		{"how much do you have", true, "all"},
		{"what's your balance?", true, "all"},
		{"check wallet", true, "all"},
		{"how much sol do you have", true, "solana"},
		{"check your base balance", true, "base"},
		{"what's your robinhood balance", true, "robinhood"},
		{"how do i balance a binary tree", false, ""},
		{"show me the balance sheet", false, ""},
		{"what are we cooking today?", false, ""},
	}

	for _, tt := range tests {
		ok, chain := isBalanceIntent(tt.prompt)
		if ok != tt.wantOK || chain != tt.wantChain {
			t.Errorf("isBalanceIntent(%q) = (%v, %q); want (%v, %q)", tt.prompt, ok, chain, tt.wantOK, tt.wantChain)
		}
	}
}

func TestDeduplicateResponse(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{
			input:    "need the chain, amount and your wallet address to fire it off. - need the chain, amount and your wallet address to fire it off.",
			expected: "need the chain, amount and your wallet address to fire it off.",
		},
		{
			input:    "need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it.need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it.",
			expected: "need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it.",
		},
		{
			input:    "need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it. need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it.",
			expected: "need the chain (solana, base, etc.), the exact amount you want, and the destination address to send it.",
		},
		{
			input:    "got it. need the chain and amount. need the chain and amount.",
			expected: "got it. need the chain and amount.",
		},
		{
			input:    "sitting on about $1.30 on robinhood right now (0.0004835 ETH). rest of the chains are dry.",
			expected: "sitting on about $1.30 on robinhood right now (0.0004835 ETH). rest of the chains are dry.",
		},
		{
			input:    "yeah anon\nyeah anon",
			expected: "yeah anon",
		},
		{
			input:    "we are cooking. we are cooking.",
			expected: "we are cooking.",
		},
		{
			input:    "all good. no worries. all good. no worries.",
			expected: "all good. no worries.",
		},
	}

	for _, c := range cases {
		got := deduplicateResponse(c.input)
		if got != c.expected {
			t.Errorf("deduplicateResponse(%q)\n  got:      %q\n  expected: %q", c.input, got, c.expected)
		}
	}
}

func TestCleanNoEmojisWithDeduplication(t *testing.T) {
	input := "need the chain, amount and your wallet address to fire it off. — need the chain, amount and your wallet address to fire it off."
	expected := "need the chain, amount and your wallet address to fire it off."
	got := cleanNoEmojis(input)
	if got != expected {
		t.Errorf("cleanNoEmojis(%q) = %q; want %q", input, got, expected)
	}
}

func TestFormatEmergencyBalanceFallback(t *testing.T) {
	rawJSON := `{"total_usd_value":"$1.30","active_holdings":[{"chain":"robinhood","token":"ETH","amount":"0.0004835","usd":"$1.30"}],"dry_chains":["solana","base","ethereum","arbitrum","bnb"]}`
	res := formatEmergencyBalanceFallback(rawJSON)
	if !strings.Contains(res, "robinhood") || !strings.Contains(res, "$1.30") {
		t.Errorf("expected emergency fallback to mention robinhood and $1.30, got: %s", res)
	}
}

func TestCleanNoEmojisEagerScrubber(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{
			input:    "latest action run succeeded on main. what's next?",
			expected: "latest action run succeeded on main",
		},
		{
			input:    "action run succeeded, green across the board. what are we building next?",
			expected: "action run succeeded, green across the board",
		},
		{
			input:    "TeraWallet mcap is $36.19K. what's the next move?",
			expected: "TeraWallet mcap is $36.19K",
		},
		{
			input:    "all eyes anon, what's the move?",
			expected: "all eyes anon",
		},
		{
			input:    "done and dusted, what are we cooking?",
			expected: "done and dusted",
		},
	}

	for _, c := range cases {
		got := cleanNoEmojis(c.input)
		// Trailing periods may be conditionally trimmed
		gotTrimmed := strings.TrimSuffix(got, ".")
		wantTrimmed := strings.TrimSuffix(c.expected, ".")
		if gotTrimmed != wantTrimmed {
			t.Errorf("cleanNoEmojis(%q)\n  got:      %q\n  expected: %q", c.input, got, c.expected)
		}
	}
}
