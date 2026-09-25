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
	"shipp/internal/memory"
	"shipp/internal/server"
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
	)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize crypto service: %v", err)
	}
	svmAddr, evmAddr := cryptoSvc.GetAddresses()
	log.Printf("[Main] Crypto Service initialized | SVM: %s | EVM: %s", svmAddr, evmAddr)

	// 4. Initialize AI Client
	aiClient := ai.NewClient(cfg.GroqAPIKey, cfg.GroqModel, cfg.Owners)
	log.Printf("[Main] AI Client initialized with Groq model: %s", cfg.GroqModel)

	// 5. Initialize Telegram Bot
	tgBot, err := bot.NewBot(cfg, aiClient, memStore, cryptoSvc)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize Telegram Bot: %v", err)
	}

	// 6. Initialize & Start HTTP Server for Render Healthchecks & Telegram Webhooks
	httpServer := server.NewServer(cfg.Port)
	httpServer.RegisterHandler("/webhook", tgBot.WebhookHandler)
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
