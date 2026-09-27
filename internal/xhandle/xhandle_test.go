package xhandle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"@liegeagents", "liegeagents"},
		{"liegeagents", "liegeagents"},
		{"https://x.com/liegeagents", "liegeagents"},
		{"https://twitter.com/curtainrh/", "curtainrh"},
		{"http://www.x.com/@elonmusk", "elonmusk"},
		{"'veilora'", "veilora"},
		{"@test_user_123", "test_user_123"},
	}

	for _, tt := range tests {
		got := NormalizeHandle(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeHandle(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestValidateFormat(t *testing.T) {
	// Valid handles
	valid := []string{"a", "abc", "liegeagents", "curtain_rh", "x_123456789012"}
	for _, h := range valid {
		ok, status, _ := ValidateFormat(h)
		if !ok {
			t.Errorf("ValidateFormat(%q) = false; want true (status %s)", h, status)
		}
	}

	// Too long (> 15 chars)
	long := "thisusernameiswaytoolong"
	ok, status, _ := ValidateFormat(long)
	if ok || status != StatusTooLong {
		t.Errorf("ValidateFormat(%q) = (%v, %s); want (false, StatusTooLong)", long, ok, status)
	}

	// Invalid characters
	invalidChars := []string{"foo-bar", "foo.bar", "foo@bar", "foo bar"}
	for _, h := range invalidChars {
		ok, status, _ := ValidateFormat(h)
		if ok || status != StatusInvalidChar {
			t.Errorf("ValidateFormat(%q) = (%v, %s); want (false, StatusInvalidChar)", h, ok, status)
		}
	}

	// Empty
	ok, status, _ = ValidateFormat("")
	if ok || status != StatusTooShort {
		t.Errorf("ValidateFormat('') = (%v, %s); want (false, StatusTooShort)", ok, status)
	}
}

func TestParseInputHandles(t *testing.T) {
	svc := NewService()

	// 1. Multiple comma-separated handles with @
	input1 := "@liege, @curtain, @veilora"
	res1 := svc.ParseInputHandles(input1)
	expected1 := []string{"liege", "curtain", "veilora"}
	if len(res1) != 3 || res1[0] != expected1[0] || res1[1] != expected1[1] || res1[2] != expected1[2] {
		t.Errorf("ParseInputHandles(%q) = %v; want %v", input1, res1, expected1)
	}

	// 2. Natural query with conversational stop words
	input2 := "is liege available on X?"
	res2 := svc.ParseInputHandles(input2)
	if len(res2) != 1 || res2[0] != "liege" {
		t.Errorf("ParseInputHandles(%q) = %v; want ['liege']", input2, res2)
	}

	// 3. URLs
	input3 := "https://x.com/curtainrh, https://twitter.com/veilora"
	res3 := svc.ParseInputHandles(input3)
	if len(res3) != 2 || res3[0] != "curtainrh" || res3[1] != "veilora" {
		t.Errorf("ParseInputHandles(%q) = %v; want ['curtainrh', 'veilora']", input3, res3)
	}
}

func TestCheckHandleMockAPI(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("username")
		w.Header().Set("Content-Type", "application/json")

		switch username {
		case "available_user":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"valid":true,"reason":"available","msg":"Available!","desc":"Available!"}`))
		case "taken_user":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"valid":false,"reason":"taken","msg":"Username has already been taken","desc":"That username has been taken. Please choose another."}`))
		case "reserved_user":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"valid":false,"reason":"contains_banned_word","msg":"Username is unavailable","desc":"This username is unavailable. Please try another."}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	svc := NewService()
	svc.SetEndpointsForTesting(mockServer.URL, mockServer.URL)

	// 1. Available user
	resAvail, err := svc.CheckHandle(context.Background(), "available_user")
	if err != nil {
		t.Fatalf("CheckHandle error: %v", err)
	}
	if !resAvail.Available || resAvail.Status != StatusAvailable {
		t.Errorf("expected available, got: %+v", resAvail)
	}

	// 2. Taken user
	resTaken, err := svc.CheckHandle(context.Background(), "@taken_user")
	if err != nil {
		t.Fatalf("CheckHandle error: %v", err)
	}
	if resTaken.Available || resTaken.Status != StatusTaken {
		t.Errorf("expected taken, got: %+v", resTaken)
	}

	// 3. Reserved user
	resRes, err := svc.CheckHandle(context.Background(), "reserved_user")
	if err != nil {
		t.Fatalf("CheckHandle error: %v", err)
	}
	if resRes.Available || resRes.Status != StatusReserved {
		t.Errorf("expected reserved, got: %+v", resRes)
	}

	// 4. Over 15 characters (rejected before API)
	resLong, err := svc.CheckHandle(context.Background(), "super_long_username_over_limit")
	if err != nil {
		t.Fatalf("CheckHandle error: %v", err)
	}
	if resLong.Available || resLong.Status != StatusTooLong {
		t.Errorf("expected too_long, got: %+v", resLong)
	}
}

func TestFormatResponse(t *testing.T) {
	svc := NewService()

	results := []HandleResult{
		{
			Username:  "liegeagents",
			Available: true,
			Status:    StatusAvailable,
			Message:   "Available",
		},
		{
			Username:  "elonmusk",
			Available: false,
			Status:    StatusTaken,
			Message:   "Taken",
		},
		{
			Username:  "admin",
			Available: false,
			Status:    StatusReserved,
			Message:   "Reserved (Unavailable)",
		},
	}

	formatted := svc.FormatResponse(results)

	// Check content
	if !strings.Contains(formatted, "@liegeagents") || !strings.Contains(formatted, "Available") {
		t.Errorf("expected liegeagents Available in output: %s", formatted)
	}
	if !strings.Contains(formatted, "@elonmusk") || !strings.Contains(formatted, "Taken") {
		t.Errorf("expected elonmusk Taken in output: %s", formatted)
	}
	if !strings.Contains(formatted, "@admin") || !strings.Contains(formatted, "Reserved") {
		t.Errorf("expected admin Reserved in output: %s", formatted)
	}

	// Strict constraint check: NO emojis, NO em dashes
	if strings.Contains(formatted, "—") || strings.Contains(formatted, "–") {
		t.Errorf("formatted text contains em dash: %s", formatted)
	}
}
