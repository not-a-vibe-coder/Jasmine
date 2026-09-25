package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Owners
	Owners []string

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

	// Server & Webhook
	Port       string
	WebhookURL string
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
		Port:                os.Getenv("PORT"),
		WebhookURL:          os.Getenv("WEBHOOK_URL"),
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
		ownersRaw = "@skipp_dev,@shigarakiXBT"
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

	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	return cfg, nil
}

// IsOwner checks whether a Telegram username is one of the bot owners
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
