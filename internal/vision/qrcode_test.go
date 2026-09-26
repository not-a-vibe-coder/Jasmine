package vision

import (
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
