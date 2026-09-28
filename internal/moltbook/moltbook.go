package moltbook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://www.moltbook.com/api/v1"

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) IsConfigured() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

type AgentStatus struct {
	Success  bool   `json:"success"`
	Status   string `json:"status"` // "pending_claim", "claimed"
	Message  string `json:"message"`
	ClaimURL string `json:"claim_url"`
}

func (c *Client) CheckStatus(ctx context.Context) (*AgentStatus, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("moltbook client not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/agents/status", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var st AgentStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

type AgentProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Karma       int    `json:"karma"`
	IsClaimed   bool   `json:"is_claimed"`
}

type MeResponse struct {
	Success bool         `json:"success"`
	Agent   AgentProfile `json:"agent"`
}

func (c *Client) GetMe(ctx context.Context) (*AgentProfile, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("moltbook client not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/agents/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get profile (status %d): %s", resp.StatusCode, string(b))
	}

	var res MeResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res.Agent, nil
}

func (c *Client) UpdateDescription(ctx context.Context, description string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("moltbook client not configured")
	}

	payload := map[string]string{"description": description}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/agents/me", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to update description (status %d): %s", resp.StatusCode, string(b))
	}
	return nil
}

type VerificationInfo struct {
	VerificationCode string `json:"verification_code"`
	ChallengeText    string `json:"challenge_text"`
	Instructions     string `json:"instructions"`
	ExpiresAt        string `json:"expires_at"`
}

type Post struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Content            string            `json:"content"`
	SubmoltName        string            `json:"submolt_name"`
	Upvotes            int               `json:"upvotes"`
	Downvotes          int               `json:"downvotes"`
	VerificationStatus string            `json:"verification_status"`
	Verification       *VerificationInfo `json:"verification,omitempty"`
	Author             struct {
		Name string `json:"name"`
	} `json:"author"`
}

type PostResponse struct {
	Success              bool              `json:"success"`
	Message              string            `json:"message"`
	VerificationRequired bool              `json:"verification_required"`
	Post                 *Post             `json:"post"`
	Verification         *VerificationInfo `json:"verification,omitempty"`
}

type FeedResponse struct {
	Success bool   `json:"success"`
	Posts   []Post `json:"posts"`
	Results []Post `json:"results"` // some endpoints return results array
	HasMore bool   `json:"has_more"`
}

func (c *Client) GetFeed(ctx context.Context, sort string, limit int) ([]Post, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("moltbook client not configured")
	}
	if sort == "" {
		sort = "hot"
	}
	if limit <= 0 {
		limit = 15
	}

	url := fmt.Sprintf("%s/posts?sort=%s&limit=%d", c.baseURL, sort, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get feed (status %d): %s", resp.StatusCode, string(b))
	}

	var feed FeedResponse
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}
	if len(feed.Posts) > 0 {
		return feed.Posts, nil
	}
	return feed.Results, nil
}

func (c *Client) CreatePost(ctx context.Context, submolt, title, content string) (*PostResponse, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("moltbook client not configured")
	}
	if submolt == "" {
		submolt = "general"
	}

	payload := map[string]string{
		"submolt_name": submolt,
		"title":        title,
		"content":      content,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/posts", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("failed to create post (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var res PostResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type CommentResponse struct {
	Success              bool              `json:"success"`
	Message              string            `json:"message"`
	VerificationRequired bool              `json:"verification_required"`
	Verification         *VerificationInfo `json:"verification,omitempty"`
}

func (c *Client) CreateComment(ctx context.Context, postID, content string) (*CommentResponse, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("moltbook client not configured")
	}

	payload := map[string]string{"content": content}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/posts/%s/comments", c.baseURL, postID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("failed to comment (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var res CommentResponse
	_ = json.Unmarshal(bodyBytes, &res)
	return &res, nil
}

func (c *Client) UpvotePost(ctx context.Context, postID string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("moltbook client not configured")
	}

	url := fmt.Sprintf("%s/posts/%s/upvote", c.baseURL, postID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to upvote (status %d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) VerifyChallenge(ctx context.Context, verificationCode, answer string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("moltbook client not configured")
	}

	payload := map[string]string{
		"verification_code": verificationCode,
		"answer":            answer,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/verify", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("verification rejected (status %d): %s", resp.StatusCode, string(b))
	}
	return nil
}
