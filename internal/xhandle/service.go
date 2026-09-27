package xhandle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type HandleStatus string

const (
	StatusAvailable   HandleStatus = "available"
	StatusTaken       HandleStatus = "taken"
	StatusReserved    HandleStatus = "reserved"
	StatusTooLong     HandleStatus = "too_long"
	StatusTooShort    HandleStatus = "too_short"
	StatusInvalidChar HandleStatus = "invalid_format"
	StatusError       HandleStatus = "error"
)

type HandleResult struct {
	Username  string       `json:"username"`
	Available bool         `json:"available"`
	Status    HandleStatus `json:"status"`
	Reason    string       `json:"reason"`
	Message   string       `json:"message"`
}

type twitterAPIResponse struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason"`
	Msg    string `json:"msg"`
	Desc   string `json:"desc"`
}

var validHandleRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{1,15}$`)
var urlExtractRegex = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?(?:twitter\.com|x\.com)/@?([a-zA-Z0-9_]+)`)

var xStopWords = map[string]bool{
	"is":          true,
	"are":         true,
	"available":   true,
	"on":          true,
	"x":           true,
	"twitter":     true,
	"handle":      true,
	"handles":     true,
	"username":    true,
	"usernames":   true,
	"check":       true,
	"for":         true,
	"the":         true,
	"a":           true,
	"an":          true,
	"can":         true,
	"you":         true,
	"shipp":       true,
	"shipp0bot":   true,
	"bot":         true,
	"find":        true,
	"get":         true,
	"account":     true,
	"accounts":    true,
	"what":        true,
	"about":       true,
	"how":         true,
	"much":        true,
	"does":        true,
	"it":          true,
	"exist":       true,
	"taken":       true,
	"free":        true,
	"to":          true,
	"make":        true,
	"create":      true,
}

type Service struct {
	apiEndpoint string
	webEndpoint string
	httpClient  *http.Client
}

func NewService() *Service {
	return &Service{
		apiEndpoint: "https://api.twitter.com/i/users/username_available.json?username=",
		webEndpoint: "https://x.com/",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SetEndpointsForTesting overrides API and web endpoints for unit testing.
func (s *Service) SetEndpointsForTesting(apiEndpoint, webEndpoint string) {
	s.apiEndpoint = apiEndpoint
	s.webEndpoint = webEndpoint
}

// NormalizeHandle strips '@', URL prefixes, spaces, and punctuation from a username.
func NormalizeHandle(raw string) string {
	clean := strings.TrimSpace(raw)
	clean = strings.Trim(clean, `"'`+"`"+"()[]{}<>,?!:;")

	// Check if full URL (e.g. https://x.com/liegeagents or twitter.com/liegeagents)
	if m := urlExtractRegex.FindStringSubmatch(clean); len(m) > 1 {
		clean = m[1]
	}

	clean = strings.TrimPrefix(clean, "@")
	clean = strings.Trim(clean, "/?#")
	return clean
}

// ValidateFormat checks if a handle meets X's format requirements (1-15 chars, alphanumeric + underscore).
func ValidateFormat(handle string) (bool, HandleStatus, string) {
	if len(handle) == 0 {
		return false, StatusTooShort, "Username is empty"
	}
	if len(handle) > 15 {
		return false, StatusTooLong, "Too long (max 15 chars)"
	}
	if !validHandleRegex.MatchString(handle) {
		return false, StatusInvalidChar, "Only letters, numbers, and '_' allowed"
	}
	return true, "", ""
}

// ParseInputHandles extracts valid candidate usernames from natural text or command arguments.
func (s *Service) ParseInputHandles(input string) []string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil
	}

	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})

	var candidates []string
	seen := make(map[string]bool)

	for _, f := range fields {
		item := NormalizeHandle(f)
		if item == "" {
			continue
		}

		lower := strings.ToLower(item)
		if xStopWords[lower] {
			continue
		}

		// Keep original case or lowercase for Twitter handles (handles are case-insensitive on X)
		clean := strings.ToLower(item)
		if !seen[clean] {
			seen[clean] = true
			candidates = append(candidates, item)
		}

		if len(candidates) >= 10 {
			break
		}
	}

	return candidates
}

// CheckHandle checks the availability of a single X handle.
func (s *Service) CheckHandle(ctx context.Context, rawHandle string) (*HandleResult, error) {
	username := NormalizeHandle(rawHandle)

	// Pre-API format validation
	if ok, status, errMsg := ValidateFormat(username); !ok {
		return &HandleResult{
			Username:  username,
			Available: false,
			Status:    status,
			Reason:    string(status),
			Message:   errMsg,
		}, nil
	}

	reqURL := s.apiEndpoint
	if strings.Contains(reqURL, "?username=") {
		reqURL = reqURL + url.QueryEscape(username)
	} else if strings.HasSuffix(reqURL, "/") {
		reqURL = reqURL + url.QueryEscape(username)
	} else {
		reqURL = fmt.Sprintf("%s?username=%s", reqURL, url.QueryEscape(username))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Fallback to web endpoint if API network call fails
		return s.checkWebFallback(ctx, username)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return s.checkWebFallback(ctx, username)
		}

		var apiResp twitterAPIResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return s.checkWebFallback(ctx, username)
		}

		if apiResp.Valid {
			return &HandleResult{
				Username:  username,
				Available: true,
				Status:    StatusAvailable,
				Reason:    apiResp.Reason,
				Message:   "Available",
			}, nil
		}

		// Handle unavailable reasons
		switch apiResp.Reason {
		case "taken":
			return &HandleResult{
				Username:  username,
				Available: false,
				Status:    StatusTaken,
				Reason:    apiResp.Reason,
				Message:   "Taken",
			}, nil
		case "contains_banned_word":
			return &HandleResult{
				Username:  username,
				Available: false,
				Status:    StatusReserved,
				Reason:    apiResp.Reason,
				Message:   "Reserved (Unavailable)",
			}, nil
		case "invalid_username":
			return &HandleResult{
				Username:  username,
				Available: false,
				Status:    StatusTooLong,
				Reason:    apiResp.Reason,
				Message:   apiResp.Msg,
			}, nil
		case "improper_format":
			return &HandleResult{
				Username:  username,
				Available: false,
				Status:    StatusInvalidChar,
				Reason:    apiResp.Reason,
				Message:   apiResp.Msg,
			}, nil
		default:
			msg := apiResp.Msg
			if msg == "" {
				msg = "Unavailable"
			}
			return &HandleResult{
				Username:  username,
				Available: false,
				Status:    StatusTaken,
				Reason:    apiResp.Reason,
				Message:   msg,
			}, nil
		}
	}

	// If API returned 404 or other status, try web fallback
	return s.checkWebFallback(ctx, username)
}

// checkWebFallback checks https://x.com/<handle> via HTTP GET / HEAD as a secondary fallback.
func (s *Service) checkWebFallback(ctx context.Context, username string) (*HandleResult, error) {
	reqURL := s.webEndpoint
	if !strings.HasSuffix(reqURL, "/") {
		reqURL += "/"
	}
	reqURL += url.PathEscape(username)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create fallback request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("x handle check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &HandleResult{
			Username:  username,
			Available: true,
			Status:    StatusAvailable,
			Reason:    "not_found",
			Message:   "Available",
		}, nil
	}

	if resp.StatusCode == http.StatusOK {
		return &HandleResult{
			Username:  username,
			Available: false,
			Status:    StatusTaken,
			Reason:    "profile_exists",
			Message:   "Taken",
		}, nil
	}

	return &HandleResult{
		Username:  username,
		Available: false,
		Status:    StatusError,
		Reason:    fmt.Sprintf("http_%d", resp.StatusCode),
		Message:   fmt.Sprintf("Status %d", resp.StatusCode),
	}, nil
}

// CheckHandles checks availability for a slice of usernames concurrently.
func (s *Service) CheckHandles(ctx context.Context, usernames []string) ([]HandleResult, error) {
	if len(usernames) == 0 {
		return nil, fmt.Errorf("no handles provided")
	}

	if len(usernames) > 10 {
		usernames = usernames[:10]
	}

	results := make([]HandleResult, len(usernames))
	var wg sync.WaitGroup

	for i, u := range usernames {
		wg.Add(1)
		go func(idx int, handle string) {
			defer wg.Done()
			res, err := s.CheckHandle(ctx, handle)
			if err != nil {
				results[idx] = HandleResult{
					Username:  handle,
					Available: false,
					Status:    StatusError,
					Reason:    "error",
					Message:   err.Error(),
				}
				return
			}
			results[idx] = *res
		}(i, u)
	}

	wg.Wait()
	return results, nil
}

// FormatResponse formats X handle check results into clean, readable Telegram HTML.
// Strictly adheres to formatting rules: zero emojis, no em dashes, clean typography.
func (s *Service) FormatResponse(results []HandleResult) string {
	if len(results) == 0 {
		return "no X handles checked."
	}

	var availableList []string
	var unavailableList []string

	for _, r := range results {
		display := "@" + r.Username
		if r.Available {
			availableList = append(availableList, fmt.Sprintf("• <b>%s</b> - Available", display))
		} else {
			reasonStr := r.Message
			if reasonStr == "" {
				reasonStr = "Unavailable"
			}
			unavailableList = append(unavailableList, fmt.Sprintf("• <b>%s</b> - %s", display, reasonStr))
		}
	}

	var sb strings.Builder
	sb.WriteString("<b>X (Twitter) Handle Availability:</b>\n\n")

	if len(availableList) > 0 {
		sb.WriteString(strings.Join(availableList, "\n"))
	}

	if len(unavailableList) > 0 {
		if len(availableList) > 0 {
			sb.WriteString("\n\n<b>Unavailable:</b>\n")
		}
		sb.WriteString(strings.Join(unavailableList, "\n"))
	}

	return sb.String()
}
