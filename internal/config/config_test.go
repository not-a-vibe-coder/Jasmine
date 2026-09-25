package config

import (
	"os"
	"testing"
)

func TestConfigOwners(t *testing.T) {
	cfg := &Config{
		Owners: []string{"skipp_dev", "shigarakixbt"},
	}

	tests := []struct {
		username string
		expected bool
	}{
		{"@skipp_dev", true},
		{"skipp_dev", true},
		{"SKIPP_DEV", true},
		{"@shigarakiXBT", true},
		{"shigarakiXBT", true},
		{"random_user", false},
		{"", false},
	}

	for _, tt := range tests {
		result := cfg.IsOwner(tt.username)
		if result != tt.expected {
			t.Errorf("IsOwner(%q) = %v; want %v", tt.username, result, tt.expected)
		}
	}
}

func TestLoadConfigDefaultPort(t *testing.T) {
	_ = os.Setenv("TELEGRAM_BOT_TOKEN", "mock_token")
	_ = os.Setenv("PORT", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("expected default port 8080, got %s", cfg.Port)
	}
}
