package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"shipp/internal/memory"
)

type geminiChatReq struct {
	SystemInstruction *geminiChatContent    `json:"systemInstruction,omitempty"`
	Contents          []geminiChatContent   `json:"contents"`
	Tools             []geminiChatTool      `json:"tools,omitempty"`
	SafetySettings    []geminiSafetySetting `json:"safetySettings,omitempty"`
	GenerationConfig  *geminiChatGenConfig  `json:"generationConfig,omitempty"`
}

type geminiSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type geminiChatContent struct {
	Role  string           `json:"role,omitempty"`
	Parts []geminiChatPart `json:"parts"`
}

type geminiChatPart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResp `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type geminiFunctionResp struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type geminiChatTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type geminiChatGenConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiChatResp struct {
	Candidates []struct {
		Content struct {
			Role  string           `json:"role"`
			Parts []geminiChatPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func defaultSafetySettings() []geminiSafetySetting {
	return []geminiSafetySetting{
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
		{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
		{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
	}
}

func (c *Client) callGeminiGenerate(ctx context.Context, payload geminiChatReq) (*geminiChatResp, error) {
	if c.geminiKey == "" {
		return nil, fmt.Errorf("gemini API key is not configured")
	}

	payload.SafetySettings = defaultSafetySettings()

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gemini payload: %w", err)
	}

	models := []string{"gemini-2.5-flash", "gemini-flash-latest"}
	var lastErr error

	for _, model := range models {
		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, c.geminiKey)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBytes))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			time.Sleep(1500 * time.Millisecond)
			retryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBytes))
			if err == nil {
				retryReq.Header.Set("Content-Type", "application/json")
				resp, err = c.httpClient.Do(retryReq)
			}
		}

		if err != nil {
			lastErr = err
			continue
		}

		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("gemini api error (status %d): %s", resp.StatusCode, string(respBytes))
			continue
		}

		var geminiResp geminiChatResp
		if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
			lastErr = fmt.Errorf("failed to decode gemini response: %w", err)
			continue
		}

		if geminiResp.Error != nil {
			lastErr = fmt.Errorf("gemini returned error %d: %s", geminiResp.Error.Code, geminiResp.Error.Message)
			continue
		}

		return &geminiResp, nil
	}

	return nil, fmt.Errorf("all gemini models failed: %w", lastErr)
}

func (c *Client) generateReplyGemini(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	history []memory.Message,
	currentPrompt string,
	summary string,
	profile *memory.UserProfile,
) (*AIResponse, error) {
	sysPrompt := c.systemPrompt(senderUsername, isOwner, profile)
	if summary != "" {
		sysPrompt += fmt.Sprintf("\n\n[Past Chat Summary Context]: %s", summary)
	}

	req := geminiChatReq{
		SystemInstruction: &geminiChatContent{
			Parts: []geminiChatPart{{Text: sysPrompt}},
		},
		GenerationConfig: &geminiChatGenConfig{
			Temperature:     0.5,
			MaxOutputTokens: 300,
		},
	}

	// Build function declarations from tools
	var decls []geminiFunctionDecl
	for _, t := range c.tools {
		decls = append(decls, geminiFunctionDecl{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	if len(decls) > 0 {
		req.Tools = []geminiChatTool{{FunctionDeclarations: decls}}
	}

	// Format history
	for _, h := range history {
		role := "user"
		if h.Role == "assistant" {
			role = "model"
		}
		prefix := ""
		if h.Sender != "" && role == "user" {
			prefix = fmt.Sprintf("@%s: ", h.Sender)
		}
		content := h.Content
		if len(content) > 350 {
			content = content[:350] + "..."
		}
		req.Contents = append(req.Contents, geminiChatContent{
			Role:  role,
			Parts: []geminiChatPart{{Text: prefix + content}},
		})
	}

	// Add current prompt
	req.Contents = append(req.Contents, geminiChatContent{
		Role:  "user",
		Parts: []geminiChatPart{{Text: fmt.Sprintf("@%s: %s", senderUsername, currentPrompt)}},
	})

	resp, err := c.callGeminiGenerate(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(resp.Candidates) == 0 {
		return &AIResponse{Content: ""}, nil
	}

	cand := resp.Candidates[0]
	var textParts []string
	var toolCalls []ToolCall

	for i, part := range cand.Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			argsJSON, _ := json.Marshal(part.FunctionCall.Args)
			toolCalls = append(toolCalls, ToolCall{
				ID:   fmt.Sprintf("call_gemini_%d_%d", time.Now().UnixNano(), i),
				Type: "function",
				Function: FunctionCall{
					Name:      part.FunctionCall.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	return &AIResponse{
		Content:   strings.Join(textParts, "\n"),
		ToolCalls: toolCalls,
	}, nil
}

func (c *Client) generateToolFollowupGemini(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	userPrompt string,
	toolName string,
	toolCallID string,
	toolArgs string,
	toolResult string,
	profile *memory.UserProfile,
) (string, error) {
	sysPrompt := c.systemPrompt(senderUsername, isOwner, profile)
	req := geminiChatReq{
		SystemInstruction: &geminiChatContent{
			Parts: []geminiChatPart{{Text: sysPrompt}},
		},
		GenerationConfig: &geminiChatGenConfig{
			Temperature:     0.5,
			MaxOutputTokens: 250,
		},
		Contents: []geminiChatContent{
			{
				Role:  "user",
				Parts: []geminiChatPart{{Text: userPrompt}},
			},
			{
				Role: "model",
				Parts: []geminiChatPart{
					{
						Text: fmt.Sprintf("[Executed Tool %s with args %s. Output: %s]", toolName, toolArgs, toolResult),
					},
				},
			},
			{
				Role: "user",
				Parts: []geminiChatPart{
					{
						Text: "Now answer naturally in 1-2 conversational sentences based strictly on the tool result. Strictly zero emojis, no em dashes, no bulky lists.",
					},
				},
			},
		},
	}

	resp, err := c.callGeminiGenerate(ctx, req)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 {
		return toolResult, nil
	}

	var textParts []string
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.Text != "" {
			textParts = append(textParts, p.Text)
		}
	}
	return strings.Join(textParts, " "), nil
}

func (c *Client) refactorFileGemini(
	ctx context.Context,
	filename string,
	currentContent string,
	instruction string,
) (string, error) {
	prompt := fmt.Sprintf(`You are an expert software engineer and editor.
You are modifying the file '%s'.
Here is the current content of the file:
`+"```"+`
%s
`+"```"+`

User instruction: "%s"

CRITICAL INSTRUCTIONS:
1. Output ONLY the updated full content of the file.
2. Do NOT wrap the entire output in markdown backticks unless the file itself is markdown.
3. Strictly NO conversational preamble, no "Here is the updated file:", no explanations.
4. Return ONLY the raw file contents ready to be committed directly to git.`, filename, currentContent, instruction)

	req := geminiChatReq{
		Contents: []geminiChatContent{
			{
				Role:  "user",
				Parts: []geminiChatPart{{Text: prompt}},
			},
		},
		GenerationConfig: &geminiChatGenConfig{
			Temperature:     0.2,
			MaxOutputTokens: 4000,
		},
	}

	resp, err := c.callGeminiGenerate(ctx, req)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned empty response for refactor")
	}

	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return cleanCodeBlock(sb.String()), nil
}

func (c *Client) analyzeDocumentGemini(
	ctx context.Context,
	senderUsername string,
	isOwner bool,
	filename string,
	docText string,
	userPrompt string,
	profile *memory.UserProfile,
) (string, error) {
	sysPrompt := c.systemPrompt(senderUsername, isOwner, profile)
	prompt := fmt.Sprintf(`Document '%s' content:
%s

User question/context: "%s"

Analyze this document concisely. Strictly zero emojis, no em dashes, 1-3 sentences max.`, filename, docText, userPrompt)

	req := geminiChatReq{
		SystemInstruction: &geminiChatContent{
			Parts: []geminiChatPart{{Text: sysPrompt}},
		},
		Contents: []geminiChatContent{
			{
				Role:  "user",
				Parts: []geminiChatPart{{Text: prompt}},
			},
		},
		GenerationConfig: &geminiChatGenConfig{
			Temperature:     0.4,
			MaxOutputTokens: 400,
		},
	}

	resp, err := c.callGeminiGenerate(ctx, req)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 {
		return "", fmt.Errorf("empty gemini response for document analysis")
	}

	var textParts []string
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.Text != "" {
			textParts = append(textParts, p.Text)
		}
	}
	return strings.Join(textParts, " "), nil
}
