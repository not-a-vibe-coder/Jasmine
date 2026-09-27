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
// space-separated list, or bare brand name) into a list of full domains with TLDs.
func (s *Service) ParseInputDomains(input string) []string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil
	}

	// Split by commas, newlines, or spaces
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})

	var domains []string
	seen := make(map[string]bool)

	for _, f := range fields {
		item := strings.TrimSpace(f)
		item = strings.Trim(item, `"'` + "`" + `()[]{}<>`)
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
				domains = append(domains, item)
			}
		} else {
			// Bare name without TLD: auto-expand across popular TLDs
			cleanName := strings.Trim(item, ".-_")
			if cleanName != "" {
				for _, tld := range defaultPopularTLDs {
					cand := cleanName + tld
					if validDomainRegex.MatchString(cand) && !seen[cand] {
						seen[cand] = true
						domains = append(domains, cand)
					}
				}
			}
		}

		// Cap maximum query items at 15
		if len(domains) >= 15 {
			break
		}
	}

	return domains
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
