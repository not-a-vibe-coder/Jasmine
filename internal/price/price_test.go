package price

import (
	"context"
	"testing"
	"time"
)

func TestPriceConversion(t *testing.T) {
	p := Prices{
		ETH: 2600.0,
		SOL: 150.0,
		BNB: 580.0,
	}

	ethUSD := ConvertToUSD(0.00011159, "ETH", p)
	if ethUSD < 0.28 || ethUSD > 0.30 {
		t.Errorf("expected ~0.29 USD, got %f", ethUSD)
	}

	formatted := FormatUSD(ethUSD)
	if formatted != "$0.29" {
		t.Errorf("expected $0.29, got %s", formatted)
	}

	tiny := FormatUSD(0.0042)
	if tiny != "$0.0042" {
		t.Errorf("expected $0.0042, got %s", tiny)
	}

	// Test 0.0004835 ETH (the exact amount from Robinhood)
	rhUSD, rate, err := Convert(0.0004835, "ETH", "USD", p)
	if err != nil {
		t.Fatalf("unexpected error converting ETH: %v", err)
	}
	if rhUSD < 1.25 || rhUSD > 1.27 {
		t.Errorf("expected ~1.26 USD, got %f", rhUSD)
	}
	if FormatUSD(rhUSD) != "$1.26" {
		t.Errorf("expected $1.26, got %s", FormatUSD(rhUSD))
	}
	if rate != 2600.0 {
		t.Errorf("expected rate 2600.0, got %f", rate)
	}

	// Test USD to SOL
	solAmount, _, err := Convert(50.0, "USD", "SOL", p)
	if err != nil {
		t.Fatalf("unexpected error converting USD to SOL: %v", err)
	}
	if solAmount < 0.33 || solAmount > 0.34 {
		t.Errorf("expected ~0.333 SOL, got %f", solAmount)
	}
}

func TestPriceServiceLiveOrFallback(t *testing.T) {
	svc := NewService()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prices := svc.GetPrices(ctx)
	t.Logf("Fetched prices: ETH=$%.2f, SOL=$%.2f, BNB=$%.2f", prices.ETH, prices.SOL, prices.BNB)

	if prices.ETH == 0 && prices.SOL == 0 {
		t.Errorf("expected non-zero prices from live or fallback")
	}

	// Test cache hit
	cached := svc.GetPrices(ctx)
	if cached != prices {
		t.Errorf("expected identical cached prices")
	}
}

func TestCoinGecko429Cooldown(t *testing.T) {
	svc := NewService()
	// Simulate active cooldown
	svc.coingeckoCooldown = time.Now().Add(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := svc.GetPrices(ctx)
	if p.ETH <= 0 || p.SOL <= 0 {
		t.Errorf("expected valid fallback prices during CoinGecko cooldown, got %+v", p)
	}
}
