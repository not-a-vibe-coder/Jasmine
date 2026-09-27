package token

import (
	"context"
	"strings"
	"testing"
	"unicode"
)

func hasEmoji(s string) bool {
	for _, r := range s {
		if (r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
			(r >= 0x1F300 && r <= 0x1F5FF) || // Misc Symbols and Pictographs
			(r >= 0x1F680 && r <= 0x1F6FF) || // Transport and Map
			(r >= 0x2600 && r <= 0x26FF) || // Misc symbols
			(r >= 0x2700 && r <= 0x27BF) || // Dingbats
			(r >= 0x1F900 && r <= 0x1F9FF) || // Supplemental Symbols and Pictographs
			(r >= 0x1F1E0 && r <= 0x1F1FF) { // Flags
			return true
		}
		if unicode.In(r, unicode.So) {
			// Other symbols
			return true
		}
	}
	return false
}

func TestAddressValidation(t *testing.T) {
	evmValid := "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	evmInvalid := "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA029"

	if !IsValidEVMAddress(evmValid) {
		t.Errorf("expected valid EVM address: %s", evmValid)
	}
	if IsValidEVMAddress(evmInvalid) {
		t.Errorf("expected invalid EVM address: %s", evmInvalid)
	}

	solValid := "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
	if !IsValidSolanaAddress(solValid) {
		t.Errorf("expected valid Solana address: %s", solValid)
	}
}

func TestExtractAddressAndChain(t *testing.T) {
	input1 := "/ca base 0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	addr1, chain1 := ExtractAddressAndChain(input1)
	if addr1 != "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913" || chain1 != "base" {
		t.Errorf("got addr=%s chain=%s", addr1, chain1)
	}

	input2 := "check this ca DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263 on solana"
	addr2, chain2 := ExtractAddressAndChain(input2)
	if addr2 != "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263" || chain2 != "solana" {
		t.Errorf("got addr=%s chain=%s", addr2, chain2)
	}

	input3 := "/ca rh 0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	addr3, chain3 := ExtractAddressAndChain(input3)
	if addr3 != "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913" || chain3 != "robinhood" {
		t.Errorf("got addr=%s chain=%s", addr3, chain3)
	}
}

func TestFormatCardNoEmojis(t *testing.T) {
	metrics := &TokenMetrics{
		Address:        "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
		Name:           "USD Coin",
		Symbol:         "USDC",
		Chain:          "Base",
		PriceUSD:       0.9999,
		MarketCap:      4305345336,
		LiquidityUSD:   76409091,
		Volume24h:      108711998,
		PriceChange24h: 1.25,
		Buys24h:        6791,
		Sells24h:       6933,
		DexID:          "UNISWAP",
		DexURL:         "https://dexscreener.com/base/0x...",
		HoldersCount:   15234,
		Top10HoldersPct: 14.5,
		OtherChains:    []string{"Ethereum"},
	}

	card := FormatCard(metrics)
	if hasEmoji(card) {
		t.Errorf("FormatCard contains emojis! Output:\n%s", card)
	}

	if !strings.Contains(card, "[$USDC] USD Coin") {
		t.Errorf("expected header in card: %s", card)
	}
	if !strings.Contains(card, "+1.2%") {
		t.Errorf("expected price change in card: %s", card)
	}
	if !strings.Contains(card, "Buys: 6,791 / Sells: 6,933") {
		t.Errorf("expected buys and sells in card: %s", card)
	}

	if strings.Contains(card, "Market data only") {
		t.Errorf("FormatCard still contains disclaimer: %s", card)
	}

	natural := FormatNatural(metrics)
	if hasEmoji(natural) {
		t.Errorf("FormatNatural contains emojis: %s", natural)
	}
	if !strings.Contains(natural, "$USDC") || !strings.Contains(natural, "mcap") {
		t.Errorf("FormatNatural missing expected fields: %s", natural)
	}
	t.Logf("Natural response:\n%s", natural)

	ambiguous := FormatAmbiguousChains(metrics.Address, []string{"Base", "Ethereum"})
	if hasEmoji(ambiguous) {
		t.Errorf("FormatAmbiguousChains contains emojis! Output:\n%s", ambiguous)
	}

	notFound := FormatNotFound(metrics.Address)
	if hasEmoji(notFound) {
		t.Errorf("FormatNotFound contains emojis! Output:\n%s", notFound)
	}
}

func TestDebugDexScreener(t *testing.T) {
	svc := NewService("b1df909457d1fa9fbee8973752409664b076b74d")
	
	// Test 1: Base EVM
	res1, err := svc.AnalyzeToken(context.Background(), "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "")
	if err != nil || res1.Status != StatusSuccess {
		t.Fatalf("Base test failed: %v, status: %s", err, res1.Status)
	}
	card1 := FormatCard(res1.Metrics)
	if hasEmoji(card1) {
		t.Errorf("Card 1 contains emojis: %s", card1)
	}
	t.Logf("Base Card:\n%s", card1)

	// Test 2: Solana Mint
	res2, err := svc.AnalyzeToken(context.Background(), "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", "")
	if err != nil || res2.Status != StatusSuccess {
		t.Fatalf("Solana test failed: %v, status: %s", err, res2.Status)
	}
	card2 := FormatCard(res2.Metrics)
	if hasEmoji(card2) {
		t.Errorf("Card 2 contains emojis: %s", card2)
	}
	t.Logf("Solana Card:\n%s", card2)

	// Test 3: TERA on Robinhood (enriched with Codex market cap)
	res3, err := svc.AnalyzeToken(context.Background(), "0x3c12e57fa7817a86ce7c254db9ea5fe639e233f8", "robinhood")
	if err != nil || res3.Status != StatusSuccess {
		t.Fatalf("Robinhood test failed: %v, status: %s", err, res3.Status)
	}
	t.Logf("Robinhood TERA MarketCap: %v, FDV: %v, Holders: %d", res3.Metrics.MarketCap, res3.Metrics.FDV, res3.Metrics.HoldersCount)
	if res3.Metrics.MarketCap < 35000 {
		t.Errorf("expected MarketCap to be enriched from Codex (>35K), got: %v", res3.Metrics.MarketCap)
	}
	natural3 := FormatNatural(res3.Metrics)
	t.Logf("Natural Format:\n%s", natural3)
}


