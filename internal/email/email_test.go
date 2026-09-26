package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmailServiceInit(t *testing.T) {
	svc := NewService("re_123", "", "shippzero@atomicmail.io")
	if !svc.IsConfigured() {
		t.Errorf("expected service to be configured")
	}
	if svc.fromEmail != "Shipp <shipp@bot.davidnzube.xyz>" {
		t.Errorf("expected default fromEmail, got %s", svc.fromEmail)
	}
	if svc.replyToEmail != "shippzero@atomicmail.io" {
		t.Errorf("expected replyTo to be shippzero@atomicmail.io, got %s", svc.replyToEmail)
	}
}

func TestEmailValidation(t *testing.T) {
	svc := NewService("re_123", "shipp@bot.davidnzube.xyz", "shippzero@atomicmail.io")

	// Missing recipient
	_, err := svc.Send(context.Background(), "", "Hello", "World")
	if err == nil {
		t.Errorf("expected error for empty recipient")
	}

	// Invalid email
	_, err = svc.Send(context.Background(), "invalid-email-address", "Hello", "World")
	if err == nil {
		t.Errorf("expected error for invalid email syntax")
	}
}

func TestEmailSendMock(t *testing.T) {
	var receivedPayload map[string]interface{}
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&receivedPayload)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": "msg_test_12345"}`))
	}))
	defer server.Close()

	svc := NewService("re_mock_key", "Shipp <shipp@bot.davidnzube.xyz>", "shippzero@atomicmail.io")
	svc.baseURL = server.URL

	res, err := svc.Send(context.Background(), "recipient@example.com", "Test Subject", "Hello world from Shipp!\n\nSecond line.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.ID != "msg_test_12345" {
		t.Errorf("expected ID msg_test_12345, got %s", res.ID)
	}
	if authHeader != "Bearer re_mock_key" {
		t.Errorf("expected auth Bearer re_mock_key, got %s", authHeader)
	}
	if receivedPayload["reply_to"] != "shippzero@atomicmail.io" {
		t.Errorf("expected reply_to to be shippzero@atomicmail.io, got %v", receivedPayload["reply_to"])
	}

	formatted := FormatEmailSent(res)
	if !strings.Contains(formatted, "recipient@example.com") || !strings.Contains(formatted, "msg_test_12345") {
		t.Errorf("formatted message missing details: %s", formatted)
	}
}
