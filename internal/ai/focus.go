package ai

// Conversation focus: send the model only the instructions and tools that matter for what
// people are talking about right now. A smaller prompt keeps the model on topic, answers
// faster, and lets ordinary chat fit under Groq's free-tier token limit.

import (
	"regexp"
	"strings"

	"shipp/internal/memory"
)

type domain string

const (
	domainCrypto   domain = "crypto"
	domainGitHub   domain = "github"
	domainSandbox  domain = "sandbox"
	domainEmail    domain = "email"
	domainMoltbook domain = "moltbook"
	domainDomains  domain = "domains"
	domainX        domain = "x"
	domainSocial   domain = "social"
	domainRemind   domain = "remind"
	domainCalls    domain = "calls"
)

var domainPatterns = map[domain]*regexp.Regexp{
	domainCrypto:   regexp.MustCompile(`(?i)\b(wallet|balance|bags?|funds?|send|transfer|tip|pay|sol|solana|eth|ether|base|arbitrum|bnb|robinhood|usdc|usdt|token|coin|crypto|ca|contract|mcap|market ?cap|price|convert|usd|\$[0-9]|deposit|address|on-?chain|airdrop)\b|0x[0-9a-fA-F]{6,}|[1-9A-HJ-NP-Za-km-z]{32,44}`),
	domainGitHub:   regexp.MustCompile(`(?i)\b(github|repo|repository|pr|pull request|commit|branch|merge|issue|readme|ci|workflow|release|push)\b|github\.com/`),
	domainSandbox:  regexp.MustCompile(`(?i)\b(sandbox|run|execute|script|bash|terminal|shell|command|python|benchmark|scrape|install|compile|linux|vm)\b`),
	domainEmail:    regexp.MustCompile(`(?i)\b(e-?mail|inbox|mail)\b|[\w.+-]+@[\w-]+\.[a-z]{2,}`),
	domainMoltbook: regexp.MustCompile(`(?i)\bmoltbook\b`),
	domainDomains:  regexp.MustCompile(`(?i)\b(domain|domains|\.com|\.io|\.xyz|\.ai|\.app|tld|vercel)\b`),
	domainX:        regexp.MustCompile(`(?i)\b(twitter|tweet|x handle|x account|x link|x profile|x\.com|handle)\b|x\.com/|twitter\.com/`),
	domainRemind:   regexp.MustCompile(`(?i)\b(remind\w*|reminders?|alarm|wake me|forget|ping me|nudge me|every (day|morning|night|week|monday|tuesday|wednesday|thursday|friday|saturday|sunday))\b`),
	domainCalls:    regexp.MustCompile(`(?i)\b(call|calls|calling|ring|phone|number|dial|whatsapp|wa)\b|\+?\d[\d\s-]{8,}\d`),
	domainSocial:   regexp.MustCompile(`(?i)\b(tweet|thread|post|curate|draft|caption|announcement|copy)\b`),
}

// detectDomains looks at the new message and the last few history lines (so follow-ups
// like "yes send it" keep their topic) and returns the active domains.
func detectDomains(userPrompt string, history []memory.Message, chatContext string) map[domain]bool {
	var sb strings.Builder
	sb.WriteString(userPrompt)
	for i := len(history) - 1; i >= 0 && i >= len(history)-4; i-- {
		sb.WriteString("\n")
		sb.WriteString(history[i].Content)
	}
	// Reply context quotes the message being answered; include it too.
	if idx := strings.Index(chatContext, "ACTIVE REPLY CONTEXT"); idx >= 0 {
		sb.WriteString("\n")
		sb.WriteString(chatContext[idx:])
	}
	text := sb.String()
	active := map[domain]bool{}
	for d, re := range domainPatterns {
		if re.MatchString(text) {
			active[d] = true
		}
	}
	if active[domainX] {
		active[domainSocial] = true
	}
	if active[domainCalls] {
		active[domainRemind] = true // "call me at 6am" is a reminder delivered by call
	}
	return active
}

// Prompt sections (by number, under "Operational Superpowers & Tools") that only matter
// for one domain. Unlisted sections are always kept.
var sectionDomains = map[string]domain{
	"5":  domainCrypto,
	"7":  domainCrypto,
	"9":  domainGitHub,
	"10": domainEmail,
	"11": domainSandbox,
	"14": domainDomains,
	"15": domainX,
	"19": domainX,
	"21": domainMoltbook,
	"22": domainSocial,
	"29": domainRemind,
	"30": domainCalls,
}

var sectionHeaderRe = regexp.MustCompile(`^(\d+)\. `)

// focusPrompt drops operational sections for domains nobody is talking about.
func focusPrompt(prompt string, active map[domain]bool) string {
	start := strings.Index(prompt, "Operational Superpowers & Tools:")
	if start < 0 {
		return prompt
	}
	head, body := prompt[:start], prompt[start:]
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	keep := true
	for _, line := range lines {
		if m := sectionHeaderRe.FindStringSubmatch(line); m != nil {
			d, scoped := sectionDomains[m[1]]
			keep = !scoped || active[d]
		}
		if keep {
			out = append(out, line)
		}
	}
	return head + strings.Join(out, "\n")
}

// Tools that only make sense inside one domain. Unlisted tools are always offered.
var toolDomains = map[string]domain{
	"get_wallet_address":     domainCrypto,
	"get_balances":           domainCrypto,
	"convert_crypto":         domainCrypto,
	"send_crypto":            domainCrypto,
	"analyze_token":          domainCrypto,
	"github_inspect_project": domainGitHub,
	"github_view_repo":       domainGitHub,
	"github_edit_file":       domainGitHub,
	"github_merge_pr":        domainGitHub,
	"github_close_pr":        domainGitHub,
	"github_close_issue":     domainGitHub,
	"github_create_repo":     domainGitHub,
	"run_sandbox_task":       domainSandbox,
	"get_sandbox_runs":       domainSandbox,
	"send_email":             domainEmail,
	"moltbook_feed":          domainMoltbook,
	"moltbook_search":        domainMoltbook,
	"moltbook_notifications": domainMoltbook,
	"moltbook_post":          domainMoltbook,
	"moltbook_comment":       domainMoltbook,
	"vercel_search_domains":  domainDomains,
	"set_reminder":           domainRemind,
	"list_reminders":         domainRemind,
	"cancel_reminder":        domainRemind,
	"call_user":              domainCalls,
	"save_phone_number":      domainCalls,
	"forget_phone_number":    domainCalls,
	"check_x_username":       domainX,
	"read_x_post":            domainX,
}

func focusTools(tools []ToolDefinition, active map[domain]bool) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if d, scoped := toolDomains[t.Function.Name]; scoped && !active[d] {
			continue
		}
		out = append(out, t)
	}
	return out
}
