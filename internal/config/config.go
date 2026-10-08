package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Owners: @usernames, plus numeric Telegram user IDs which survive username changes
	Owners   []string
	OwnerIDs []int64

	// Telegram
	TelegramBotToken    string
	TelegramBotUsername string

	// Groq AI
	GroqAPIKey string
	GroqModel  string

	// Storage
	RedisURL    string
	DatabaseURL string

	// Wallets
	SVMWalletPublicKey  string
	SVMWalletPrivateKey string
	EVMWalletPublicKey  string
	EVMWalletPrivateKey string

	// RPCs
	SVMRPCURL        string
	SVMFallbackRPC   string
	BaseRPCURL       string
	RobinhoodRPCURL  string
	EthereumRPCURL   string
	ArbitrumRPCURL   string
	BnbRPCURL        string
	HyperliquidRPCURL string
	MonadRPCURL      string

	// GitHub
	GithubPAT      string
	GithubUsername string
	PersonalEmail  string

	// Resend Email
	ResendAPIKey    string
	ResendFromEmail string

	// Search & Analytics & Vision & Registrar & AI Social
	TavilyAPIKey   string
	CodexIOAPIKey  string
	GeminiAPIKey   string
	VercelToken    string
	MoltbookAPIKey string

	// Fallback AI providers (OpenAI-compatible). Models are comma-separated overrides.
	CerebrasAPIKey    string
	CerebrasModels    string
	OpenRouterAPIKey  string
	OpenRouterModels  string
	MistralAPIKey     string
	MistralModels     string
	HuggingFaceAPIKey string
	HuggingFaceModels string

	// Voice, stickers and GIFs
	VoiceName    string   // Microsoft Edge neural voice, e.g. en-US-AvaMultilingualNeural
	OrpheusVoice string   // Groq Orpheus voice, e.g. tara
	GiphyAPIKey  string
	StickerSets  []string // public Telegram sticker set names

	// Image generation
	HFImageModel       string
	PollinationsAPIKey string
	PollinationsModels string
	GeminiImageModel   string

	// Walrus Memory
	MemwalServerURL   string
	MemwalAccountID   string
	MemwalDelegateKey string

	// Sandbox runner
	SandboxRepo        string
	SandboxPrivateRepo string
	SandboxCallbackURL string

	// Server & Webhook
	Port          string
	WebhookURL    string
	WebhookSecret string
}

func LoadConfig() (*Config, error) {
	// Attempt to load .env if present (ignore error if not present in production)
	_ = godotenv.Load(".env", "../.env")

	cfg := &Config{
		TelegramBotToken:    os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramBotUsername: os.Getenv("TELEGRAM_BOT_USERNAME"),
		GroqAPIKey:          os.Getenv("GROQ_API_KEY"),
		GroqModel:           os.Getenv("GROQ_MODEL"),
		RedisURL:            os.Getenv("REDIS_URL"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		SVMWalletPublicKey:  os.Getenv("SVM_WALLET_PUBLIC_KEY"),
		SVMWalletPrivateKey: os.Getenv("SVM_WALLET_PRIVATE_KEY"),
		EVMWalletPublicKey:  os.Getenv("EVM_WALLET_PUBLIC_KEY"),
		EVMWalletPrivateKey: os.Getenv("EVM_WALLET_PRIVATE_KEY"),
		SVMRPCURL:           os.Getenv("SVM_RPC_URL"),
		SVMFallbackRPC:      "https://api.mainnet-beta.solana.com",
		BaseRPCURL:          os.Getenv("BASE_RPC_URL"),
		RobinhoodRPCURL:     os.Getenv("ROBINHOOD_RPC_URL"),
		EthereumRPCURL:      os.Getenv("ETHEREUM_RPC_URL"),
		ArbitrumRPCURL:      os.Getenv("ARBITRUM_RPC_URL"),
		BnbRPCURL:           os.Getenv("BNB_RPC_URL"),
		HyperliquidRPCURL:   os.Getenv("HYPERLIQUID_RPC_URL"),
		MonadRPCURL:         os.Getenv("MONAD_RPC_URL"),
		GithubPAT:           os.Getenv("GITHUB_PAT"),
		GithubUsername:      os.Getenv("GITHUB_USERNAME"),
		PersonalEmail:       os.Getenv("PERSONAL_EMAIL"),
		ResendAPIKey:        os.Getenv("RESEND_API_KEY"),
		ResendFromEmail:     os.Getenv("RESEND_FROM_EMAIL"),
		TavilyAPIKey:        os.Getenv("TAVILY_API_KEY"),
		CodexIOAPIKey:       os.Getenv("CODEX_IO_API_KEY"),
		GeminiAPIKey:        os.Getenv("GEMINI_API_KEY"),
		VercelToken:         os.Getenv("VERCEL_TOKEN"),
		MoltbookAPIKey:      os.Getenv("MOLTBOOK_API_KEY"),
		CerebrasAPIKey:      os.Getenv("CEREBRAS_API_KEY"),
		CerebrasModels:      os.Getenv("CEREBRAS_MODELS"),
		OpenRouterAPIKey:    os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModels:    os.Getenv("OPENROUTER_MODELS"),
		MistralAPIKey:       os.Getenv("MISTRAL_API_KEY"),
		MistralModels:       os.Getenv("MISTRAL_MODELS"),
		HuggingFaceAPIKey:   os.Getenv("HF_TOKEN"),
		HuggingFaceModels:   os.Getenv("HF_MODELS"),
		VoiceName:           os.Getenv("VOICE_NAME"),
		OrpheusVoice:        os.Getenv("ORPHEUS_VOICE"),
		GiphyAPIKey:         os.Getenv("GIPHY_API_KEY"),
		HFImageModel:        os.Getenv("HF_IMAGE_MODEL"),
		PollinationsAPIKey:  os.Getenv("POLLINATIONS_API_KEY"),
		PollinationsModels:  os.Getenv("POLLINATIONS_MODELS"),
		GeminiImageModel:    os.Getenv("GEMINI_IMAGE_MODEL"),
		MemwalServerURL:     os.Getenv("MEMWAL_SERVER_URL"),
		MemwalAccountID:     os.Getenv("MEMWAL_ACCOUNT_ID"),
		MemwalDelegateKey:   os.Getenv("MEMWAL_DELEGATE_KEY"),
		SandboxRepo:         os.Getenv("SANDBOX_REPO"),
		SandboxPrivateRepo:  os.Getenv("SANDBOX_PRIVATE_REPO"),
		SandboxCallbackURL:  os.Getenv("SANDBOX_CALLBACK_URL"),
		Port:                os.Getenv("PORT"),
		WebhookURL:          os.Getenv("WEBHOOK_URL"),
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
	}

	// On Render, the public URL is injected automatically; use it for the webhook
	// unless WEBHOOK_URL was set explicitly. USE_POLLING=true opts out.
	if cfg.WebhookURL == "" && os.Getenv("USE_POLLING") != "true" {
		if ext := strings.TrimRight(os.Getenv("RENDER_EXTERNAL_URL"), "/"); ext != "" {
			cfg.WebhookURL = ext + "/webhook"
		}
	}

	if cfg.SandboxCallbackURL == "" && cfg.WebhookURL != "" {
		// WEBHOOK_URL is https://host/webhook; the sandbox calls back on the same host.
		cfg.SandboxCallbackURL = strings.TrimSuffix(strings.TrimRight(cfg.WebhookURL, "/"), "/webhook") + "/api/sandbox/callback"
	}

	if cfg.CodexIOAPIKey == "" {
		cfg.CodexIOAPIKey = os.Getenv("CODEX_API_KEY")
	}

	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	if cfg.GroqModel == "" {
		cfg.GroqModel = "qwen/qwen3.8-27b"
	}

	// Parse owners
	ownersRaw := os.Getenv("OWNERS_USERNAME")
	if ownersRaw == "" {
		ownersRaw = "@jackdotsol"
	}
	parts := strings.Split(ownersRaw, ",")
	for _, p := range parts {
		cleaned := strings.TrimSpace(p)
		cleaned = strings.TrimPrefix(cleaned, "@")
		cleaned = strings.ToLower(cleaned)
		if cleaned != "" {
			cfg.Owners = append(cfg.Owners, cleaned)
		}
	}

	for _, raw := range strings.Split(os.Getenv("STICKER_SETS"), ",") {
		if name := strings.TrimSpace(raw); name != "" {
			cfg.StickerSets = append(cfg.StickerSets, name)
		}
	}

	for _, raw := range strings.Split(os.Getenv("OWNER_IDS"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id != 0 {
			cfg.OwnerIDs = append(cfg.OwnerIDs, id)
		}
	}

	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	return cfg, nil
}

// IsOwnerID checks a numeric Telegram user ID against OWNER_IDS. IDs never change,
// so this is the preferred way to identify owners.
func (c *Config) IsOwnerID(id int64) bool {
	for _, o := range c.OwnerIDs {
		if id != 0 && id == o {
			return true
		}
	}
	return false
}

// IsOwner checks whether a Telegram @username exactly matches one of the bot owners.
// Display names are never trusted: anyone can set their first name to an owner's name.
func (c *Config) IsOwner(username string) bool {
	if username == "" {
		return false
	}
	cleaned := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(username)), "@")
	for _, owner := range c.Owners {
		if cleaned == owner {
			return true
		}
	}
	return false
}
