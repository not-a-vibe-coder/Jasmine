package crypto

import (
	"bytes"
	"context"
	"testing"
)

func TestCryptoServiceInitAndValidation(t *testing.T) {
	// Sample keys
	svmPriv := "mBTq9bbtut9vVSbwRRhAKkgiQdKMi8GNxRQ7Qq4UvbDXfauVMB9Rs9F4xTe3vh9M1pQzYHTh7BeeqzzYb63Xfvk"
	evmPriv := "0x8fb64349cbb7feb1506006287c373a9cd0cf06120a87e90ad40492958c145d59"

	svc, err := NewService(
		"", svmPriv, "", "",
		"", evmPriv,
		"https://base.rpc", "https://eth.rpc", "https://arb.rpc", "https://bnb.rpc",
	)
	if err != nil {
		t.Fatalf("unexpected error creating crypto service: %v", err)
	}

	svmAddr, evmAddr := svc.GetAddresses()
	if svmAddr != "HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr" {
		t.Errorf("expected SVM address HzxdDjSZPw9JCbrknZ3dUru5SwnuTQrJFqZN7gPKHfXr, got %s", svmAddr)
	}
	if evmAddr != "0x0a2e799d0b57217a1066a4CDD132F01215E132b8" {
		t.Errorf("expected EVM address 0x0a2e799d0b57217a1066a4CDD132F01215E132b8, got %s", evmAddr)
	}

	ctx := context.Background()

	// Test invalid EVM address
	_, _, err = svc.SendEVM(ctx, "base", "not-a-valid-address", 0.01)
	if err == nil {
		t.Errorf("expected error for invalid EVM recipient, got nil")
	}

	// Test unsupported chain
	_, _, err = svc.SendEVM(ctx, "dogechain", "0x0a2e799d0b57217a1066a4CDD132F01215E132b8", 0.01)
	if err == nil {
		t.Errorf("expected error for unsupported chain, got nil")
	}

	// Test invalid Solana address
	_, _, err = svc.SendSVM(ctx, "invalid-base58!", 0.01)
	if err == nil {
		t.Errorf("expected error for invalid Solana recipient, got nil")
	}
}

func TestEncodeCompactU16(t *testing.T) {
	tests := []struct {
		val      uint16
		expected []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
	}

	for _, tt := range tests {
		var buf bytes.Buffer
		encodeCompactU16(&buf, tt.val)
		if !bytes.Equal(buf.Bytes(), tt.expected) {
			t.Errorf("encodeCompactU16(%d) = %v; want %v", tt.val, buf.Bytes(), tt.expected)
		}
	}
}
