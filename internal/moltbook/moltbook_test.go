package moltbook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMoltbookMockClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Header.Get("Authorization") != "Bearer test_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/notifications":
			resp := NotificationsResponse{
				Success:     true,
				UnreadCount: 1,
				Notifications: []NotificationItem{
					{
						ID:            "notif-1",
						Type:          "mention",
						Content:       "dragonflier mentioned you",
						RelatedPostID: "post-123",
						IsRead:        false,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost && r.URL.Path == "/notifications/read-all":
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/notifications/read-by-post/"):
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})

		case r.Method == http.MethodGet && r.URL.Path == "/posts":
			submolt := r.URL.Query().Get("submolt_name")
			resp := FeedResponse{
				Success: true,
				Posts: []Post{
					{
						ID:          "p-1",
						Title:       "Post in " + submolt,
						SubmoltName: submolt,
						Upvotes:     10,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodGet && r.URL.Path == "/search":
			query := r.URL.Query().Get("q")
			resp := SearchResponse{
				Success: true,
				Query:   query,
				Results: []SearchResultItem{
					{
						ID:      "search-p-1",
						Type:    "post",
						Title:   "Result for " + query,
						Upvotes: 5,
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient("test_key")
	client.baseURL = server.URL
	client.httpClient = server.Client()

	ctx := context.Background()

	// 1. Notifications
	notifs, err := client.GetNotifications(ctx, 5)
	if err != nil {
		t.Fatalf("GetNotifications failed: %v", err)
	}
	if notifs.UnreadCount != 1 || len(notifs.Notifications) != 1 {
		t.Errorf("unexpected notifications response: %+v", notifs)
	}

	// 2. Mark read
	if err := client.MarkAllNotificationsRead(ctx); err != nil {
		t.Errorf("MarkAllNotificationsRead failed: %v", err)
	}
	if err := client.MarkPostNotificationsRead(ctx, "post-123"); err != nil {
		t.Errorf("MarkPostNotificationsRead failed: %v", err)
	}

	// 3. Submolt Feed
	posts, err := client.GetSubmoltFeed(ctx, "agents", "hot", 5)
	if err != nil {
		t.Fatalf("GetSubmoltFeed failed: %v", err)
	}
	if len(posts) != 1 || posts[0].SubmoltName != "agents" {
		t.Errorf("unexpected posts from submolt feed: %+v", posts)
	}

	// 4. Search
	searchRes, err := client.SearchPosts(ctx, "memory", 5)
	if err != nil {
		t.Fatalf("SearchPosts failed: %v", err)
	}
	if len(searchRes) != 1 || !strings.Contains(searchRes[0].Title, "memory") {
		t.Errorf("unexpected search results: %+v", searchRes)
	}
}
