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

var (
	htmlTagRegex     = regexp.MustCompile(`<[^>]*>`)
	urlOrDomainRegex = regexp.MustCompile(`(?i)\b(?:https?://)?(?:[a-zA-Z0-9-]+\.)+(?:com|org|net|io|xyz|app|dev|co|ai|me|cc|sh|so|gg|tech|network|finance|fun|link|site|online|store|world)\b(?:/[^\s]*)?`)
	titleTagRegex    = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	metaDescRegex    = regexp.MustCompile(`(?i)<meta\s+[^>]*name=["']description["'][^>]*content=["']([^"']+)["']|<meta\s+[^>]*content=["']([^"']+)["'][^>]*name=["']description["']`)
	ogTitleRegex     = regexp.MustCompile(`(?i)<meta\s+[^>]*property=["']og:title["'][^>]*content=["']([^"']+)["']|<meta\s+[^>]*content=["']([^"']+)["'][^>]*property=["']og:title["']`)
	ogDescRegex      = regexp.MustCompile(`(?i)<meta\s+[^>]*property=["']og:description["'][^>]*content=["']([^"']+)["']|<meta\s+[^>]*content=["']([^"']+)["'][^>]*property=["']og:description["']`)
	scriptTagRegex   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleTagRegex    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	svgTagRegex      = regexp.MustCompile(`(?is)<svg[^>]*>.*?</svg>`)
	headTagRegex     = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`)
	whitespaceRegex  = regexp.MustCompile(`\s+`)
)

func stripHTML(s string) string {
	clean := htmlTagRegex.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(clean))
}

// FetchWebPage fetches and extracts structured metadata, title, description, and visible text from any URL or domain.
func (s *Service) FetchWebPage(ctx context.Context, rawURL string) (string, error) {
	targetURL := strings.TrimSpace(rawURL)
	if targetURL == "" {
		return "Please provide a valid URL or domain.", nil
	}
	if !strings.HasPrefix(strings.ToLower(targetURL), "http://") && !strings.HasPrefix(strings.ToLower(targetURL), "https://") {
		targetURL = "https://" + targetURL
	}

	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Host == "" {
		return fmt.Sprintf("Invalid URL or domain format: %s", rawURL), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Sprintf("Failed to build request for %s: %v", targetURL, err), nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Failed to fetch webpage %s: %v", targetURL, err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Sprintf("Webpage %s returned HTTP status %d (%s)", targetURL, resp.StatusCode, resp.Status), nil
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return fmt.Sprintf("Error reading content from %s: %v", targetURL, err), nil
	}
	htmlStr := string(bodyBytes)

	// Extract title
	pageTitle := ""
	if m := titleTagRegex.FindStringSubmatch(htmlStr); len(m) > 1 {
		pageTitle = stripHTML(m[1])
	}

	// Extract description
	pageDesc := ""
	if m := metaDescRegex.FindStringSubmatch(htmlStr); len(m) > 0 {
		for _, v := range m[1:] {
			if v != "" {
				pageDesc = stripHTML(v)
				break
			}
		}
	}
	if pageDesc == "" {
		if m := ogDescRegex.FindStringSubmatch(htmlStr); len(m) > 0 {
			for _, v := range m[1:] {
				if v != "" {
					pageDesc = stripHTML(v)
					break
				}
			}
		}
	}

	if pageTitle == "" {
		if m := ogTitleRegex.FindStringSubmatch(htmlStr); len(m) > 0 {
			for _, v := range m[1:] {
				if v != "" {
					pageTitle = stripHTML(v)
					break
				}
			}
		}
	}

	// Clean body text
	bodyOnly := headTagRegex.ReplaceAllString(htmlStr, "")
	bodyOnly = scriptTagRegex.ReplaceAllString(bodyOnly, "")
	bodyOnly = styleTagRegex.ReplaceAllString(bodyOnly, "")
	bodyOnly = svgTagRegex.ReplaceAllString(bodyOnly, "")
	cleanText := stripHTML(bodyOnly)
	cleanText = whitespaceRegex.ReplaceAllString(cleanText, " ")
	cleanText = strings.TrimSpace(cleanText)

	if len(cleanText) > 2500 {
		cleanText = cleanText[:2500] + "..."
	}

	var sb strings.Builder
	finalURL := targetURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	sb.WriteString(fmt.Sprintf("[Webpage: %s]\n", finalURL))
	if pageTitle != "" {
		sb.WriteString(fmt.Sprintf("Title: %s\n", pageTitle))
	}
	if pageDesc != "" {
		sb.WriteString(fmt.Sprintf("Description: %s\n", pageDesc))
	}
	if cleanText != "" {
		sb.WriteString(fmt.Sprintf("\nContent Summary / Text:\n%s\n", cleanText))
	}

	return strings.TrimSpace(sb.String()), nil
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

	// 2. Proactive domain/URL fetch: if query contains a domain or URL, fetch live website content!
	if domainMatch := urlOrDomainRegex.FindString(cleanQuery); domainMatch != "" {
		pageContent, err := s.FetchWebPage(ctx, domainMatch)
		if err == nil && pageContent != "" && !strings.Contains(pageContent, "returned HTTP status 4") {
			sections = append(sections, fmt.Sprintf("[Live Website Content]\n%s", pageContent))
		}
	}

	// 3. Query Google News RSS for real-time news & current events
	newsResults, err := s.searchGoogleNews(ctx, cleanQuery)
	if err == nil && newsResults != "" {
		sections = append(sections, fmt.Sprintf("[Recent News & Real-Time Intel]\n%s", newsResults))
	}

	// 4. Query Wikipedia full-text search for factual/historical context
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
