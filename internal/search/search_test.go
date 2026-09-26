package search

import (
	"context"
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
