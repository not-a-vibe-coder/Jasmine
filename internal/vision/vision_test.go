package vision

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

func TestVisionGeminiColorDetection(t *testing.T) {
	_ = godotenv.Load("../../.env")
	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		t.Skip("skipping test: GEMINI_API_KEY not set")
	}

	svc := NewService(geminiKey)

	// 1x1 Coral/Red PNG
	b64Image := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	rawBytes, err := base64.StdEncoding.DecodeString(b64Image)
	if err != nil {
		t.Fatalf("failed to decode base64 image: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	analysis, err := svc.AnalyzeImage(ctx, rawBytes, "image/png", "What dominant color is this pixel?")
	if err != nil {
		t.Fatalf("AnalyzeImage failed: %v", err)
	}

	t.Logf("Vision response:\n%s", analysis)
	lower := strings.ToLower(analysis)
	if !strings.Contains(lower, "red") && !strings.Contains(lower, "coral") && !strings.Contains(lower, "pink") {
		t.Errorf("expected vision to detect red/coral color, got: %s", analysis)
	}
}
