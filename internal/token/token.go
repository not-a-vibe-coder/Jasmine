package token

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	evmRegex    = regexp.MustCompile(`(?i)\b0x[a-f0-9]{40}\b`)
	solanaRegex = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{32,44}\b`)
)

type TokenMetrics struct {
	Address         string
	Name            string
	Symbol          string
	Chain           string
	PriceUSD        float64
	PriceNative     string
	MarketCap       float64
	FDV             float64
	LiquidityUSD    float64
	Volume24h       float64
	PriceChange24h  float64
	Buys24h         int64
	Sells24h        int64
	DexID           string
	PairAddress     string
	DexURL          string
	Website         string
	Twitter         string
	Telegram        string
	HoldersCount    int64
	Top10HoldersPct float64
	DevHoldingsPct  float64
	DevBuys         int64
	DevSells        int64
	DataSource      string
	OtherChains     []string
}

type AnalysisStatus string

const (
	StatusSuccess        AnalysisStatus = "ok"
	StatusAmbiguousChain AnalysisStatus = "ambiguous_chain"
	StatusNotFound       AnalysisStatus = "not_found"
	StatusInvalidAddress AnalysisStatus = "invalid_address"
)

type AnalysisResult struct {
	Status          AnalysisStatus `json:"status"`
	Metrics         *TokenMetrics  `json:"metrics,omitempty"`
	CandidateChains []string       `json:"candidateChains,omitempty"`
	Address         string         `json:"address"`
	Message         string         `json:"message"`
}

type cacheEntry struct {
	timestamp time.Time
	data      *AnalysisResult
}

type Service struct {
	codexAPIKey string
	httpClient  *http.Client
	mu          sync.RWMutex
	cache       map[string]cacheEntry
	cacheTTL    time.Duration
}

func NewService(codexAPIKey string) *Service {
	return &Service{
		codexAPIKey: codexAPIKey,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		cache:    make(map[string]cacheEntry),
		cacheTTL: 60 * time.Second,
	}
}

// IsValidEVMAddress checks if string is a valid EVM address
func IsValidEVMAddress(addr string) bool {
	clean := strings.TrimSpace(addr)
	return regexp.MustCompile(`(?i)^0x[a-f0-9]{40}$`).MatchString(clean)
}

// IsValidSolanaAddress checks if string is a plausible base58 Solana address
func IsValidSolanaAddress(addr string) bool {
	clean := strings.TrimSpace(addr)
	return regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`).MatchString(clean)
}

// ExtractAddressAndChain scans input text for token address and optional chain
func ExtractAddressAndChain(input string) (address string, chain string) {
	tokens := strings.Fields(input)
	knownChains := map[string]string{
		"base":        "base",
		"sol":         "solana",
		"solana":      "solana",
		"svm":         "solana",
		"eth":         "ethereum",
		"ethereum":    "ethereum",
		"mainnet":     "ethereum",
		"bsc":         "bsc",
		"bnb":         "bsc",
		"binance":     "bsc",
		"arb":         "arbitrum",
		"arbitrum":    "arbitrum",
		"polygon":     "polygon",
		"matic":       "polygon",
		"optimism":    "optimism",
		"op":          "optimism",
		"avax":        "avalanche",
		"avalanche":   "avalanche",
		"pulsechain":  "pulsechain",
		"pulse":       "pulsechain",
		"rh":          "robinhood",
		"robinhood":   "robinhood",
	}

	for _, t := range tokens {
		clean := strings.Trim(t, ",.:;!?()'\"[]{}")
		if normChain, exists := knownChains[strings.ToLower(clean)]; exists {
			chain = normChain
			continue
		}
		if IsValidEVMAddress(clean) {
			address = clean
		} else if IsValidSolanaAddress(clean) && len(clean) >= 32 {
			// Prefer EVM if not set yet, or keep Solana
			if address == "" {
				address = clean
			}
		}
	}

	// Fallback regex search if fields did not find it
	if address == "" {
		if evmMatch := evmRegex.FindString(input); evmMatch != "" {
			address = evmMatch
		} else if solMatch := solanaRegex.FindString(input); solMatch != "" && len(solMatch) >= 32 {
			address = solMatch
		}
	}

	return address, chain
}

func NormalizeChain(chain string) string {
	c := strings.ToLower(strings.TrimSpace(chain))
	switch c {
	case "sol", "solana", "svm":
		return "solana"
	case "eth", "ethereum", "mainnet":
		return "ethereum"
	case "bsc", "bnb", "binance":
		return "bsc"
	case "arb", "arbitrum":
		return "arbitrum"
	case "polygon", "matic":
		return "polygon"
	case "base":
		return "base"
	case "op", "optimism":
		return "optimism"
	case "avax", "avalanche":
		return "avalanche"
	case "rh", "robinhood":
		return "robinhood"
	default:
		return c
	}
}

// ChainCodexNetworkID maps normalized chain name to Codex GraphQL network ID
func ChainCodexNetworkID(chain string) int {
	switch chain {
	case "ethereum":
		return 1
	case "base":
		return 8453
	case "solana":
		return 1399811149
	case "bsc":
		return 56
	case "arbitrum":
		return 42161
	case "polygon":
		return 137
	case "optimism":
		return 10
	case "avalanche":
		return 43114
	case "robinhood", "rh":
		return 4663
	default:
		return 0
	}
}

// DexScreener API Structs
type dexScreenerResponse struct {
	SchemaVersion string          `json:"schemaVersion"`
	Pairs         []dexPairSchema `json:"pairs"`
}

type dexPairSchema struct {
	ChainID     string `json:"chainId"`
	DexID       string `json:"dexId"`
	URL         string `json:"url"`
	PairAddress string `json:"pairAddress"`
	BaseToken   struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Symbol  string `json:"symbol"`
	} `json:"baseToken"`
	PriceNative string  `json:"priceNative"`
	PriceUSD    string  `json:"priceUsd"`
	FDV         float64 `json:"fdv"`
	MarketCap   float64 `json:"marketCap"`
	PriceChange struct {
		H24 float64 `json:"h24"`
	} `json:"priceChange"`
	Liquidity struct {
		USD float64 `json:"usd"`
	} `json:"liquidity"`
	Volume struct {
		H24 float64 `json:"h24"`
	} `json:"volume"`
	Txns struct {
		H24 struct {
			Buys  int64 `json:"buys"`
			Sells int64 `json:"sells"`
		} `json:"h24"`
	} `json:"txns"`
	Info struct {
		Websites []struct {
			Label string `json:"label"`
			URL   string `json:"url"`
		} `json:"websites"`
		Socials []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"socials"`
	} `json:"info"`
}

// AnalyzeToken analyzes a token address using DexScreener with Codex.io fallback/enrichment.
// If multiple chains have active liquidity and no chain is specified, it returns an ambiguous status.
func (s *Service) AnalyzeToken(ctx context.Context, rawAddress string, preferredChain string) (*AnalysisResult, error) {
	address := strings.TrimSpace(rawAddress)
	if address == "" {
		return &AnalysisResult{
			Status:  StatusInvalidAddress,
			Address: address,
			Message: "No address provided.",
		}, nil
	}

	normChain := NormalizeChain(preferredChain)
	cacheKey := fmt.Sprintf("%s:%s", strings.ToLower(address), normChain)

	// Check cache
	s.mu.RLock()
	if entry, found := s.cache[cacheKey]; found && time.Since(entry.timestamp) < s.cacheTTL {
		s.mu.RUnlock()
		return entry.data, nil
	}
	s.mu.RUnlock()

	// 1. Query DexScreener
	dexData, err := s.fetchDexScreener(ctx, address)
	if err != nil || dexData == nil || len(dexData.Pairs) == 0 {
		// Try Codex directly if DexScreener has no pairs and we know the chain or it's EVM/Solana
		codexMetrics := s.fetchCodexFallback(ctx, address, normChain)
		if codexMetrics != nil {
			res := &AnalysisResult{
				Status:  StatusSuccess,
				Metrics: codexMetrics,
				Address: address,
			}
			s.saveCache(cacheKey, res)
			return res, nil
		}

		res := &AnalysisResult{
			Status:  StatusNotFound,
			Address: address,
			Message: fmt.Sprintf("Could not find market data for address %s. Which chain is this token on? (e.g. Solana, Base, Ethereum, BSC, Arbitrum)", address),
		}
		s.saveCache(cacheKey, res)
		return res, nil
	}

	// 2. Multi-chain analysis & ambiguity resolution
	chainLiquidity := make(map[string]float64)
	chainPairs := make(map[string][]dexPairSchema)

	for _, p := range dexData.Pairs {
		c := strings.ToLower(p.ChainID)
		chainLiquidity[c] += p.Liquidity.USD
		chainPairs[c] = append(chainPairs[c], p)
	}

	// If preferredChain was specified, filter for that chain
	if normChain != "" {
		pairs, exists := chainPairs[normChain]
		if !exists || len(pairs) == 0 {
			// Try Codex for this chain before failing
			codexMetrics := s.fetchCodexFallback(ctx, address, normChain)
			if codexMetrics != nil {
				res := &AnalysisResult{
					Status:  StatusSuccess,
					Metrics: codexMetrics,
					Address: address,
				}
				s.saveCache(cacheKey, res)
				return res, nil
			}

			// User specified chain that has no pools
			var available []string
			for c := range chainPairs {
				available = append(available, FormatChainTitle(c))
			}
			res := &AnalysisResult{
				Status:          StatusNotFound,
				CandidateChains: available,
				Address:         address,
				Message:         fmt.Sprintf("No active pairs found for address %s on %s. Available chains: %s.", address, FormatChainTitle(normChain), strings.Join(available, ", ")),
			}
			return res, nil
		}

		metrics := s.extractMetricsFromPairs(address, normChain, pairs, nil)
		s.enrichWithCodex(ctx, metrics)
		res := &AnalysisResult{
			Status:  StatusSuccess,
			Metrics: metrics,
			Address: address,
		}
		s.saveCache(cacheKey, res)
		return res, nil
	}

	// No chain specified: check if address is active on multiple chains
	type chainSum struct {
		chain string
		liq   float64
	}
	var chainSums []chainSum
	var totalLiq float64
	for c, l := range chainLiquidity {
		chainSums = append(chainSums, chainSum{chain: c, liq: l})
		totalLiq += l
	}

	// Sort descending by liquidity
	sort.Slice(chainSums, func(i, j int) bool {
		return chainSums[i].liq > chainSums[j].liq
	})

	// Ambiguity check:
	// If 2 or more chains have active pools with > $2,000 liquidity AND second chain has > 10% of total liquidity
	if len(chainSums) > 1 && chainSums[1].liq >= 2000 && (totalLiq > 0 && chainSums[1].liq/totalLiq >= 0.10) {
		var candidateChains []string
		for _, cs := range chainSums {
			if cs.liq >= 1000 {
				candidateChains = append(candidateChains, FormatChainTitle(cs.chain))
			}
		}

		res := &AnalysisResult{
			Status:          StatusAmbiguousChain,
			CandidateChains: candidateChains,
			Address:         address,
			Message:         fmt.Sprintf("This token address exists across multiple chains (%s). Please clarify which chain you'd like to check.", strings.Join(candidateChains, ", ")),
		}
		s.saveCache(cacheKey, res)
		return res, nil
	}

	// Primary chain selected
	primaryChain := chainSums[0].chain
	primaryPairs := chainPairs[primaryChain]

	var otherChains []string
	for i := 1; i < len(chainSums); i++ {
		if chainSums[i].liq > 500 {
			otherChains = append(otherChains, FormatChainTitle(chainSums[i].chain))
		}
	}

	metrics := s.extractMetricsFromPairs(address, primaryChain, primaryPairs, otherChains)
	s.enrichWithCodex(ctx, metrics)

	res := &AnalysisResult{
		Status:  StatusSuccess,
		Metrics: metrics,
		Address: address,
	}
	s.saveCache(cacheKey, res)
	return res, nil
}

func (s *Service) saveCache(key string, res *AnalysisResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cacheEntry{
		timestamp: time.Now(),
		data:      res,
	}
}

func (s *Service) fetchDexScreener(ctx context.Context, address string) (*dexScreenerResponse, error) {
	url := fmt.Sprintf("https://api.dexscreener.com/latest/dex/tokens/%s", address)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dexscreener status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data dexScreenerResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func (s *Service) extractMetricsFromPairs(address, chain string, pairs []dexPairSchema, otherChains []string) *TokenMetrics {
	// Pick pair with highest liquidity
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].Liquidity.USD > pairs[j].Liquidity.USD
	})
	best := pairs[0]

	priceUSD, _ := strconv.ParseFloat(best.PriceUSD, 64)
	mcap := best.MarketCap
	if mcap == 0 {
		mcap = best.FDV
	}

	var website, twitter, telegram string
	for _, w := range best.Info.Websites {
		if w.URL != "" {
			website = w.URL
			break
		}
	}
	for _, sc := range best.Info.Socials {
		switch strings.ToLower(sc.Type) {
		case "twitter", "x":
			if twitter == "" {
				twitter = sc.URL
			}
		case "telegram":
			if telegram == "" {
				telegram = sc.URL
			}
		}
	}

	return &TokenMetrics{
		Address:        address,
		Name:           best.BaseToken.Name,
		Symbol:         best.BaseToken.Symbol,
		Chain:          FormatChainTitle(chain),
		PriceUSD:       priceUSD,
		PriceNative:    best.PriceNative,
		MarketCap:      mcap,
		FDV:            best.FDV,
		LiquidityUSD:   best.Liquidity.USD,
		Volume24h:      best.Volume.H24,
		PriceChange24h: best.PriceChange.H24,
		Buys24h:        best.Txns.H24.Buys,
		Sells24h:       best.Txns.H24.Sells,
		DexID:          strings.ToUpper(best.DexID),
		PairAddress:    best.PairAddress,
		DexURL:         best.URL,
		Website:        website,
		Twitter:        twitter,
		Telegram:       telegram,
		DataSource:     "dexscreener",
		OtherChains:    otherChains,
	}
}

// Codex GraphQL Fallback & Enrichment
type codexGraphQLResponse struct {
	Data struct {
		FilterTokens struct {
			Results []struct {
				PriceUSD    string `json:"priceUSD"`
				MarketCap   string `json:"marketCap"`
				Liquidity   string `json:"liquidity"`
				Volume24    string `json:"volume24"`
				Change24    string `json:"change24"`
				BuyCount24  int64  `json:"buyCount24"`
				SellCount24 int64  `json:"sellCount24"`
				Holders     int64  `json:"holders"`
				Token       struct {
					Name   string `json:"name"`
					Symbol string `json:"symbol"`
					Info   struct {
						TotalSupply       string `json:"totalSupply"`
						CirculatingSupply string `json:"circulatingSupply"`
					} `json:"info"`
					Exchanges []struct {
						Name    string `json:"name"`
						Address string `json:"address"`
					} `json:"exchanges"`
				} `json:"token"`
				Top10HoldersPercent float64 `json:"top10HoldersPercent"`
			} `json:"results"`
		} `json:"filterTokens"`
	} `json:"data"`
}

func (s *Service) enrichWithCodex(ctx context.Context, metrics *TokenMetrics) {
	if s.codexAPIKey == "" || metrics == nil {
		return
	}
	chainKey := strings.ToLower(metrics.Chain)
	networkID := ChainCodexNetworkID(chainKey)
	if networkID == 0 {
		return
	}

	tokenQuery := fmt.Sprintf("%s:%d", metrics.Address, networkID)
	res, err := s.queryCodex(ctx, tokenQuery)
	if err != nil || res == nil || len(res.Data.FilterTokens.Results) == 0 {
		return
	}

	item := res.Data.FilterTokens.Results[0]
	if item.Holders > 0 {
		metrics.HoldersCount = item.Holders
	}
	if item.Top10HoldersPercent > 0 {
		metrics.Top10HoldersPct = item.Top10HoldersPercent
	}
	if mc, err := strconv.ParseFloat(item.MarketCap, 64); err == nil && mc > 0 {
		metrics.MarketCap = mc
		if metrics.FDV == 0 || metrics.FDV < mc {
			metrics.FDV = mc
		}
	}
	if p, err := strconv.ParseFloat(item.PriceUSD, 64); err == nil && p > 0 {
		if metrics.PriceUSD == 0 {
			metrics.PriceUSD = p
		}
	}
	metrics.DataSource = "codex.io + dexscreener"
}

func (s *Service) fetchCodexFallback(ctx context.Context, address, chain string) *TokenMetrics {
	if s.codexAPIKey == "" {
		return nil
	}

	var networkIDs []int
	if chain != "" {
		if id := ChainCodexNetworkID(chain); id > 0 {
			networkIDs = append(networkIDs, id)
		}
	} else if IsValidEVMAddress(address) {
		// Try Base, Robinhood, Ethereum, Arbitrum, BSC
		networkIDs = []int{8453, 4663, 1, 42161, 56}
	} else if IsValidSolanaAddress(address) {
		networkIDs = []int{1399811149}
	}

	for _, nid := range networkIDs {
		tokenQuery := fmt.Sprintf("%s:%d", address, nid)
		res, err := s.queryCodex(ctx, tokenQuery)
		if err != nil || res == nil || len(res.Data.FilterTokens.Results) == 0 {
			continue
		}
		item := res.Data.FilterTokens.Results[0]
		price, _ := strconv.ParseFloat(item.PriceUSD, 64)
		mc, _ := strconv.ParseFloat(item.MarketCap, 64)
		liq, _ := strconv.ParseFloat(item.Liquidity, 64)
		vol, _ := strconv.ParseFloat(item.Volume24, 64)
		change, _ := strconv.ParseFloat(item.Change24, 64)
		// Codex returns decimal fraction e.g. 0.125 = 12.5%
		changePct := change * 100

		chainName := "EVM"
		switch nid {
		case 8453:
			chainName = "Base"
		case 4663:
			chainName = "Robinhood"
		case 1:
			chainName = "Ethereum"
		case 1399811149:
			chainName = "Solana"
		case 56:
			chainName = "BSC"
		case 42161:
			chainName = "Arbitrum"
		}

		dexName := "DEX"
		pairAddr := ""
		if len(item.Token.Exchanges) > 0 {
			dexName = item.Token.Exchanges[0].Name
			pairAddr = item.Token.Exchanges[0].Address
		}

		return &TokenMetrics{
			Address:         address,
			Name:            item.Token.Name,
			Symbol:          item.Token.Symbol,
			Chain:           chainName,
			PriceUSD:        price,
			MarketCap:       mc,
			FDV:             mc,
			LiquidityUSD:    liq,
			Volume24h:       vol,
			PriceChange24h:  changePct,
			Buys24h:         item.BuyCount24,
			Sells24h:        item.SellCount24,
			DexID:           strings.ToUpper(dexName),
			PairAddress:     pairAddr,
			HoldersCount:    item.Holders,
			Top10HoldersPct: item.Top10HoldersPercent,
			DataSource:      "codex.io",
		}
	}
	return nil
}

func (s *Service) queryCodex(ctx context.Context, tokenArg string) (*codexGraphQLResponse, error) {
	query := `query FilterTokens($tokens: [String!]!) {
		filterTokens(tokens: $tokens) {
			results {
				priceUSD
				marketCap
				liquidity
				volume24
				change24
				buyCount24
				sellCount24
				holders
				top10HoldersPercent
				token {
					name
					symbol
					info {
						totalSupply
						circulatingSupply
					}
					exchanges {
						name
						address
					}
				}
			}
		}
	}`

	reqBody, err := json.Marshal(map[string]interface{}{
		"query": query,
		"variables": map[string]interface{}{
			"tokens": []string{tokenArg},
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://graph.codex.io/graphql", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.codexAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex api returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data codexGraphQLResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// FormatCard formats token metrics with STRICTLY NO EMOJIS
func FormatCard(m *TokenMetrics) string {
	if m == nil {
		return "No token metrics available."
	}

	// Change format: e.g. (+14.2%) or (-5.8%)
	var changeStr string
	if m.PriceChange24h >= 0 {
		changeStr = fmt.Sprintf("+%.1f%%", m.PriceChange24h)
	} else {
		changeStr = fmt.Sprintf("%.1f%%", m.PriceChange24h)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("[$%s] %s", m.Symbol, m.Name))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("• Price: %s (%s)", FormatPrice(m.PriceUSD), changeStr))
	lines = append(lines, fmt.Sprintf("• MCap: %s | LP: %s", FormatUSD(m.MarketCap), FormatUSD(m.LiquidityUSD)))
	lines = append(lines, fmt.Sprintf("• Vol 24h: %s | Buys: %s / Sells: %s", FormatUSD(m.Volume24h), FormatCount(m.Buys24h), FormatCount(m.Sells24h)))

	chainDex := m.Chain
	if m.DexID != "" && m.DexID != "DEX" {
		chainDex = fmt.Sprintf("%s (%s)", m.Chain, m.DexID)
	}
	lines = append(lines, fmt.Sprintf("• Chain: %s", chainDex))

	if m.HoldersCount > 0 || m.Top10HoldersPct > 0 {
		holderStr := FormatCount(m.HoldersCount)
		if m.HoldersCount == 0 {
			holderStr = "-"
		}
		top10Str := ""
		if m.Top10HoldersPct > 0 {
			top10Str = fmt.Sprintf(" (Top 10: %.1f%%)", m.Top10HoldersPct)
		}
		lines = append(lines, fmt.Sprintf("• Holders: %s%s", holderStr, top10Str))
	}

	if m.DexURL != "" {
		lines = append(lines, fmt.Sprintf("• DexScreener: %s", m.DexURL))
	}

	if len(m.OtherChains) > 0 {
		lines = append(lines, fmt.Sprintf("• Note: Pools also detected on %s. Specify chain if needed.", strings.Join(m.OtherChains, ", ")))
	}

	return strings.Join(lines, "\n")
}

// FormatNatural formats token metrics in a casual, conversational tone without an official bulleted list
func FormatNatural(m *TokenMetrics) string {
	if m == nil {
		return "couldn't pull up metrics for that token."
	}

	changeStr := fmt.Sprintf("+%.1f%%", m.PriceChange24h)
	if m.PriceChange24h < 0 {
		changeStr = fmt.Sprintf("%.1f%%", m.PriceChange24h)
	}

	var parts []string
	if m.MarketCap > 0 {
		parts = append(parts, fmt.Sprintf("sitting around %s mcap", FormatUSD(m.MarketCap)))
	}
	if m.PriceUSD > 0 {
		parts = append(parts, fmt.Sprintf("trading at %s (%s today)", FormatPrice(m.PriceUSD), changeStr))
	}
	if m.Volume24h > 0 {
		parts = append(parts, fmt.Sprintf("doing about %s in 24h volume", FormatUSD(m.Volume24h)))
	}

	core := strings.Join(parts, ", ")
	if core == "" {
		core = fmt.Sprintf("trading at %s on %s", FormatPrice(m.PriceUSD), m.Chain)
	}

	chainNote := ""
	if m.Chain != "" {
		chainNote = fmt.Sprintf(" on %s", m.Chain)
	}

	return fmt.Sprintf("that's $%s (%s)%s, %s. hit me with 'detailed' if you want the full breakdown.", m.Symbol, m.Name, chainNote, core)
}

// FormatAmbiguousChains asks user to clarify chain with NO EMOJIS
func FormatAmbiguousChains(address string, chains []string) string {
	return fmt.Sprintf("This token address exists across multiple chains: %s.\nWhich chain would you like to check? (e.g. /ca %s %s)",
		strings.Join(chains, ", "),
		strings.ToLower(chains[0]),
		address,
	)
}

// FormatNotFound notifies user token was not found and requests chain clarification with NO EMOJIS
func FormatNotFound(address string) string {
	return fmt.Sprintf("Could not find market data for address %s.\nWhich chain is this token on? (e.g. Solana, Base, Robinhood, Ethereum, BSC, Arbitrum)", address)
}

// FormatUSD formats currency values into K, M, B abbreviations
func FormatUSD(val float64) string {
	if val <= 0 {
		return "$0.00"
	}
	if val >= 1_000_000_000 {
		return fmt.Sprintf("$%.2fB", val/1_000_000_000)
	}
	if val >= 1_000_000 {
		return fmt.Sprintf("$%.2fM", val/1_000_000)
	}
	if val >= 1_000 {
		return fmt.Sprintf("$%.2fK", val/1_000)
	}
	return fmt.Sprintf("$%.2f", val)
}

// FormatPrice formats token prices cleanly
func FormatPrice(val float64) string {
	if val == 0 {
		return "$0.00"
	}
	if val < 0.000001 {
		return fmt.Sprintf("$%.4e", val)
	}
	if val < 0.0001 {
		return fmt.Sprintf("$%.8f", val)
	}
	if val < 1 {
		return fmt.Sprintf("$%.6f", val)
	}
	return fmt.Sprintf("$%.2f", val)
}

// FormatCount formats integers with commas
func FormatCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	s := strconv.FormatInt(n, 10)
	var res []string
	l := len(s)
	for i := 0; i < l; i++ {
		if i > 0 && (l-i)%3 == 0 {
			res = append(res, ",")
		}
		res = append(res, string(s[i]))
	}
	return strings.Join(res, "")
}

// FormatChainTitle capitalizes chain names appropriately
func FormatChainTitle(c string) string {
	switch strings.ToLower(c) {
	case "solana":
		return "Solana"
	case "ethereum":
		return "Ethereum"
	case "base":
		return "Base"
	case "bsc":
		return "BSC"
	case "arbitrum":
		return "Arbitrum"
	case "polygon":
		return "Polygon"
	case "optimism":
		return "Optimism"
	case "avalanche":
		return "Avalanche"
	case "pulsechain":
		return "Pulsechain"
	case "robinhood", "rh":
		return "Robinhood"
	default:
		if len(c) > 0 {
			return strings.ToUpper(c[:1]) + strings.ToLower(c[1:])
		}
		return c
	}
}
