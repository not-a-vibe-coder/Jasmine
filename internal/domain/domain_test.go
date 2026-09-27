package domain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseInputDomains(t *testing.T) {
	svc := NewService("")

	// 1. Multiple comma-separated domains
	input1 := "liegeagents.app, curtainrh.com"
	res1 := svc.ParseInputDomains(input1)
	if len(res1) != 2 || res1[0] != "liegeagents.app" || res1[1] != "curtainrh.com" {
		t.Errorf("ParseInputDomains(%q) = %v; want [liegeagents.app curtainrh.com]", input1, res1)
	}

	// 2. URL prefix stripping
	input2 := "https://www.curtainrh.com, http://liegeagents.app"
	res2 := svc.ParseInputDomains(input2)
	if len(res2) != 2 || res2[0] != "curtainrh.com" || res2[1] != "liegeagents.app" {
		t.Errorf("ParseInputDomains(%q) = %v; want [curtainrh.com liegeagents.app]", input2, res2)
	}

	// 3. Bare brand name auto-expansion
	input3 := "curtainrh"
	res3 := svc.ParseInputDomains(input3)
	if len(res3) < 4 {
		t.Errorf("ParseInputDomains(%q) = %v; expected expanded TLDs", input3, res3)
	}
	hasCom := false
	hasApp := false
	for _, d := range res3 {
		if d == "curtainrh.com" {
			hasCom = true
		}
		if d == "curtainrh.app" {
			hasApp = true
		}
	}
	if !hasCom || !hasApp {
		t.Errorf("expected auto-expanded domains to contain curtainrh.com and curtainrh.app, got %v", res3)
	}
}

func TestFormatResponse(t *testing.T) {
	svc := NewService("")

	resp := &SearchResponse{
		Results: []DomainResult{
			{
				Domain:       "liegeagents.app",
				Available:    true,
				Years:        1,
				Price:        14.99,
				RenewalPrice: 15.00,
				Premium:      false,
			},
			{
				Domain:       "curtainrh.com",
				Available:    true,
				Years:        1,
				Price:        11.25,
				RenewalPrice: 11.25,
				Premium:      false,
			},
			{
				Domain:    "google.com",
				Available: false,
			},
		},
	}

	formatted := svc.FormatResponse(resp)

	// Check required contents
	if !strings.Contains(formatted, "liegeagents.app") || !strings.Contains(formatted, "$14.99/yr") {
		t.Errorf("expected formatted output to contain liegeagents.app with pricing, got: %s", formatted)
	}
	if !strings.Contains(formatted, "curtainrh.com") || !strings.Contains(formatted, "$11.25/yr") {
		t.Errorf("expected formatted output to contain curtainrh.com with pricing, got: %s", formatted)
	}
	if !strings.Contains(formatted, "google.com") || !strings.Contains(formatted, "Taken") {
		t.Errorf("expected formatted output to contain taken google.com, got: %s", formatted)
	}

	// Strict constraints: NO emojis, NO em dashes
	if strings.Contains(formatted, "—") || strings.Contains(formatted, "–") {
		t.Errorf("formatted text contains em dash or en dash: %s", formatted)
	}
}

func TestMockVercelSearchAPI(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"results": [
				{"domain": "liegeagents.app", "available": true, "years": 1, "price": 14.99, "renewalPrice": 15, "premium": false},
				{"domain": "curtainrh.com", "available": true, "years": 1, "price": 11.25, "renewalPrice": 11.25, "premium": false}
			]
		}`))
	}))
	defer mockServer.Close()

	svc := NewService("")
	svc.SetEndpointForTesting(mockServer.URL)

	resp, err := svc.SearchDomains(context.Background(), []string{"liegeagents.app", "curtainrh.com"})
	if err != nil {
		t.Fatalf("SearchDomains failed: %v", err)
	}

	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Domain != "liegeagents.app" || !resp.Results[0].Available {
		t.Errorf("unexpected result 0: %+v", resp.Results[0])
	}
	if resp.Results[1].Domain != "curtainrh.com" || resp.Results[1].Price != 11.25 {
		t.Errorf("unexpected result 1: %+v", resp.Results[1])
	}
}
