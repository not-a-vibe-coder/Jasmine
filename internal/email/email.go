package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

type Service struct {
	apiKey       string
	fromEmail    string
	replyToEmail string
	baseURL      string
	httpClient   *http.Client
}

type SendResult struct {
	ID           string `json:"id"`
	To           string `json:"to"`
	Subject      string `json:"subject"`
	From         string `json:"from"`
	ReplyTo      string `json:"reply_to,omitempty"`
}

func NewService(apiKey, fromEmail, replyToEmail string) *Service {
	from := strings.TrimSpace(fromEmail)
	if from == "" {
		from = "Shipp <shipp@bot.davidnzube.xyz>"
	}
	return &Service{
		apiKey:       strings.TrimSpace(apiKey),
		fromEmail:    from,
		replyToEmail: strings.TrimSpace(replyToEmail),
		baseURL:      "https://api.resend.com",
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// IsConfigured returns true if Resend API key is configured
func (s *Service) IsConfigured() bool {
	return s.apiKey != ""
}

// Send sends an email via Resend
func (s *Service) Send(ctx context.Context, to, subject, body string) (*SendResult, error) {
	if !s.IsConfigured() {
		return nil, fmt.Errorf("resend email service is not configured (missing RESEND_API_KEY)")
	}

	cleanTo := strings.TrimSpace(to)
	if cleanTo == "" {
		return nil, fmt.Errorf("recipient email address is required")
	}

	// Validate recipient email syntax
	if _, err := mail.ParseAddress(cleanTo); err != nil {
		return nil, fmt.Errorf("invalid recipient email address '%s': %w", cleanTo, err)
	}

	cleanSubj := strings.TrimSpace(subject)
	if cleanSubj == "" {
		cleanSubj = "(No Subject)"
	}

	cleanBody := strings.TrimSpace(body)
	if cleanBody == "" {
		cleanBody = "(Empty message body)"
	}

	// Simple markdown to HTML conversion for paragraphs and line breaks
	htmlBody := formatHTMLBody(cleanBody)

	payload := map[string]interface{}{
		"from":    s.fromEmail,
		"to":      []string{cleanTo},
		"subject": cleanSubj,
		"text":    cleanBody,
		"html":    htmlBody,
	}

	if s.replyToEmail != "" {
		payload["reply_to"] = s.replyToEmail
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal email payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/emails", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errData struct {
			Message string `json:"message"`
			Name    string `json:"name"`
		}
		_ = json.Unmarshal(respBytes, &errData)
		if errData.Message != "" {
			return nil, fmt.Errorf("resend error (%s): %s", errData.Name, errData.Message)
		}
		return nil, fmt.Errorf("resend http %d: %s", resp.StatusCode, string(respBytes))
	}

	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return nil, fmt.Errorf("failed to parse resend response: %w", err)
	}

	return &SendResult{
		ID:      res.ID,
		To:      cleanTo,
		Subject: cleanSubj,
		From:    s.fromEmail,
		ReplyTo: s.replyToEmail,
	}, nil
}

func formatHTMLBody(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	var htmlParts []string
	for _, p := range paragraphs {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		// Convert single newlines inside paragraph to <br/>
		escaped := strings.ReplaceAll(trimmed, "\n", "<br/>")
		htmlParts = append(htmlParts, fmt.Sprintf("<p style=\"font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; font-size: 15px; line-height: 1.6; color: #1a1a1a;\">%s</p>", escaped))
	}
	if len(htmlParts) == 0 {
		return fmt.Sprintf("<p>%s</p>", text)
	}
	return strings.Join(htmlParts, "\n")
}

// FormatEmailSent formats a confirmation message for Telegram
func FormatEmailSent(res *SendResult) string {
	if res == nil {
		return "Sent that email."
	}
	return fmt.Sprintf("Sent that email to %s (subject: '%s').", res.To, res.Subject)
}
