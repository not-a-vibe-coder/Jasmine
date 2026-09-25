package search

import (
	"context"
	"strings"
	"testing"
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

func TestSearchLiveQueries(t *testing.T) {
	svc := NewService("")
	ctx := context.Background()

	// Test 1: president of Uruguay
	res, err := svc.Search(ctx, "president of Uruguay")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(res), "uruguay") {
		t.Errorf("expected result to mention Uruguay, got %s", res)
	}

	// Test 2: president of Venezuela
	res2, err := svc.Search(ctx, "president of Venezuela")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(res2), "venezuela") {
		t.Errorf("expected result to mention Venezuela, got %s", res2)
	}
}
