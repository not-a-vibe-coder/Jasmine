package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Service struct {
	httpClient   *http.Client
	tavilyAPIKey string
}

func NewService(tavilyAPIKey string) *Service {
	return &Service{
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tavilyAPIKey: tavilyAPIKey,
	}
}

type ddgResponse struct {
	Heading       string `json:"Heading"`
	AbstractText  string `json:"AbstractText"`
	AbstractURL   string `json:"AbstractURL"`
	RelatedTopics []struct {
		Text     string `json:"Text"`
		FirstURL string `json:"FirstURL"`
	} `json:"RelatedTopics"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
}

// Search queries DuckDuckGo (and Tavily if API key configured) with Wikipedia fallback
func (s *Service) Search(ctx context.Context, query string) (string, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return "Please provide a search query.", nil
	}

	// 1. If Tavily API Key is configured, use Tavily for high-depth search
	if s.tavilyAPIKey != "" {
		res, err := s.searchTavily(ctx, cleanQuery)
		if err == nil && res != "" {
			return res, nil
		}
	}

	// 2. Query DuckDuckGo Instant Answer API
	res, err := s.searchDuckDuckGo(ctx, cleanQuery)
	if err == nil && res != "" {
		return res, nil
	}

	// 3. Fallback to Wikipedia Knowledge Search
	res, err = s.searchWikipedia(ctx, cleanQuery)
	if err == nil && res != "" {
		return res, nil
	}

	return fmt.Sprintf("No relevant live search results found for: %s", cleanQuery), nil
}

func (s *Service) searchDuckDuckGo(ctx context.Context, query string) (string, error) {
	apiURL := fmt.Sprintf("https://api.duckduckgo.com/?q=%s&format=json&no_html=1&skip_disambig=1", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ShippBot/1.0 (https://bot.davidnzube.xyz)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("duckduckgo status %d", resp.StatusCode)
	}

	var ddg ddgResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	_ = json.Unmarshal(body, &ddg)

	if ddg.AbstractText != "" {
		res := fmt.Sprintf("**%s**\n%s", ddg.Heading, ddg.AbstractText)
		if ddg.AbstractURL != "" {
			res += fmt.Sprintf("\nSource: %s", ddg.AbstractURL)
		}
		return res, nil
	}

	var snippets []string
	for _, topic := range ddg.RelatedTopics {
		if topic.Text != "" {
			snippets = append(snippets, "• "+topic.Text)
			if len(snippets) >= 3 {
				break
			}
		}
	}
	if len(snippets) > 0 {
		return strings.Join(snippets, "\n"), nil
	}

	return "", fmt.Errorf("no direct duckduckgo answer")
}

func (s *Service) searchWikipedia(ctx context.Context, query string) (string, error) {
	searchURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=opensearch&search=%s&limit=2&namespace=0&format=json", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ShippBot/1.0 (https://bot.davidnzube.xyz)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data) < 4 {
		return "", fmt.Errorf("wikipedia opensearch decode failed")
	}

	titles, ok := data[1].([]interface{})
	if !ok || len(titles) == 0 {
		return "", fmt.Errorf("no wikipedia articles matched")
	}

	firstTitle, ok := titles[0].(string)
	if !ok || firstTitle == "" {
		return "", fmt.Errorf("invalid wikipedia title")
	}

	summaryURL := fmt.Sprintf("https://en.wikipedia.org/api/rest_v1/page/summary/%s", url.PathEscape(firstTitle))
	reqSum, err := http.NewRequestWithContext(ctx, "GET", summaryURL, nil)
	if err != nil {
		return "", err
	}
	reqSum.Header.Set("User-Agent", "ShippBot/1.0 (https://bot.davidnzube.xyz)")

	respSum, err := s.httpClient.Do(reqSum)
	if err != nil {
		return "", err
	}
	defer respSum.Body.Close()

	var page struct {
		Title   string `json:"title"`
		Extract string `json:"extract"`
		ContentURLs struct {
			Desktop struct {
				Page string `json:"page"`
			} `json:"desktop"`
		} `json:"content_urls"`
	}
	if err := json.NewDecoder(respSum.Body).Decode(&page); err == nil && page.Extract != "" {
		return fmt.Sprintf("**%s**\n%s\nSource: %s", page.Title, page.Extract, page.ContentURLs.Desktop.Page), nil
	}

	return "", fmt.Errorf("no summary extracted")
}

func (s *Service) searchTavily(ctx context.Context, query string) (string, error) {
	reqBody := map[string]interface{}{
		"api_key":             s.tavilyAPIKey,
		"query":               query,
		"search_depth":        "basic",
		"include_answer":      true,
		"max_results":         3,
	}
	bodyData, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewBuffer(bodyData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tavily status %d", resp.StatusCode)
	}

	var tavily tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&tavily); err != nil {
		return "", err
	}

	var sb strings.Builder
	for i, r := range tavily.Results {
		sb.WriteString(fmt.Sprintf("%d. **%s**\n%s\nSource: %s\n\n", i+1, r.Title, r.Content, r.URL))
	}
	return strings.TrimSpace(sb.String()), nil
}
