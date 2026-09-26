package search

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Service struct {
	httpClient   *http.Client
	tavilyAPIKey string
}

func NewService(tavilyAPIKey string) *Service {
	return &Service{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		tavilyAPIKey: tavilyAPIKey,
	}
}

type googleNewsItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	PubDate string `xml:"pubDate"`
	Source  struct {
		URL  string `xml:"url,attr"`
		Name string `xml:",chardata"`
	} `xml:"source"`
}

type googleNewsRSS struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []googleNewsItem `xml:"item"`
	} `xml:"channel"`
}

type wikiSearchResponse struct {
	Query struct {
		Search []struct {
			Title     string `json:"title"`
			Snippet   string `json:"snippet"`
			Timestamp string `json:"timestamp"`
		} `json:"search"`
	} `json:"query"`
}

type wikiSummaryResponse struct {
	Title       string `json:"title"`
	Extract     string `json:"extract"`
	Description string `json:"description"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
}

var htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	clean := htmlTagRegex.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(clean))
}

// Search coordinates multi-source real-time search across Google News, Wikipedia full-text, and Tavily
func (s *Service) Search(ctx context.Context, query string) (string, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return "Please provide a search query.", nil
	}

	// 1. Tavily if API key is provided
	if s.tavilyAPIKey != "" {
		res, err := s.searchTavily(ctx, cleanQuery)
		if err == nil && res != "" {
			return res, nil
		}
	}

	var sections []string

	// 2. Query Google News RSS for real-time news & current events
	newsResults, err := s.searchGoogleNews(ctx, cleanQuery)
	if err == nil && newsResults != "" {
		sections = append(sections, fmt.Sprintf("[Recent News & Real-Time Intel]\n%s", newsResults))
	}

	// 3. Query Wikipedia full-text search for factual/historical context
	wikiResults, err := s.searchWikipediaFullText(ctx, cleanQuery)
	if err == nil && wikiResults != "" {
		sections = append(sections, fmt.Sprintf("[Background & Encyclopedia Context]\n%s", wikiResults))
	}

	// 4. If both returned empty, try simplified keywords
	if len(sections) == 0 {
		simplified := simplifyQuery(cleanQuery)
		if simplified != cleanQuery && simplified != "" {
			newsFallback, _ := s.searchGoogleNews(ctx, simplified)
			if newsFallback != "" {
				sections = append(sections, fmt.Sprintf("[Recent News & Real-Time Intel]\n%s", newsFallback))
			} else {
				wikiFallback, _ := s.searchWikipediaFullText(ctx, simplified)
				if wikiFallback != "" {
					sections = append(sections, fmt.Sprintf("[Background & Encyclopedia Context]\n%s", wikiFallback))
				}
			}
		}
	}

	if len(sections) > 0 {
		return strings.Join(sections, "\n\n"), nil
	}

	return fmt.Sprintf("No relevant live search results found for: %s", cleanQuery), nil
}

func (s *Service) searchGoogleNews(ctx context.Context, query string) (string, error) {
	apiURL := fmt.Sprintf("https://news.google.com/rss/search?q=%s&hl=en-US&gl=US&ceid=US:en", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google news status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var rss googleNewsRSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return "", err
	}

	if len(rss.Channel.Items) == 0 {
		return "", fmt.Errorf("no news items found")
	}

	var sb strings.Builder
	limit := 4
	if len(rss.Channel.Items) < limit {
		limit = len(rss.Channel.Items)
	}

	for i := 0; i < limit; i++ {
		item := rss.Channel.Items[i]
		cleanTitle := stripHTML(item.Title)
		sourceName := stripHTML(item.Source.Name)
		dateStr := ""
		if t, err := time.Parse(time.RFC1123, item.PubDate); err == nil {
			dateStr = t.Format("02 Jan 2006")
		} else if t, err := time.Parse(time.RFC1123Z, item.PubDate); err == nil {
			dateStr = t.Format("02 Jan 2006")
		} else if len(item.PubDate) >= 16 {
			dateStr = item.PubDate[:16]
		}

		if sourceName != "" {
			sb.WriteString(fmt.Sprintf("%d. [%s] %s (Source: %s)\n", i+1, dateStr, cleanTitle, sourceName))
		} else {
			sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, dateStr, cleanTitle))
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

func (s *Service) searchWikipediaFullText(ctx context.Context, query string) (string, error) {
	searchURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&list=search&srsearch=%s&srlimit=3&utf8=&format=json", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
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
		return "", fmt.Errorf("wikipedia search status %d", resp.StatusCode)
	}

	var wikiResp wikiSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&wikiResp); err != nil {
		return "", err
	}

	results := wikiResp.Query.Search
	if len(results) == 0 {
		return "", fmt.Errorf("no wikipedia articles found")
	}

	var sb strings.Builder

	// Top result summary
	topTitle := results[0].Title
	summary, err := s.fetchWikiSummary(ctx, topTitle)
	if err == nil && summary != "" {
		sb.WriteString(fmt.Sprintf("• %s: %s\n\n", topTitle, summary))
	}

	// Related snippets
	for i, r := range results {
		cleanSnippet := stripHTML(r.Snippet)
		if cleanSnippet != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", r.Title, cleanSnippet))
		}
		if i >= 2 {
			break
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

func (s *Service) fetchWikiSummary(ctx context.Context, title string) (string, error) {
	summaryURL := fmt.Sprintf("https://en.wikipedia.org/api/rest_v1/page/summary/%s", url.PathEscape(title))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, summaryURL, nil)
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
		return "", fmt.Errorf("wiki summary status %d", resp.StatusCode)
	}

	var page wikiSummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return "", err
	}
	return page.Extract, nil
}

func (s *Service) searchTavily(ctx context.Context, query string) (string, error) {
	reqBody := map[string]interface{}{
		"api_key":        s.tavilyAPIKey,
		"query":          query,
		"search_depth":   "basic",
		"include_answer": true,
		"max_results":    3,
	}
	bodyData, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewBuffer(bodyData))
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

func simplifyQuery(query string) string {
	stopwords := []string{"why", "who is", "what is", "do not recognize", "does not recognize", "how to", "tell me about", "after", "the", "a", "an", "current", "explain"}
	lower := strings.ToLower(query)
	for _, sw := range stopwords {
		lower = strings.ReplaceAll(lower, sw, "")
	}
	fields := strings.Fields(lower)
	return strings.Join(fields, " ")
}
