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
