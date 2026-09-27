package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shipp/internal/ai"
	"shipp/internal/bot"
	"shipp/internal/config"
	"shipp/internal/crypto"
	"shipp/internal/domain"
	"shipp/internal/email"
	"shipp/internal/github"
	"shipp/internal/memory"
	"shipp/internal/price"
	"shipp/internal/sandbox"
	"shipp/internal/search"
	"shipp/internal/server"
	"shipp/internal/token"
	"shipp/internal/vision"
	"shipp/internal/xhandle"
)

func main() {
	log.Printf("-----------------------------------------")
	log.Printf("🚀 Starting Shipp Telegram Bot")
	log.Printf("-----------------------------------------")

	// 1. Load Config
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("[Main] Configuration error: %v", err)
	}
	log.Printf("[Main] Config loaded successfully (Owners: %v)", cfg.Owners)

	// 2. Initialize Memory Store (Postgres + Redis hybrid with in-memory fallback)
	memStore, err := memory.NewHybridStore(cfg.DatabaseURL, cfg.RedisURL)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize memory store: %v", err)
	}
	defer memStore.Close()

	// 3. Initialize Crypto Service
	cryptoSvc, err := crypto.NewService(
		cfg.SVMWalletPublicKey,
		cfg.SVMWalletPrivateKey,
		cfg.SVMRPCURL,
		cfg.SVMFallbackRPC,
		cfg.EVMWalletPublicKey,
		cfg.EVMWalletPrivateKey,
		cfg.BaseRPCURL,
		cfg.EthereumRPCURL,
		cfg.ArbitrumRPCURL,
		cfg.BnbRPCURL,
		cfg.RobinhoodRPCURL,
	)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize crypto service: %v", err)
	}
	svmAddr, evmAddr := cryptoSvc.GetAddresses()
	log.Printf("[Main] Crypto Service initialized | SVM: %s | EVM: %s", svmAddr, evmAddr)

	// 4. Initialize AI Client & Search Service
	aiClient := ai.NewClient(cfg.GroqAPIKey, cfg.GroqModel, cfg.GeminiAPIKey, cfg.Owners)
	log.Printf("[Main] AI Client initialized with Groq model: %s (Gemini Flash fallback active)", cfg.GroqModel)

	searchSvc := search.NewService(cfg.TavilyAPIKey)
	log.Printf("[Main] Web Search Service initialized (DuckDuckGo + Wikipedia active)")

	tokenSvc := token.NewService(cfg.CodexIOAPIKey)
	log.Printf("[Main] Token CA Analytics Service initialized (DexScreener + Codex.io active)")

	priceSvc := price.NewService()
	log.Printf("[Main] Price Service initialized (CoinGecko Simple Price + Cache active)")

	visionSvc := vision.NewService(cfg.GeminiAPIKey)
	log.Printf("[Main] Vision Service initialized (Gemini Flash Multimodal active)")

	githubSvc := github.NewService(cfg.GithubPAT, cfg.GithubUsername, cfg.PersonalEmail)
	log.Printf("[Main] GitHub Service initialized for user @%s (Email: %s)", cfg.GithubUsername, cfg.PersonalEmail)

	emailSvc := email.NewService(cfg.ResendAPIKey, cfg.ResendFromEmail, cfg.PersonalEmail)
	if emailSvc.IsConfigured() {
		log.Printf("[Main] Resend Email Service initialized (From: %s | Reply-To: %s)", cfg.ResendFromEmail, cfg.PersonalEmail)
	} else {
		log.Printf("[Main] Resend Email Service not configured (RESEND_API_KEY missing)")
	}

	// 5. Initialize Ephemeral Sandbox Service (Blink Compute pattern)
	sandboxSvc := sandbox.NewService(cfg.GithubPAT, "ShippZero/sandbox", "https://bot.davidnzube.xyz/api/sandbox/callback")
	log.Printf("[Main] Sandbox Service initialized (ShippZero/sandbox ephemeral VM runner active)")

	// 6. Initialize Vercel Domain Registrar Service
	domainSvc := domain.NewService(cfg.VercelToken)
	log.Printf("[Main] Vercel Domain Registrar Service initialized")

	// 7. Initialize X Handle Availability Service
	xhandleSvc := xhandle.NewService()
	log.Printf("[Main] X Handle Availability Service initialized")

	// 8. Initialize Telegram Bot
	tgBot, err := bot.NewBot(cfg, aiClient, memStore, cryptoSvc, searchSvc, tokenSvc, priceSvc, visionSvc, githubSvc, emailSvc, sandboxSvc, domainSvc, xhandleSvc)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize Telegram Bot: %v", err)
	}

	// 7. Initialize & Start HTTP Server for Render Healthchecks & Webhooks
	httpServer := server.NewServer(cfg.Port, memStore.GetDB(), memStore.GetRedis())
	httpServer.RegisterHandler("/webhook", tgBot.WebhookHandler)
	httpServer.RegisterHandler("/api/sandbox/callback", sandboxSvc.CallbackHTTPHandler)
	go func() {
		if err := httpServer.Start(); err != nil {
			log.Printf("[Main] HTTP server exited: %v", err)
		}
	}()

	// 7. Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run Bot polling in goroutine
	go func() {
		if err := tgBot.Start(ctx); err != nil {
			log.Fatalf("[Main] Bot error: %v", err)
		}
	}()

	// 8. Wait for OS termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	log.Printf("[Main] Received signal %v, initiating graceful shutdown...", sig)
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Stop(shutdownCtx)

	log.Printf("[Main] Shipp Bot gracefully stopped. Goodbye!")
}
