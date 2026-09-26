package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type Service struct {
	geminiKey  string
	httpClient *http.Client
}

func NewService(geminiKey string) *Service {
	return &Service{
		geminiKey: geminiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// PerceiveImage extracts objective visual facts (objects, text, colors, layout, charts, numbers)
// using Gemini Flash as primary visual perception engine and OCR as fallback.
func (s *Service) PerceiveImage(ctx context.Context, imageBytes []byte, mimeType string, userPrompt string) (string, error) {
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("empty image bytes")
	}
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	// 1. Primary: Gemini Vision (2.5 Flash)
	if s.geminiKey != "" {
		res, err := s.callGemini(ctx, "gemini-2.5-flash", imageBytes, mimeType, userPrompt)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		log.Printf("[VisionService] Gemini 2.5 Flash failed: %v. Retrying with gemini-flash-latest...", err)

		// 1b. Fallback Gemini model
		res, err = s.callGemini(ctx, "gemini-flash-latest", imageBytes, mimeType, userPrompt)
		if err == nil && strings.TrimSpace(res) != "" {
			return res, nil
		}
		log.Printf("[VisionService] Gemini fallback model failed: %v. Falling back to OCR...", err)
	}

	// 2. Secondary Fallback: OCR text extraction
	ocrText, ocrErr := s.fallbackOCR(ctx, imageBytes)
	if ocrErr == nil && strings.TrimSpace(ocrText) != "" {
		return fmt.Sprintf("[Visual OCR Text Extracted]:\n%s", strings.TrimSpace(ocrText)), nil
	}

	return "", fmt.Errorf("unable to visually perceive image or extract OCR text")
}

// AnalyzeImage is an alias for PerceiveImage for backward compatibility.
func (s *Service) AnalyzeImage(ctx context.Context, imageBytes []byte, mimeType string, userPrompt string) (string, error) {
	return s.PerceiveImage(ctx, imageBytes, mimeType, userPrompt)
}

type geminiReq struct {
	Contents       []geminiContent       `json:"contents"`
	SafetySettings []geminiSafetySetting `json:"safetySettings,omitempty"`
}

type geminiSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type geminiResp struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (s *Service) callGemini(ctx context.Context, model string, imageBytes []byte, mimeType string, userPrompt string) (string, error) {
	b64Data := base64.StdEncoding.EncodeToString(imageBytes)

	systemInstruction := "You are an objective, expert visual perception engine. Examine the image carefully and extract all visual facts objectively, accurately, and thoroughly for an AI reasoning system.\n" +
		"Extract and report:\n" +
		"- Main subject, scene, objects, people, or media type (e.g. photo, crypto PnL card/chart, meme, screenshot, document, artwork).\n" +
		"- Visible text, numbers, labels, currency amounts, percentages, and tickers exactly as displayed (full OCR).\n" +
		"- Dominant colors, aesthetic, layout, UI elements, and visual themes (e.g. dark mode/light mode, green/red accents).\n" +
		"- If it's a crypto trade / PnL card: identify platform (Bayse, Binance, Bybit, Hyperliquid, etc.), pair/coin, position (Long/Short), leverage multiplier (e.g. 40x), PnL % and profit USD, entry price, mark/current price, liquidation price.\n" +
		"- If it's a chart: time frame, price action trend, indicators, key levels.\n" +
		"- If it's a meme or graphic: describe what is depicted and any punchlines or visual humor.\n" +
		"Be objective, structured, and factual. Do NOT address the user directly, do NOT write chat greetings, banter, or conversational filler."

	if userPrompt != "" {
		systemInstruction += fmt.Sprintf("\n\nUser Context/Question: \"%s\". Pay special attention to any visual elements or text that answer this context.", userPrompt)
	}

	payload := geminiReq{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: systemInstruction},
					{
						InlineData: &geminiInlineData{
							MimeType: mimeType,
							Data:     b64Data,
						},
					},
				},
			},
		},
		SafetySettings: []geminiSafetySetting{
			{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
			{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
			{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
			{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
			{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
		},
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, s.geminiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Retry once on rate limit (429) after brief delay
	if resp.StatusCode == http.StatusTooManyRequests {
		time.Sleep(1500 * time.Millisecond)
		retryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBytes))
		if err == nil {
			retryReq.Header.Set("Content-Type", "application/json")
			if retryResp, err := s.httpClient.Do(retryReq); err == nil {
				defer retryResp.Body.Close()
				resp = retryResp
			}
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini api returned status %d: %s", resp.StatusCode, string(body))
	}

	var gResp geminiResp
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return "", err
	}

	if len(gResp.Candidates) > 0 && len(gResp.Candidates[0].Content.Parts) > 0 {
		return strings.TrimSpace(gResp.Candidates[0].Content.Parts[0].Text), nil
	}

	return "", fmt.Errorf("no response candidates returned by gemini")
}

func (s *Service) fallbackOCR(ctx context.Context, imageBytes []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", "image.png")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(imageBytes); err != nil {
		return "", err
	}

	_ = writer.WriteField("apikey", "K87899142388957")
	_ = writer.WriteField("isOverlayRequired", "false")
	_ = writer.WriteField("OCREngine", "1")
	_ = writer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.ocr.space/parse/image", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var ocrRes struct {
		ParsedResults []struct {
			ParsedText string `json:"ParsedText"`
		} `json:"ParsedResults"`
		ErrorMessage []string `json:"ErrorMessage"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&ocrRes); err != nil {
		return "", err
	}

	if len(ocrRes.ParsedResults) > 0 {
		return strings.TrimSpace(ocrRes.ParsedResults[0].ParsedText), nil
	}

	return "", fmt.Errorf("no ocr text detected")
}
