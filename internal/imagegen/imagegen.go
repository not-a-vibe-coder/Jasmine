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
	// gen.pollinations.ai serves current models; the legacy keyless host only serves a small model.
	DefaultPollinationsURL       = "https://gen.pollinations.ai/image/"
	DefaultLegacyPollinationsURL = "https://image.pollinations.ai/prompt/"
	DefaultGeminiModel           = "gemini-2.5-flash-image"
	maxImageBytes                = 15 << 20
)

// DefaultPollinationsModels are free (non paid-only, flat-rate) models, best first.
var DefaultPollinationsModels = []string{
	"tongyi-mai/z-image-turbo",
	"black-forest-labs/flux.2-klein-4b",
	"black-forest-labs/flux.1-schnell",
}

type Image struct {
	Data     []byte
	MIMEType string
	Provider string
}

type Service struct {
	hfToken            string
	hfModel            string
	hfURL              string
	pollinationsURL    string
	legacyURL          string
	pollinationsKey    string
	pollinationsModels []string
	geminiKey       string
	geminiModel     string
	geminiBaseURL   string
	httpClient      *http.Client
}

func NewService(pollinationsKey, geminiKey, geminiModel string, pollinationsModels ...string) *Service {
	if geminiModel == "" {
		geminiModel = DefaultGeminiModel
	}
	if len(pollinationsModels) == 0 {
		pollinationsModels = DefaultPollinationsModels
	}
	return &Service{
		pollinationsURL:    DefaultPollinationsURL,
		legacyURL:          DefaultLegacyPollinationsURL,
		pollinationsKey:    pollinationsKey,
		pollinationsModels: pollinationsModels,
		geminiKey:       geminiKey,
		geminiModel:     geminiModel,
		geminiBaseURL:   "https://generativelanguage.googleapis.com/v1beta/models/",
		httpClient:      &http.Client{Timeout: 90 * time.Second},
	}
}

// DefaultHFModel is served through Hugging Face's router (nscale provider) and billed
// against the account's free monthly inference credits.
const DefaultHFModel = "black-forest-labs/FLUX.1-schnell"

// SetHuggingFace enables FLUX via the Hugging Face router.
func (s *Service) SetHuggingFace(token, model string) {
	if model == "" {
		model = DefaultHFModel
	}
	s.hfToken, s.hfModel = token, model
	if s.hfURL == "" {
		s.hfURL = "https://router.huggingface.co/nscale/v1/images/generations"
	}
}

func (s *Service) huggingFace(ctx context.Context, prompt string) (*Image, error) {
	body, _ := json.Marshal(map[string]string{"model": s.hfModel, "prompt": prompt, "response_format": "b64_json"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.hfURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.hfToken)
	req.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || out.Data[0].B64 == "" {
		return nil, fmt.Errorf("no image in response")
	}
	data, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil {
		return nil, err
	}
	return &Image{Data: data, MIMEType: http.DetectContentType(data), Provider: "huggingface/" + s.hfModel}, nil
}

// SetBaseURLs points the service at test servers.
func (s *Service) SetBaseURLs(pollinations, gemini string) {
	s.pollinationsURL = pollinations
	s.legacyURL = pollinations
	s.geminiBaseURL = gemini
}

// Generate returns the first image any provider produces for the prompt.
func (s *Service) Generate(ctx context.Context, prompt string) (*Image, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("empty image prompt")
	}
	var errs []string
	// 1. Pollinations' current models, best first. These require a (free) Pollinations key.
	for _, model := range s.pollinationsModels {
		if s.pollinationsKey == "" {
			break
		}
		img, err := s.pollinations(ctx, s.pollinationsURL, model, prompt, true)
		if err == nil {
			return img, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", model, err))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("image generation timed out: %s", strings.Join(errs, "; "))
		}
	}
	// 2. Hugging Face FLUX (free monthly credits).
	if s.hfToken != "" {
		img, err := s.huggingFace(ctx, prompt)
		if err == nil {
			return img, nil
		}
		errs = append(errs, "huggingface: "+err.Error())
	}
	// 3. Gemini (needs a key with image quota, i.e. billing enabled).
	if s.geminiKey != "" {
		img, err := s.gemini(ctx, prompt)
		if err == nil {
			return img, nil
		}
		errs = append(errs, "gemini: "+err.Error())
	}
	// 4. Legacy keyless endpoint as a last resort.
	img, err := s.pollinations(ctx, s.legacyURL, "", prompt, false)
	if err == nil {
		return img, nil
	}
	errs = append(errs, "legacy: "+err.Error())
	return nil, fmt.Errorf("all image providers failed: %s", strings.Join(errs, "; "))
}

func (s *Service) pollinations(ctx context.Context, baseURL, model, prompt string, withKey bool) (*Image, error) {
	q := url.Values{}
	q.Set("width", "1024")
	q.Set("height", "1024")
	q.Set("nologo", "true")
	if model != "" {
		q.Set("model", model)
	}
	q.Set("seed", fmt.Sprint(rand.Intn(1_000_000)))
	reqURL := baseURL + url.PathEscape(prompt) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if withKey && s.pollinationsKey != "" {
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
	provider := "pollinations"
	if model != "" {
		provider += "/" + model
	}
	return &Image{Data: data, MIMEType: ct, Provider: provider}, nil
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
