package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

type serviceHealth struct {
	Status    string  `json:"status"`
	LatencyMs float64 `json:"latencyMs"`
	Error     string  `json:"error,omitempty"`
}

type healthResponse struct {
	Status    string                   `json:"status"`
	Timestamp string                   `json:"timestamp"`
	Services  map[string]serviceHealth `json:"services"`
}

type Server struct {
	httpServer *http.Server
	mux        *http.ServeMux
	db         *sql.DB
	redis      *redis.Client
}

func probePostgres(ctx context.Context, db *sql.DB) serviceHealth {
	if db == nil {
		return serviceHealth{Status: "down", LatencyMs: 0, Error: "database connection not initialized"}
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var one int
	err := db.QueryRowContext(probeCtx, `SELECT 1`).Scan(&one)
	latency := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		return serviceHealth{Status: "down", LatencyMs: latency, Error: err.Error()}
	}

	return serviceHealth{Status: "ok", LatencyMs: latency}
}

func probeRedis(ctx context.Context, redisClient *redis.Client) serviceHealth {
	if redisClient == nil {
		return serviceHealth{Status: "disabled", LatencyMs: 0}
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	err := redisClient.Ping(probeCtx).Err()
	latency := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		return serviceHealth{Status: "down", LatencyMs: latency, Error: err.Error()}
	}

	return serviceHealth{Status: "ok", LatencyMs: latency}
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	pgHealth := probePostgres(r.Context(), s.db)
	redisHealth := probeRedis(r.Context(), s.redis)

	overallStatus := "ok"
	if pgHealth.Status != "ok" {
		overallStatus = "down"
	}

	statusCode := http.StatusOK
	if overallStatus != "ok" {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:    overallStatus,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Services: map[string]serviceHealth{
			"postgres": pgHealth,
			"redis":    redisHealth,
		},
	})
}

func NewServer(port string, db *sql.DB, redisClient *redis.Client) *Server {
	mux := http.NewServeMux()

	s := &Server{
		mux:   mux,
		db:    db,
		redis: redisClient,
	}

	mux.HandleFunc("/healthz", s.healthHandler)
	mux.HandleFunc("/health", s.healthHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Shipp Telegram Bot is running!"))
	})

	s.httpServer = &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return s
}

func (s *Server) RegisterHandler(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

func (s *Server) Start() error {
	log.Printf("[Server] Healthcheck HTTP server listening on %s", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server error: %w", err)
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
