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
