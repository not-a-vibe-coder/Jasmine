package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSearchEmptyQuery(t *testing.T) {
	svc := NewService("")
	res, err := svc.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "Please provide a search query") {
		t.Errorf("expected empty query prompt, got %s", res)
	}
}

func TestSearchUserQueries(t *testing.T) {
	svc := NewService("")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Query 1: current president of Uruguay 2025
	q1 := "current president of Uruguay 2025"
	res1, err := svc.Search(ctx, q1)
	if err != nil {
		t.Fatalf("search q1 failed: %v", err)
	}
	t.Logf("Result for '%s':\n%s\n", q1, res1)

	lower1 := strings.ToLower(res1)
	if strings.Contains(lower1, "no relevant live search results found") {
		t.Errorf("q1 returned no results: %s", res1)
	}
	if !strings.Contains(lower1, "uruguay") && !strings.Contains(lower1, "orsi") {
		t.Errorf("expected q1 to mention Uruguay or Orsi, got: %s", res1)
	}

	// Query 2: why US and other countries do not recognize Nicolas Maduro after disputed 2024 Venezuela election
	q2 := "why US and other countries do not recognize Nicolas Maduro after disputed 2024 Venezuela election"
	res2, err := svc.Search(ctx, q2)
	if err != nil {
		t.Fatalf("search q2 failed: %v", err)
	}
	t.Logf("Result for '%s':\n%s\n", q2, res2)

	lower2 := strings.ToLower(res2)
	if strings.Contains(lower2, "no relevant live search results found") {
		t.Errorf("q2 returned no results: %s", res2)
	}
	if !strings.Contains(lower2, "maduro") && !strings.Contains(lower2, "venezuela") {
		t.Errorf("expected q2 to mention Maduro or Venezuela, got: %s", res2)
	}
}

func TestFetchWebPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		htmlDoc := `<!DOCTYPE html><html><head><title>Liege — Agents work. You're the liege.</title><meta name="description" content="The agent labor market built for work. Explore Liege agents, job escrow, evaluation, and client-controlled strategy wallets."/></head><body><h1>Welcome to Liege</h1><p>Hire autonomous AI agents for software engineering tasks.</p><script>console.log("ignore me");</script><style>body { color: red; }</style></body></html>`
		w.Write([]byte(htmlDoc))
	}))
	defer ts.Close()

	svc := NewService("")
	res, err := svc.FetchWebPage(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("FetchWebPage failed: %v", err)
	}

	if !strings.Contains(res, "Liege — Agents work") {
		t.Errorf("expected title in output, got: %s", res)
	}
	if !strings.Contains(res, "agent labor market built for work") {
		t.Errorf("expected description in output, got: %s", res)
	}
	if !strings.Contains(res, "Hire autonomous AI agents") {
		t.Errorf("expected body text in output, got: %s", res)
	}
	if strings.Contains(res, "ignore me") || strings.Contains(res, "color: red") {
		t.Errorf("expected script and style tags to be stripped, got: %s", res)
	}
}

func TestSearchWithDomainProactiveFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		htmlDoc := `<!DOCTYPE html><html><head><title>Liege Agents Platform</title><meta name="description" content="Decentralized agent coordination"/></head><body><p>Live on production</p></body></html>`
		w.Write([]byte(htmlDoc))
	}))
	defer ts.Close()

	svc := NewService("")
	// Query containing the test server URL
	res, err := svc.Search(context.Background(), "check out "+ts.URL)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if !strings.Contains(res, "Liege Agents Platform") {
		t.Errorf("expected proactive website content in search result, got: %s", res)
	}
}

