package ai

import (
	"strings"
	"testing"

	"shipp/internal/memory"
)

func TestDetectDomains(t *testing.T) {
	cases := []struct {
		prompt  string
		history []string
		want    []domain
		notWant []domain
	}{
		{"haha you're so funny jasmine", nil, nil, []domain{domainCrypto, domainGitHub, domainSandbox}},
		{"what's your sol balance?", nil, []domain{domainCrypto}, []domain{domainGitHub}},
		{"yes do it", []string{"want me to send 0.1 sol to that address?"}, []domain{domainCrypto}, nil},
		{"open a PR on my repo", nil, []domain{domainGitHub}, []domain{domainCrypto}},
		{"draft a tweet about our launch", nil, []domain{domainX, domainSocial}, nil},
	}
	for _, tc := range cases {
		var hist []memory.Message
		for _, h := range tc.history {
			hist = append(hist, memory.Message{Role: "assistant", Content: h})
		}
		got := detectDomains(tc.prompt, hist, "")
		for _, d := range tc.want {
			if !got[d] {
				t.Errorf("%q: expected %s active", tc.prompt, d)
			}
		}
		for _, d := range tc.notWant {
			if got[d] {
				t.Errorf("%q: %s should not be active", tc.prompt, d)
			}
		}
	}
}

func TestFocusPromptAndTools(t *testing.T) {
	c := NewClient("k", "m", "", []string{"owner"})
	full := c.systemPrompt("someone", false, nil)
	casual := focusPrompt(full, map[domain]bool{})
	if len(casual) >= len(full) {
		t.Fatalf("casual prompt not trimmed: %d >= %d", len(casual), len(full))
	}
	for _, mustKeep := range []string{"Long-Term Memory on Walrus", "Expressing Yourself Like a Person", "HARD FORMATTING CONSTRAINTS", "You are Jasmine"} {
		if !strings.Contains(casual, mustKeep) {
			t.Errorf("casual prompt lost %q", mustKeep)
		}
	}
	for _, mustDrop := range []string{"Native Crypto Superpowers", "GitHub Intelligence", "Ephemeral Sandbox Runner", "Moltbook AI Social Network"} {
		if strings.Contains(casual, mustDrop) {
			t.Errorf("casual prompt still has %q", mustDrop)
		}
	}
	crypto := focusPrompt(full, map[domain]bool{domainCrypto: true})
	if !strings.Contains(crypto, "Native Crypto Superpowers") || strings.Contains(crypto, "GitHub Intelligence") {
		t.Error("crypto focus kept the wrong sections")
	}

	names := func(ts []ToolDefinition) string {
		var n []string
		for _, t := range ts {
			n = append(n, t.Function.Name)
		}
		return strings.Join(n, ",")
	}
	casualTools := names(focusTools(c.tools, map[domain]bool{}))
	if strings.Contains(casualTools, "send_crypto") || strings.Contains(casualTools, "github_merge_pr") || !strings.Contains(casualTools, "react_to_message") || !strings.Contains(casualTools, "recall_memory") {
		t.Errorf("casual tools wrong: %s", casualTools)
	}
	if !strings.Contains(names(focusTools(c.tools, map[domain]bool{domainCrypto: true})), "send_crypto") {
		t.Error("crypto tools missing when crypto is active")
	}
}

func TestReminderAndCallFocus(t *testing.T) {
	cases := map[string][]domain{
		"jasmine remind me by 3pm to fetch water": {domainRemind},
		"jasmine call me at 6am to wake me up":    {domainCalls, domainRemind},
		"my number is 0803 123 4567":              {domainCalls},
	}
	for prompt, want := range cases {
		got := detectDomains(prompt, nil, "")
		for _, d := range want {
			if !got[d] {
				t.Errorf("%q should activate %s, got %v", prompt, d, got)
			}
		}
	}
	casual := detectDomains("lmaooo okay fine", nil, "")
	if casual[domainRemind] || casual[domainCalls] {
		t.Errorf("casual chat should not load reminders or calls: %v", casual)
	}
}
