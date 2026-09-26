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
	BTC float64 `json:"btc"`
}

// Emergency baseline in case all APIs are down simultaneously (rates are non-zero)
var defaultPrices = Prices{
	ETH: 2680.0,
	SOL: 125.0,
	BNB: 770.0,
	BTC: 89000.0,
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
		cache:      defaultPrices,
	}
}

// GetPrices returns cached crypto prices in USD, refreshing every 3 minutes.
// Fallback order: CoinGecko -> Coinbase -> Binance -> Stale Cache -> Default Baseline.
func (s *Service) GetPrices(ctx context.Context) Prices {
	s.cacheMu.RLock()
	if time.Since(s.cacheTime) < s.cacheTTL && (s.cache.ETH > 0 && s.cache.SOL > 0) {
		cached := s.cache
		s.cacheMu.RUnlock()
		return cached
	}
	s.cacheMu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	// Double check under write lock
	if time.Since(s.cacheTime) < s.cacheTTL && (s.cache.ETH > 0 && s.cache.SOL > 0) {
		return s.cache
	}

	// 1. Primary: CoinGecko
	prices, err := s.fetchCoinGecko(ctx)
	if err == nil && prices.ETH > 0 && prices.SOL > 0 {
		s.cache = prices
		s.cacheTime = time.Now()
		return s.cache
	}
	log.Printf("[PriceService] CoinGecko fetch failed: %v", err)

	// 2. Secondary: Coinbase Spot API
	cbPrices, cbErr := s.fetchCoinbase(ctx)
	if cbErr == nil && cbPrices.ETH > 0 && cbPrices.SOL > 0 {
		s.cache = cbPrices
		s.cacheTime = time.Now()
		return s.cache
	}
	log.Printf("[PriceService] Coinbase fallback failed: %v", cbErr)

	// 3. Tertiary: Binance public ticker
	binancePrices, bErr := s.fetchBinance(ctx)
	if bErr == nil && binancePrices.ETH > 0 && binancePrices.SOL > 0 {
		s.cache = binancePrices
		s.cacheTime = time.Now()
		return s.cache
	}
	log.Printf("[PriceService] Binance fallback failed: %v", bErr)

	// 4. Stale cache if available
	if s.cache.ETH > 0 && s.cache.SOL > 0 {
		log.Printf("[PriceService] Using stale cached prices")
		return s.cache
	}

	// 5. Default emergency baseline (never return $0)
	return defaultPrices
}

func (s *Service) fetchCoinGecko(ctx context.Context) (Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.coingecko.com/api/v3/simple/price?ids=ethereum,solana,binancecoin,bitcoin&vs_currencies=usd", nil)
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

	p := defaultPrices
	if eth, ok := data["ethereum"]; ok && eth.USD > 0 {
		p.ETH = eth.USD
	}
	if sol, ok := data["solana"]; ok && sol.USD > 0 {
		p.SOL = sol.USD
	}
	if bnb, ok := data["binancecoin"]; ok && bnb.USD > 0 {
		p.BNB = bnb.USD
	}
	if btc, ok := data["bitcoin"]; ok && btc.USD > 0 {
		p.BTC = btc.USD
	}

	return p, nil
}

func (s *Service) fetchCoinbase(ctx context.Context) (Prices, error) {
	symbols := []string{"ETH", "SOL", "BNB", "BTC"}
	p := defaultPrices
	var anySuccess bool

	for _, sym := range symbols {
		url := fmt.Sprintf("https://api.coinbase.com/v2/prices/%s-USD/spot", sym)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "ShippBot/1.0")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}

		var cbResp struct {
			Data struct {
				Amount string `json:"amount"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &cbResp); err == nil {
			if val, err := strconv.ParseFloat(cbResp.Data.Amount, 64); err == nil && val > 0 {
				anySuccess = true
				switch sym {
				case "ETH":
					p.ETH = val
				case "SOL":
					p.SOL = val
				case "BNB":
					p.BNB = val
				case "BTC":
					p.BTC = val
				}
			}
		}
	}

	if !anySuccess {
		return Prices{}, fmt.Errorf("coinbase spot price fetch failed")
	}
	return p, nil
}

func (s *Service) fetchBinance(ctx context.Context) (Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, `https://api.binance.com/api/v3/ticker/price?symbols=["ETHUSDT","SOLUSDT","BNBUSDT","BTCUSDT"]`, nil)
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

	p := defaultPrices
	for _, item := range items {
		val, _ := strconv.ParseFloat(item.Price, 64)
		if val <= 0 {
			continue
		}
		switch item.Symbol {
		case "ETHUSDT":
			p.ETH = val
		case "SOLUSDT":
			p.SOL = val
		case "BNBUSDT":
			p.BNB = val
		case "BTCUSDT":
			p.BTC = val
		}
	}

	return p, nil
}

// GetPrice returns the current USD price of a crypto asset
func GetPrice(symbol string, p Prices) float64 {
	switch strings.ToUpper(strings.TrimSpace(symbol)) {
	case "ETH", "WETH":
		return p.ETH
	case "SOL", "WSOL":
		return p.SOL
	case "BNB", "WBNB":
		return p.BNB
	case "BTC", "WBTC":
		return p.BTC
	default:
		return 0
	}
}

// Convert converts between any supported crypto asset and USD (or vice-versa)
func Convert(amount float64, from, to string, p Prices) (result float64, rate float64, err error) {
	f := strings.ToUpper(strings.TrimSpace(from))
	t := strings.ToUpper(strings.TrimSpace(to))

	if f == "" || t == "" {
		return 0, 0, fmt.Errorf("source and target currencies cannot be empty")
	}

	if f == t {
		return amount, 1.0, nil
	}

	// 1. From Crypto to USD
	if t == "USD" || t == "USDC" || t == "USDT" {
		price := GetPrice(f, p)
		if price <= 0 {
			return 0, 0, fmt.Errorf("unsupported or unavailable price for %s", f)
		}
		return amount * price, price, nil
	}

	// 2. From USD to Crypto
	if f == "USD" || f == "USDC" || f == "USDT" {
		price := GetPrice(t, p)
		if price <= 0 {
			return 0, 0, fmt.Errorf("unsupported or unavailable price for %s", t)
		}
		return amount / price, price, nil
	}

	// 3. Crypto to Crypto (via USD valuation)
	priceFrom := GetPrice(f, p)
	priceTo := GetPrice(t, p)
	if priceFrom <= 0 || priceTo <= 0 {
		return 0, 0, fmt.Errorf("unsupported cross-conversion between %s and %s", f, t)
	}

	usdVal := amount * priceFrom
	converted := usdVal / priceTo
	rate = priceFrom / priceTo
	return converted, rate, nil
}

// ConvertToUSD calculates USD value for a given amount and currency symbol
func ConvertToUSD(amount float64, symbol string, p Prices) float64 {
	val, _, err := Convert(amount, symbol, "USD", p)
	if err != nil {
		return 0
	}
	return val
}

// FormatUSD formats currency cleanly, guaranteeing precision even for tiny values
func FormatUSD(val float64) string {
	if val <= 0 {
		return "$0.00"
	}
	if val < 0.0001 {
		return fmt.Sprintf("$%.6f", val)
	}
	if val < 0.01 {
		return fmt.Sprintf("$%.4f", val)
	}
	return fmt.Sprintf("$%.2f", val)
}

// FormatCrypto formats a crypto token amount cleanly
func FormatCrypto(amount float64, symbol string) string {
	if amount < 0.0001 {
		return fmt.Sprintf("%.6f", amount)
	}
	return fmt.Sprintf("%.4f", amount)
}
