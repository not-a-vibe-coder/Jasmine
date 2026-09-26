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

// AnalyzeImage analyzes an image with visual awareness (colors, objects, layout, style, charts, text)
// using Gemini Flash as primary and OCR as fallback.
func (s *Service) AnalyzeImage(ctx context.Context, imageBytes []byte, mimeType string, userPrompt string) (string, error) {
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("empty image bytes")
	}
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	// 1. Primary: Gemini Vision (2.5 Flash)
	if s.geminiKey != "" {
		res, err := s.callGemini(ctx, "gemini-2.5-flash", imageBytes, mimeType, userPrompt)
		if err == nil && res != "" {
			return res, nil
		}
		log.Printf("[VisionService] Gemini 2.5 Flash failed: %v. Retrying with gemini-flash-latest...", err)

		// 1b. Fallback Gemini model
		res, err = s.callGemini(ctx, "gemini-flash-latest", imageBytes, mimeType, userPrompt)
		if err == nil && res != "" {
			return res, nil
		}
		log.Printf("[VisionService] Gemini fallback model failed: %v. Falling back to OCR...", err)
	}

	// 2. Secondary Fallback: OCR text extraction
	ocrText, ocrErr := s.fallbackOCR(ctx, imageBytes)
	if ocrErr == nil && strings.TrimSpace(ocrText) != "" {
		promptPrefix := "I extracted this text from the image using OCR:"
		if userPrompt != "" {
			return fmt.Sprintf("%s\n\n```\n%s\n```\n(Responding to '%s')", promptPrefix, ocrText, userPrompt), nil
		}
		return fmt.Sprintf("%s\n\n```\n%s\n```", promptPrefix, ocrText), nil
	}

	return "I took a look, but couldn't clearly parse the image right now.", nil
}

type geminiReq struct {
	Contents []geminiContent `json:"contents"`
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
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (s *Service) callGemini(ctx context.Context, model string, imageBytes []byte, mimeType string, userPrompt string) (string, error) {
	b64Data := base64.StdEncoding.EncodeToString(imageBytes)

	systemInstruction := "You are Shipp, a sharp, witty, highly intelligent AI companion in a Telegram group chat. " +
		"Analyze this image with full visual awareness (colors, aesthetics, charts, candlestick trends, memes, UI layout, objects, and text). " +
		"Speak naturally like a smart friend in chat. Do NOT use robotic corporate filler or preamble. "

	if userPrompt != "" {
		systemInstruction += fmt.Sprintf("The user sent this prompt with the image: '%s'. Prioritize answering their question directly using the visual details.", userPrompt)
	} else {
		systemInstruction += "Provide a punchy, perceptive, and witty observation about what's in the image, noting notable colors and details."
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
