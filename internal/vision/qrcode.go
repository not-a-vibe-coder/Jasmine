package vision

import (
	"bytes"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/tuotoo/qrcode"
)

// DecodeQRCode scans an image for QR codes and extracts the decoded payload string.
// Returns an error if no QR code is found or if decoding fails.
func DecodeQRCode(imageBytes []byte) (string, error) {
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("empty image bytes")
	}

	matrix, err := qrcode.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return "", err
	}

	content := strings.TrimSpace(matrix.Content)
	if content == "" {
		return "", fmt.Errorf("empty qr code content")
	}

	return content, nil
}

// FormatQRPerception formats decoded QR code content with its detected type
// (Web URL, Crypto Address/Transfer, Deep Link, Wi-Fi, or Data) for AI reasoning.
func FormatQRPerception(content string) string {
	cleaned := strings.TrimSpace(content)
	if cleaned == "" {
		return ""
	}

	lower := strings.ToLower(cleaned)

	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return fmt.Sprintf("[Deterministic QR Code Decoded: Web URL -> %s]", cleaned)
	}

	if strings.HasPrefix(lower, "solana:") || strings.HasPrefix(lower, "ethereum:") || strings.HasPrefix(lower, "bitcoin:") {
		return fmt.Sprintf("[Deterministic QR Code Decoded: Crypto Transfer Request -> %s]", cleaned)
	}

	if strings.HasPrefix(lower, "tg:") || strings.HasPrefix(lower, "t.me/") || strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") {
		return fmt.Sprintf("[Deterministic QR Code Decoded: Deep Link/Contact -> %s]", cleaned)
	}

	if strings.HasPrefix(lower, "wifi:") {
		return fmt.Sprintf("[Deterministic QR Code Decoded: Wi-Fi Credentials -> %s]", cleaned)
	}

	// Check if pure 0x EVM address (42 hex chars)
	if strings.HasPrefix(cleaned, "0x") && len(cleaned) == 42 {
		return fmt.Sprintf("[Deterministic QR Code Decoded: EVM Address -> %s]", cleaned)
	}

	// Check if Solana base58 address (32-44 chars)
	if len(cleaned) >= 32 && len(cleaned) <= 44 && !strings.Contains(cleaned, " ") && !strings.Contains(cleaned, "/") {
		return fmt.Sprintf("[Deterministic QR Code Decoded: Potential Crypto Address/Base58 -> %s]", cleaned)
	}

	return fmt.Sprintf("[Deterministic QR Code Decoded: Text/Data -> %s]", cleaned)
}

