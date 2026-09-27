package xpost

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExtractTweetID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://x.com/jack/status/20", "20"},
		{"https://twitter.com/jack/status/20?s=20", "20"},
		{"https://fxtwitter.com/elonmusk/status/1789012345678901234", "1789012345678901234"},
		{"https://vxtwitter.com/user/status/987654321", "987654321"},
		{"https://fixupx.com/user/status/11223344", "11223344"},
		{"https://api.fxtwitter.com/status/12345", "12345"},
		{"not a url", ""},
		{"https://x.com/home", ""},
	}

	for _, tt := range tests {
		got := ExtractTweetID(tt.input)
		if got != tt.want {
			t.Errorf("ExtractTweetID(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractTweetURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"check this tweet https://x.com/jack/status/20 what you think", "https://x.com/jack/status/20"},
		{"https://twitter.com/elonmusk/status/123456789 is crazy", "https://twitter.com/elonmusk/status/123456789"},
		{"no link here at all", ""},
	}

	for _, tt := range tests {
		got := ExtractTweetURL(tt.input)
		if got != tt.want {
			t.Errorf("ExtractTweetURL(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatTweet(t *testing.T) {
	tweet := &FxTweet{
		Text: "just setting up my twttr",
		Author: FxAuthor{
			ScreenName: "jack",
			Name:       "jack",
		},
		Likes:    300000,
		Retweets: 120000,
		Replies:  18000,
	}

	formatted := FormatTweet(tweet)
	if !strings.Contains(formatted, "@jack") {
		t.Errorf("expected formatted to contain @jack")
	}
	if !strings.Contains(formatted, "just setting up my twttr") {
		t.Errorf("expected formatted to contain text")
	}
	if !strings.Contains(formatted, "300000 likes") {
		t.Errorf("expected formatted to contain likes")
	}
}

func TestFetchTweetLive(t *testing.T) {
	svc := NewService()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	tweet, err := svc.FetchTweet(ctx, "https://x.com/jack/status/20")
	if err != nil {
		t.Skipf("skipping live test due to network/api availability: %v", err)
	}

	if tweet.Author.ScreenName != "jack" {
		t.Errorf("expected author 'jack', got %q", tweet.Author.ScreenName)
	}
	if !strings.Contains(tweet.Text, "setting up my twttr") {
		t.Errorf("expected tweet text to contain 'setting up my twttr', got %q", tweet.Text)
	}
}
