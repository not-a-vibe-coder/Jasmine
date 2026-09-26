package ai

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type TokenUsageStats struct {
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	RequestCount     int64  `json:"request_count"`
}

type TokenTracker struct {
	mu          sync.RWMutex
	currentDate string
	dailyUsage  map[string]*TokenUsageStats // key = model
	totalUsage  map[string]*TokenUsageStats // key = model
}

func NewTokenTracker() *TokenTracker {
	return &TokenTracker{
		currentDate: time.Now().UTC().Format("2006-01-02"),
		dailyUsage:  make(map[string]*TokenUsageStats),
		totalUsage:  make(map[string]*TokenUsageStats),
	}
}

// Record adds consumed prompt (in) and completion (out) tokens for a model.
func (t *TokenTracker) Record(model string, promptTokens, completionTokens int) {
	if model == "" {
		model = "unknown"
	}
	today := time.Now().UTC().Format("2006-01-02")

	t.mu.Lock()
	defer t.mu.Unlock()

	// Reset daily usage if a new UTC day has rolled over
	if today != t.currentDate {
		t.currentDate = today
		t.dailyUsage = make(map[string]*TokenUsageStats)
	}

	// Update daily usage
	d, exists := t.dailyUsage[model]
	if !exists {
		d = &TokenUsageStats{Model: model}
		t.dailyUsage[model] = d
	}
	d.PromptTokens += int64(promptTokens)
	d.CompletionTokens += int64(completionTokens)
	d.TotalTokens += int64(promptTokens + completionTokens)
	d.RequestCount++

	// Update lifetime total usage
	tot, exists := t.totalUsage[model]
	if !exists {
		tot = &TokenUsageStats{Model: model}
		t.totalUsage[model] = tot
	}
	tot.PromptTokens += int64(promptTokens)
	tot.CompletionTokens += int64(completionTokens)
	tot.TotalTokens += int64(promptTokens + completionTokens)
	tot.RequestCount++
}

// FormatReport generates a clear, concise breakdown of token consumption for admin inspection.
func (t *TokenTracker) FormatReport() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var dailyIn, dailyOut, dailyTot, dailyReqs int64
	for _, s := range t.dailyUsage {
		dailyIn += s.PromptTokens
		dailyOut += s.CompletionTokens
		dailyTot += s.TotalTokens
		dailyReqs += s.RequestCount
	}

	var totIn, totOut, totAll, totReqs int64
	for _, s := range t.totalUsage {
		totIn += s.PromptTokens
		totOut += s.CompletionTokens
		totAll += s.TotalTokens
		totReqs += s.RequestCount
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("token consumption stats (utc %s):\n\n", t.currentDate))
	sb.WriteString(fmt.Sprintf("today:\n- prompt tokens (in): %d\n- completion tokens (out): %d\n- total tokens: %d (%d requests)\n\n",
		dailyIn, dailyOut, dailyTot, dailyReqs))

	if len(t.dailyUsage) > 0 {
		sb.WriteString("today by model:\n")
		for model, s := range t.dailyUsage {
			sb.WriteString(fmt.Sprintf("- %s: %d in / %d out (total %d across %d reqs)\n",
				model, s.PromptTokens, s.CompletionTokens, s.TotalTokens, s.RequestCount))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("lifetime:\n- total in: %d | total out: %d | grand total: %d (%d requests)",
		totIn, totOut, totAll, totReqs))

	return sb.String()
}
