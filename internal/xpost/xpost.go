package xpost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	tweetURLRegex = regexp.MustCompile(`(?i)https?://(?:www\.)?(?:x\.com|twitter\.com|fxtwitter\.com|vxtwitter\.com|fixupx\.com)/[a-zA-Z0-9_]{1,32}/status/(\d+)`)
	tweetStatusRegex = regexp.MustCompile(`(?i)https?://(?:api\.)?fxtwitter\.com/status/(\d+)`)
)

type FxTweetResponse struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Tweet   *FxTweet `json:"tweet"`
}

type FxTweet struct {
	URL        string   `json:"url"`
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	Author     FxAuthor `json:"author"`
	Replies    int64    `json:"replies"`
	Retweets   int64    `json:"retweets"`
	Likes      int64    `json:"likes"`
	Bookmarks  int64    `json:"bookmarks"`
	CreatedAt  string   `json:"created_at"`
}

type FxAuthor struct {
	Name       string `json:"name"`
	ScreenName string `json:"screen_name"`
}

type Service struct {
	httpClient *http.Client
}

func NewService() *Service {
	return &Service{
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ExtractTweetID parses a tweet status ID from an X/Twitter URL.
func ExtractTweetID(input string) string {
	input = strings.TrimSpace(input)
	if m := tweetURLRegex.FindStringSubmatch(input); len(m) > 1 {
		return m[1]
	}
	if m := tweetStatusRegex.FindStringSubmatch(input); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ExtractTweetURL finds any X/Twitter tweet URL embedded in a message.
func ExtractTweetURL(text string) string {
	if loc := tweetURLRegex.FindString(text); loc != "" {
		return loc
	}
	if loc := tweetStatusRegex.FindString(text); loc != "" {
		return loc
	}
	return ""
}

// FetchTweet fetches live post data using FxTwitter API.
func (s *Service) FetchTweet(ctx context.Context, urlOrID string) (*FxTweet, error) {
	tweetID := ExtractTweetID(urlOrID)
	if tweetID == "" {
		// If input is purely digits, treat it as an ID
		if regexp.MustCompile(`^\d{5,25}$`).MatchString(strings.TrimSpace(urlOrID)) {
			tweetID = strings.TrimSpace(urlOrID)
		} else {
			return nil, fmt.Errorf("invalid x/twitter url or id: %q", urlOrID)
		}
	}

	apiURL := fmt.Sprintf("https://api.fxtwitter.com/status/%s", tweetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "ShippBot/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach fxtwitter api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fxtwitter api returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var fxResp FxTweetResponse
	if err := json.Unmarshal(body, &fxResp); err != nil {
		return nil, fmt.Errorf("failed to parse tweet json: %w", err)
	}

	if fxResp.Tweet == nil || fxResp.Code != 200 {
		return nil, fmt.Errorf("tweet not found or unavailable (%s)", fxResp.Message)
	}

	return fxResp.Tweet, nil
}

// FormatTweet creates a clean dev-rhythm summary of a tweet.
func FormatTweet(tweet *FxTweet) string {
	if tweet == nil {
		return "Tweet not found or unavailable."
	}

	author := tweet.Author.ScreenName
	if author == "" {
		author = "unknown"
	}
	name := tweet.Author.Name
	if name == "" {
		name = author
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("X post by @%s (%s):\n", author, name))
	sb.WriteString(fmt.Sprintf("\"%s\"\n", strings.TrimSpace(tweet.Text)))

	var stats []string
	if tweet.Likes > 0 {
		stats = append(stats, fmt.Sprintf("%d likes", tweet.Likes))
	}
	if tweet.Retweets > 0 {
		stats = append(stats, fmt.Sprintf("%d reposts", tweet.Retweets))
	}
	if tweet.Replies > 0 {
		stats = append(stats, fmt.Sprintf("%d replies", tweet.Replies))
	}
	if len(stats) > 0 {
		sb.WriteString(fmt.Sprintf("(%s)", strings.Join(stats, " - ")))
	}

	return strings.TrimSpace(sb.String())
}
