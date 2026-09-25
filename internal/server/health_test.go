package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	s := NewServer("8080", nil, nil)
	handler := s.httpServer.Handler

	// 1. Test /healthz with nil DB (status down)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable when DB is nil, got %d", resp.StatusCode)
	}

	var hr healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}
	if hr.Status != "down" {
		t.Errorf("expected overall status 'down', got %s", hr.Status)
	}
	if hr.Services["postgres"].Status != "down" {
		t.Errorf("expected postgres status 'down', got %s", hr.Services["postgres"].Status)
	}
	if hr.Services["redis"].Status != "disabled" {
		t.Errorf("expected redis status 'disabled' when client is nil, got %s", hr.Services["redis"].Status)
	}

	// 2. Test /health route
	reqHealth := httptest.NewRequest("GET", "/health", nil)
	wHealth := httptest.NewRecorder()
	handler.ServeHTTP(wHealth, reqHealth)
	if wHealth.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected /health to return 503 when DB is nil, got %d", wHealth.Result().StatusCode)
	}

	// 3. Test / root
	reqRoot := httptest.NewRequest("GET", "/", nil)
	wRoot := httptest.NewRecorder()
	handler.ServeHTTP(wRoot, reqRoot)

	if wRoot.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for /, got %d", wRoot.Result().StatusCode)
	}
}
