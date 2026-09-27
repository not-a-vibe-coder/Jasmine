package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultTokensFile = "tokens_usage.json"

type TokenUsageStats struct {
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	RequestCount     int64  `json:"request_count"`
}

type persistedTokenData struct {
	DailyHistory map[string]map[string]*TokenUsageStats `json:"daily_history"` // date (YYYY-MM-DD) -> model -> stats
	TotalUsage   map[string]*TokenUsageStats            `json:"total_usage"`   // model -> stats
}

type TokenTracker struct {
	mu           sync.RWMutex
	filePath     string
	dailyHistory map[string]map[string]*TokenUsageStats // date -> model -> stats
	totalUsage   map[string]*TokenUsageStats            // model -> stats
}

func NewTokenTracker(customPath ...string) *TokenTracker {
	fPath := defaultTokensFile
	if len(customPath) > 0 && customPath[0] != "" {
		fPath = customPath[0]
	}

	t := &TokenTracker{
		filePath:     fPath,
		dailyHistory: make(map[string]map[string]*TokenUsageStats),
		totalUsage:   make(map[string]*TokenUsageStats),
	}

	t.loadFromDisk()
	return t
}

func (t *TokenTracker) loadFromDisk() {
	if t.filePath == "" {
		return
	}
	data, err := os.ReadFile(t.filePath)
	if err != nil {
		return
	}

	var p persistedTokenData
	if err := json.Unmarshal(data, &p); err == nil {
		if p.DailyHistory != nil {
			t.dailyHistory = p.DailyHistory
		}
		if p.TotalUsage != nil {
			t.totalUsage = p.TotalUsage
		}
	}
}

func (t *TokenTracker) saveToDiskLocked() {
	if t.filePath == "" {
		return
	}
	p := persistedTokenData{
		DailyHistory: t.dailyHistory,
		TotalUsage:   t.totalUsage,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err == nil {
		_ = os.WriteFile(t.filePath, data, 0644)
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

	// Ensure date map exists
	if _, ok := t.dailyHistory[today]; !ok {
		t.dailyHistory[today] = make(map[string]*TokenUsageStats)
	}

	// Update daily usage
	d, exists := t.dailyHistory[today][model]
	if !exists {
		d = &TokenUsageStats{Model: model}
		t.dailyHistory[today][model] = d
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

	t.saveToDiskLocked()
}

// ParseDateQuery attempts to parse user input into a YYYY-MM-DD date.
func ParseDateQuery(input string) (string, error) {
	clean := strings.ToLower(strings.TrimSpace(input))
	if clean == "" || clean == "today" {
		return time.Now().UTC().Format("2006-01-02"), nil
	}
	if clean == "yesterday" {
		return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"), nil
	}

	// Common date layouts
	layouts := []string{
		"2006-01-02",
		"02-01-2006",
		"02/01/2006",
		"02012006",
		"02-01-06",
		"02/01/06",
		"020106",
		"2006/01/02",
		"Jan 2 2006",
		"2 Jan 2006",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, clean); err == nil {
			return t.UTC().Format("2006-01-02"), nil
		}
	}

	return "", fmt.Errorf("could not parse date %q (use DDMMYY, DD-MM-YYYY, YYYY-MM-DD, or 'yesterday')", input)
}

// FormatReport generates a breakdown of token consumption for today, a past date, or history overview.
func (t *TokenTracker) FormatReport(query ...string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	rawQ := ""
	if len(query) > 0 {
		rawQ = strings.ToLower(strings.TrimSpace(query[0]))
	}

	// 1. History overview query
	if rawQ == "history" || rawQ == "all" || rawQ == "past" {
		return t.formatHistoryReportLocked()
	}

	// 2. Specific date or today
	targetDate, err := ParseDateQuery(rawQ)
	if err != nil {
		return fmt.Sprintf("invalid date format: %v\n\nexamples:\n- `/tokens` (today)\n- `/tokens yesterday`\n- `/tokens 270926` (DDMMYY)\n- `/tokens 2026-09-27`\n- `/tokens history`", err)
	}

	dayStats := t.dailyHistory[targetDate]
	var dayIn, dayOut, dayTot, dayReqs int64
	for _, s := range dayStats {
		dayIn += s.PromptTokens
		dayOut += s.CompletionTokens
		dayTot += s.TotalTokens
		dayReqs += s.RequestCount
	}

	var totIn, totOut, totAll, totReqs int64
	for _, s := range t.totalUsage {
		totIn += s.PromptTokens
		totOut += s.CompletionTokens
		totAll += s.TotalTokens
		totReqs += s.RequestCount
	}

	todayUTC := time.Now().UTC().Format("2006-01-02")
	yesterdayUTC := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	dateLabel := targetDate
	if targetDate == todayUTC {
		dateLabel = fmt.Sprintf("today (%s)", targetDate)
	} else if targetDate == yesterdayUTC {
		dateLabel = fmt.Sprintf("yesterday (%s)", targetDate)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("token consumption stats for %s (utc):\n\n", dateLabel))
	sb.WriteString(fmt.Sprintf("• prompt tokens (in): %d\n• completion tokens (out): %d\n• total tokens: %d (%d requests)\n\n",
		dayIn, dayOut, dayTot, dayReqs))

	if len(dayStats) > 0 {
		sb.WriteString("by model:\n")
		// Sorted models for deterministic output
		var models []string
		for m := range dayStats {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			s := dayStats[m]
			sb.WriteString(fmt.Sprintf("• %s: %d in / %d out (total %d across %d reqs)\n",
				m, s.PromptTokens, s.CompletionTokens, s.TotalTokens, s.RequestCount))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("no requests recorded on this date.\n\n")
	}

	sb.WriteString(fmt.Sprintf("lifetime:\n• total in: %d | total out: %d | grand total: %d (%d requests)",
		totIn, totOut, totAll, totReqs))

	return sb.String()
}

func (t *TokenTracker) formatHistoryReportLocked() string {
	var dates []string
	for d := range t.dailyHistory {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	var sb strings.Builder
	sb.WriteString("token consumption history by date (utc):\n\n")

	if len(dates) == 0 {
		sb.WriteString("no daily history recorded yet.\n\n")
	} else {
		for _, date := range dates {
			dayStats := t.dailyHistory[date]
			var dTot, dReqs int64
			for _, s := range dayStats {
				dTot += s.TotalTokens
				dReqs += s.RequestCount
			}
			sb.WriteString(fmt.Sprintf("• %s: %d tokens (%d requests)\n", date, dTot, dReqs))
		}
		sb.WriteString("\n")
	}

	var totIn, totOut, totAll, totReqs int64
	for _, s := range t.totalUsage {
		totIn += s.PromptTokens
		totOut += s.CompletionTokens
		totAll += s.TotalTokens
		totReqs += s.RequestCount
	}

	sb.WriteString(fmt.Sprintf("lifetime:\n• total in: %d | total out: %d | grand total: %d (%d requests)",
		totIn, totOut, totAll, totReqs))

	return sb.String()
}
