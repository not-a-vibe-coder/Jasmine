package vision

import (
	"strings"
	"testing"

	"rsc.io/qr"
)

func TestDecodeQRCode(t *testing.T) {
	expectedPayload := "solana:HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr"

	// Generate QR code PNG in memory
	code, err := qr.Encode(expectedPayload, qr.M)
	if err != nil {
		t.Fatalf("failed to generate test QR code: %v", err)
	}

	pngBytes := code.PNG()
	if len(pngBytes) == 0 {
		t.Fatalf("generated PNG bytes are empty")
	}

	decoded, err := DecodeQRCode(pngBytes)
	if err != nil {
		t.Fatalf("DecodeQRCode failed: %v", err)
	}

	if decoded != expectedPayload {
		t.Errorf("DecodeQRCode = %q; want %q", decoded, expectedPayload)
	}
}

func TestDecodeQRCodeNonQR(t *testing.T) {
	// 1x1 dummy image or plain text should return error
	_, err := DecodeQRCode([]byte("not an image"))
	if err == nil {
		t.Errorf("expected error on non-image bytes, got nil")
	}
}

func TestFormatQRPerception(t *testing.T) {
	tests := []struct {
		input       string
		mustContain string
	}{
		{"https://github.com/davidnzube101/shipp", "Web URL -> https://github.com/davidnzube101/shipp"},
		{"http://example.com/dapp", "Web URL -> http://example.com/dapp"},
		{"t.me/Shipp0Bot", "Deep Link/Contact -> t.me/Shipp0Bot"},
		{"solana:HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr", "Crypto Transfer Request"},
		{"0x0a2e799d0b57217a1066a4CDD132F01215E132b8", "EVM Address"},
		{"WIFI:S:MyNetwork;T:WPA;P:SecretPass;;", "Wi-Fi Credentials"},
		{"Hello Telegram Group!", "Text/Data -> Hello Telegram Group!"},
	}

	for _, tt := range tests {
		result := FormatQRPerception(tt.input)
		if !strings.Contains(result, tt.mustContain) {
			t.Errorf("FormatQRPerception(%q) = %q; want it to contain %q", tt.input, result, tt.mustContain)
		}
	}
}

