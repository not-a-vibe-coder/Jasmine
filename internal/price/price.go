package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Prices struct {
	ETH float64 `json:"eth"`
	SOL float64 `json:"sol"`
	BNB float64 `json:"bnb"`
}

type Service struct {
	httpClient *http.Client
	cache      Prices
	cacheTime  time.Time
	cacheMu    sync.RWMutex
	cacheTTL   time.Duration
}

func NewService() *Service {
	return &Service{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		cacheTTL:   3 * time.Minute,
	}
}

// GetPrices returns cached crypto prices in USD, refreshing every 3 minutes.
// If CoinGecko returns 429 or fails, it falls back to stale cache or Binance public ticker.
func (s *Service) GetPrices(ctx context.Context) Prices {
	s.cacheMu.RLock()
	if time.Since(s.cacheTime) < s.cacheTTL && (s.cache.ETH > 0 || s.cache.SOL > 0 || s.cache.BNB > 0) {
		cached := s.cache
		s.cacheMu.RUnlock()
		return cached
	}
	s.cacheMu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	// Double check under write lock
	if time.Since(s.cacheTime) < s.cacheTTL && (s.cache.ETH > 0 || s.cache.SOL > 0 || s.cache.BNB > 0) {
		return s.cache
	}

	prices, err := s.fetchCoinGecko(ctx)
	if err == nil && (prices.ETH > 0 || prices.SOL > 0) {
		s.cache = prices
		s.cacheTime = time.Now()
		return s.cache
	}

	log.Printf("[PriceService] CoinGecko fetch failed: %v", err)

	// Fallback 1: Stale cache if available
	if s.cache.ETH > 0 || s.cache.SOL > 0 {
		log.Printf("[PriceService] Using stale cached prices")
		return s.cache
	}

	// Fallback 2: Binance public ticker
	binancePrices, bErr := s.fetchBinance(ctx)
	if bErr == nil && (binancePrices.ETH > 0 || binancePrices.SOL > 0) {
		s.cache = binancePrices
		s.cacheTime = time.Now()
		return s.cache
	}

	log.Printf("[PriceService] Binance fallback failed: %v", bErr)
	return s.cache
}

func (s *Service) fetchCoinGecko(ctx context.Context) (Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.coingecko.com/api/v3/simple/price?ids=ethereum,solana,binancecoin&vs_currencies=usd", nil)
	if err != nil {
		return Prices{}, err
	}
	req.Header.Set("User-Agent", "ShippBot/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Prices{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Prices{}, fmt.Errorf("coingecko returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Prices{}, err
	}

	var data map[string]struct {
		USD float64 `json:"usd"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return Prices{}, err
	}

	var p Prices
	if eth, ok := data["ethereum"]; ok {
		p.ETH = eth.USD
	}
	if sol, ok := data["solana"]; ok {
		p.SOL = sol.USD
	}
	if bnb, ok := data["binancecoin"]; ok {
		p.BNB = bnb.USD
	}

	return p, nil
}

func (s *Service) fetchBinance(ctx context.Context) (Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, `https://api.binance.com/api/v3/ticker/price?symbols=["ETHUSDT","SOLUSDT","BNBUSDT"]`, nil)
	if err != nil {
		return Prices{}, err
	}
	req.Header.Set("User-Agent", "ShippBot/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Prices{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Prices{}, fmt.Errorf("binance ticker returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Prices{}, err
	}

	var items []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return Prices{}, err
	}

	var p Prices
	for _, item := range items {
		val, _ := strconv.ParseFloat(item.Price, 64)
		switch item.Symbol {
		case "ETHUSDT":
			p.ETH = val
		case "SOLUSDT":
			p.SOL = val
		case "BNBUSDT":
			p.BNB = val
		}
	}

	return p, nil
}

// ConvertToUSD calculates USD value for a given amount and currency symbol
func ConvertToUSD(amount float64, symbol string, p Prices) float64 {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	switch sym {
	case "ETH":
		return amount * p.ETH
	case "SOL":
		return amount * p.SOL
	case "BNB":
		return amount * p.BNB
	default:
		return 0
	}
}

// FormatUSD formats currency cleanly
func FormatUSD(val float64) string {
	if val <= 0 {
		return "$0.00"
	}
	if val < 0.01 {
		return fmt.Sprintf("$%.4f", val)
	}
	return fmt.Sprintf("$%.2f", val)
}
