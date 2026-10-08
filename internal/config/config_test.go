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
		// Display-name spoofing must not grant ownership
		{"Skipp Air", false},
		{"skipp_dev_fan", false},
		{"shigaraki", false},
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

func TestRenderExternalURLDerivesWebhookAndCallback(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "mock_token")
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("SANDBOX_CALLBACK_URL", "")
	t.Setenv("RENDER_EXTERNAL_URL", "https://jasmine.onrender.com/")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookURL != "https://jasmine.onrender.com/webhook" {
		t.Errorf("webhook = %q", cfg.WebhookURL)
	}
	if cfg.SandboxCallbackURL != "https://jasmine.onrender.com/api/sandbox/callback" {
		t.Errorf("callback = %q", cfg.SandboxCallbackURL)
	}

	t.Setenv("USE_POLLING", "true")
	cfg, _ = LoadConfig()
	if cfg.WebhookURL != "" {
		t.Errorf("USE_POLLING should keep polling mode, got webhook %q", cfg.WebhookURL)
	}
}

func TestOwnerIDs(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "mock_token")
	t.Setenv("OWNER_IDS", " 12345, notanumber,678 ")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsOwnerID(12345) || !cfg.IsOwnerID(678) || cfg.IsOwnerID(999) || cfg.IsOwnerID(0) {
		t.Errorf("owner IDs parsed wrong: %v", cfg.OwnerIDs)
	}
}
