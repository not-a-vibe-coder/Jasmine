// Package imagegen turns text prompts into images. Pollinations (free, no key needed)
// is tried first; Gemini's native image model is the fallback when a key is set.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultPollinationsURL = "https://image.pollinations.ai/prompt/"
	DefaultGeminiModel     = "gemini-2.5-flash-image"
	maxImageBytes          = 15 << 20
)

type Image struct {
	Data     []byte
	MIMEType string
	Provider string
}

type Service struct {
	pollinationsURL string
	pollinationsKey string
	geminiKey       string
	geminiModel     string
	geminiBaseURL   string
	httpClient      *http.Client
}

func NewService(pollinationsKey, geminiKey, geminiModel string) *Service {
	if geminiModel == "" {
		geminiModel = DefaultGeminiModel
	}
	return &Service{
		pollinationsURL: DefaultPollinationsURL,
		pollinationsKey: pollinationsKey,
		geminiKey:       geminiKey,
		geminiModel:     geminiModel,
		geminiBaseURL:   "https://generativelanguage.googleapis.com/v1beta/models/",
		httpClient:      &http.Client{Timeout: 90 * time.Second},
	}
}

// SetBaseURLs points the service at test servers.
func (s *Service) SetBaseURLs(pollinations, gemini string) {
	s.pollinationsURL = pollinations
	s.geminiBaseURL = gemini
}

// Generate returns the first image any provider produces for the prompt.
func (s *Service) Generate(ctx context.Context, prompt string) (*Image, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("empty image prompt")
	}
	img, err := s.pollinations(ctx, prompt)
	if err == nil {
		return img, nil
	}
	if s.geminiKey == "" {
		return nil, err
	}
	gImg, gErr := s.gemini(ctx, prompt)
	if gErr != nil {
		return nil, fmt.Errorf("pollinations: %v; gemini: %w", err, gErr)
	}
	return gImg, nil
}

func (s *Service) pollinations(ctx context.Context, prompt string) (*Image, error) {
	q := url.Values{}
	q.Set("width", "1024")
	q.Set("height", "1024")
	q.Set("nologo", "true")
	q.Set("model", "flux")
	q.Set("seed", fmt.Sprint(rand.Intn(1_000_000)))
	reqURL := s.pollinationsURL + url.PathEscape(prompt) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if s.pollinationsKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.pollinationsKey)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") || len(data) == 0 {
		return nil, fmt.Errorf("pollinations status %d (%s)", resp.StatusCode, ct)
	}
	return &Image{Data: data, MIMEType: ct, Provider: "pollinations"}, nil
}

func (s *Service) gemini(ctx context.Context, prompt string) (*Image, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"IMAGE", "TEXT"},
		},
	})
	reqURL := s.geminiBaseURL + s.geminiModel + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", s.geminiKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 3*maxImageBytes))
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("gemini image status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("gemini image decode: %w", err)
	}
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData == nil || p.InlineData.Data == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini image base64: %w", err)
			}
			return &Image{Data: data, MIMEType: p.InlineData.MimeType, Provider: "gemini"}, nil
		}
	}
	return nil, fmt.Errorf("gemini returned no image")
}
