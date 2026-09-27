package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var defaultPopularTLDs = []string{".com", ".app", ".io", ".xyz", ".dev", ".ai", ".co"}
var domainCleanRegex = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?([a-zA-Z0-9.-]+).*$`)
var validDomainRegex = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)
var knownTLDRegex = regexp.MustCompile(`(?i)^\.?(com|app|io|xyz|ai|dev|co|org|net|me|bot|tech|online|store|site|so|gg|sh|cc|fun|link|live|space|pro|build|run|finance)$`)
var domainInTextRegex = regexp.MustCompile(`(?i)\b([a-zA-Z0-9-]{2,63})\.(com|app|io|xyz|ai|dev|co|org|net|me|bot|tech|online|store|site|so|gg|sh|cc|fun|link|live|space|pro|build|run|finance)\b`)
var tldInQueryRegex = regexp.MustCompile(`(?i)\.(com|app|io|xyz|ai|dev|co|org|net|me|bot|tech|online|store|site|so|gg|sh|cc|fun|link|live|space|pro|build|run|finance)\b`)

var ignoredPlatformBases = map[string]bool{
	"vercel":         true,
	"github":         true,
	"google":         true,
	"twitter":        true,
	"x":              true,
	"telegram":       true,
	"t":              true,
	"dexscreener":    true,
	"render":         true,
	"localhost":      true,
	"etherscan":      true,
	"solscan":        true,
	"coingecko":      true,
	"coinmarketcap":  true,
	"basescan":       true,
	"polygonscan":    true,
	"arbiscan":       true,
	"bscscan":        true,
}

var queryStopWords = map[string]bool{
	"how":          true,
	"much":         true,
	"is":           true,
	"are":          true,
	"what":         true,
	"about":        true,
	"for":          true,
	"check":        true,
	"search":       true,
	"price":        true,
	"pricing":      true,
	"cost":         true,
	"available":    true,
	"availability": true,
	"the":          true,
	"a":            true,
	"an":           true,
	"can":          true,
	"you":          true,
	"shipp":        true,
	"shipp0bot":    true,
	"bot":          true,
	"domain":       true,
	"domains":      true,
	"it":           true,
	"this":         true,
	"that":         true,
	"does":         true,
	"do":           true,
	"any":          true,
	"lookup":       true,
	"tell":         true,
	"me":           true,
	"find":         true,
	"get":          true,
	"with":         true,
	"extension":    true,
	"tld":          true,
}

func isStopWord(s string) bool {
	return queryStopWords[strings.ToLower(strings.TrimSpace(s))]
}

// IsTLD returns true if the string represents a TLD extension (e.g. ".com", "com", ".xyz").
func IsTLD(s string) bool {
	return knownTLDRegex.MatchString(strings.TrimSpace(s))
}

// NormalizeTLD formats an extension with a leading dot, e.g. ".com".
func NormalizeTLD(s string) string {
	clean := strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(clean, ".") {
		clean = "." + clean
	}
	return clean
}

// ExtractDomainBase extracts the main brand/project name from text, ignoring platforms.
func ExtractDomainBase(text string) string {
	// 1. Check if text contains a full domain (e.g. liegeagents.app)
	matches := domainInTextRegex.FindAllStringSubmatch(text, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		base := strings.ToLower(matches[i][1])
		if !ignoredPlatformBases[base] {
			return base
		}
	}

	// 2. Check if text is a domain command with bare name (e.g. "/domain liegeagents")
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(trimmed), "/domain") {
		parts := strings.Fields(trimmed)
		if len(parts) >= 2 {
			cand := strings.ToLower(strings.Trim(parts[1], `"'`+"`"+"()[]{}<>"))
			if idx := strings.Index(cand, "."); idx != -1 {
				cand = cand[:idx]
			}
			if len(cand) >= 2 && !IsTLD(cand) && !ignoredPlatformBases[cand] {
				return cand
			}
		}
	}

	return ""
}

// ExtractRequestedTLDs extracts any isolated TLD extensions in natural text (e.g. "how much is .com and .xyz").
func ExtractRequestedTLDs(text string) []string {
	matches := tldInQueryRegex.FindAllStringSubmatch(text, -1)
	var tlds []string
	seen := make(map[string]bool)
	for _, m := range matches {
		tld := strings.ToLower(m[0])
		if !seen[tld] {
			seen[tld] = true
			tlds = append(tlds, tld)
		}
	}
	return tlds
}

// ResolveDomainsWithBase resolves any bare TLDs against baseName.
func ResolveDomainsWithBase(items []string, baseName string) []string {
	var resolved []string
	seen := make(map[string]bool)

	for _, item := range items {
		clean := strings.TrimSpace(item)
		if clean == "" {
			continue
		}
		if IsTLD(clean) {
			if baseName != "" {
				full := baseName + NormalizeTLD(clean)
				if !seen[full] {
					seen[full] = true
					resolved = append(resolved, full)
				}
			}
		} else {
			if !seen[clean] {
				seen[clean] = true
				resolved = append(resolved, clean)
			}
		}
	}
	return resolved
}

type DomainResult struct {
	Domain       string  `json:"domain"`
	Available    bool    `json:"available"`
	Years        int     `json:"years,omitempty"`
	Price        float64 `json:"price,omitempty"`
	RenewalPrice float64 `json:"renewalPrice,omitempty"`
	Premium      bool    `json:"premium,omitempty"`
}

type SearchResponse struct {
	Results []DomainResult `json:"results"`
}

type Service struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

func NewService(token string) *Service {
	return &Service{
		endpoint: "https://api.vercel.com/v1/registrar/domains/search",
		token:    token,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// SetEndpointForTesting overrides the default Vercel API endpoint for unit tests.
func (s *Service) SetEndpointForTesting(endpoint string) {
	s.endpoint = endpoint
}

// ParseInputDomains parses user input (which can be a single domain, comma-separated list,
// space-separated list, bare brand name, or TLD extension queries) into domain candidates.
func (s *Service) ParseInputDomains(input string) []string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil
	}

	// Split by commas, newlines, or spaces
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})

	var fullDomains []string
	var bareTLDs []string
	var brandCandidates []string
	seen := make(map[string]bool)

	for _, f := range fields {
		item := strings.TrimSpace(f)
		item = strings.Trim(item, `"'`+"`"+"()[]{}<>@:,?!")
		if item == "" {
			continue
		}

		// Extract clean domain
		if m := domainCleanRegex.FindStringSubmatch(item); len(m) > 1 {
			item = m[1]
		}
		item = strings.ToLower(item)

		// Check if it has a TLD (contains a dot)
		if strings.Contains(item, ".") {
			if validDomainRegex.MatchString(item) && !seen[item] {
				seen[item] = true
				fullDomains = append(fullDomains, item)
			} else if IsTLD(item) {
				norm := NormalizeTLD(item)
				if !seen[norm] {
					seen[norm] = true
					bareTLDs = append(bareTLDs, norm)
				}
			}
		} else if IsTLD(item) {
			norm := NormalizeTLD(item)
			if !seen[norm] {
				seen[norm] = true
				bareTLDs = append(bareTLDs, norm)
			}
		} else {
			cleanName := strings.Trim(item, ".-_")
			if cleanName != "" && !isStopWord(cleanName) {
				brandCandidates = append(brandCandidates, cleanName)
			}
		}
	}

	// 1. If full domains found:
	if len(fullDomains) > 0 {
		// If user also specified bare TLDs alongside a full domain (e.g. "liegeagents.app, .com, .io")
		if len(bareTLDs) > 0 {
			if base := ExtractDomainBase(fullDomains[0]); base != "" {
				resolvedTLDs := ResolveDomainsWithBase(bareTLDs, base)
				for _, r := range resolvedTLDs {
					if !seen[r] {
						seen[r] = true
						fullDomains = append(fullDomains, r)
					}
				}
			}
		}
		if len(fullDomains) > 15 {
			fullDomains = fullDomains[:15]
		}
		return fullDomains
	}

	// 2. If both a brand name and bare TLDs are present in query (e.g. "curtainrh .com")
	if len(brandCandidates) > 0 && len(bareTLDs) > 0 {
		base := brandCandidates[0]
		return ResolveDomainsWithBase(bareTLDs, base)
	}

	// 3. If bare TLDs were directly specified (e.g. ".com", ".io")
	if len(bareTLDs) > 0 {
		return bareTLDs
	}

	// 4. Check for TLD extensions in natural text sentences (e.g. "how much is .com and .xyz")
	tldsInQuery := ExtractRequestedTLDs(raw)
	if len(tldsInQuery) > 0 {
		return tldsInQuery
	}

	// 5. Bare brand name without TLD: auto-expand across popular TLDs
	if len(brandCandidates) > 0 {
		var expanded []string
		for _, b := range brandCandidates {
			for _, tld := range defaultPopularTLDs {
				cand := b + tld
				if validDomainRegex.MatchString(cand) && !seen[cand] {
					seen[cand] = true
					expanded = append(expanded, cand)
				}
				if len(expanded) >= 15 {
					break
				}
			}
			if len(expanded) >= 15 {
				break
			}
		}
		return expanded
	}

	return nil
}

// Search searches Vercel registrar for domain availability and pricing.
func (s *Service) Search(ctx context.Context, input string) (*SearchResponse, error) {
	domains := s.ParseInputDomains(input)
	if len(domains) == 0 {
		return nil, fmt.Errorf("no valid domain names found in input")
	}
	return s.SearchDomains(ctx, domains)
}

// SearchDomains queries the Vercel Registrar API for specific domain names.
func (s *Service) SearchDomains(ctx context.Context, domains []string) (*SearchResponse, error) {
	if len(domains) == 0 {
		return nil, fmt.Errorf("domains list cannot be empty")
	}

	// Filter and validate domains: ensure no bare TLD extensions are passed to Vercel API
	var validDomains []string
	for _, d := range domains {
		clean := strings.TrimSpace(d)
		if clean == "" {
			continue
		}
		if IsTLD(clean) || strings.HasPrefix(clean, ".") || !strings.Contains(clean, ".") {
			continue
		}
		if validDomainRegex.MatchString(clean) {
			validDomains = append(validDomains, clean)
		}
	}

	if len(validDomains) == 0 {
		return nil, fmt.Errorf("no fully qualified domain names provided (found bare extensions: %s)", strings.Join(domains, ", "))
	}
	domains = validDomains

	if len(domains) > 20 {
		domains = domains[:20]
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"domains": domains,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vercel domain search request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read vercel response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vercel api returned error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var searchResp SearchResponse
	if err := json.Unmarshal(bodyBytes, &searchResp); err != nil {
		return nil, fmt.Errorf("failed to parse vercel response: %w", err)
	}

	return &searchResp, nil
}

// FormatResponse formats domain search results into clean, readable Telegram HTML.
// Adheres strictly to formatting guidelines: zero emojis, no em dashes, clean typography.
func (s *Service) FormatResponse(resp *SearchResponse) string {
	if resp == nil || len(resp.Results) == 0 {
		return "no domain results returned from Vercel."
	}

	var availableList []string
	var unavailableList []string

	for _, r := range resp.Results {
		if r.Available {
			yrsStr := "yr"
			if r.Years > 1 {
				yrsStr = fmt.Sprintf("%d yrs", r.Years)
			}

			priceInfo := fmt.Sprintf("$%.2f/%s", r.Price, yrsStr)
			if r.RenewalPrice > 0 && r.RenewalPrice != r.Price {
				priceInfo += fmt.Sprintf(" (renew $%.2f)", r.RenewalPrice)
			}
			if r.Premium {
				priceInfo += " [premium]"
			}

			availableList = append(availableList, fmt.Sprintf("• <b>%s</b> - Available: %s", r.Domain, priceInfo))
		} else {
			unavailableList = append(unavailableList, fmt.Sprintf("• <b>%s</b> - Taken", r.Domain))
		}
	}

	var sb strings.Builder
	sb.WriteString("<b>Vercel Domain Search:</b>\n\n")

	if len(availableList) > 0 {
		sb.WriteString(strings.Join(availableList, "\n"))
	}

	if len(unavailableList) > 0 {
		if len(availableList) > 0 {
			sb.WriteString("\n\n<b>Unavailable:</b>\n")
		} else {
			sb.WriteString("<b>Unavailable (Taken):</b>\n")
		}
		sb.WriteString(strings.Join(unavailableList, "\n"))
	}

	return sb.String()
}
