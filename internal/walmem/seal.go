package walmem

// Seal session credential (x-seal-session header).
//
// Relayer routes that decrypt memories (recall, ask, restore) need a Seal SessionKey:
// an ephemeral Ed25519 key that the delegate key authorises for a few minutes by signing
// a Sui personal message. This mirrors @mysten/seal SessionKey.create + export() and the
// SDK's buildSealSession, so the delegate private key itself never leaves this process.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"
)

const sealSessionTTLMin = 5

type sealSessionCache struct {
	mu        sync.Mutex
	header    string
	expiresAt time.Time
	packageID string
}

// sealSession returns a cached base64 x-seal-session value, minting a new one near expiry.
func (c *Client) sealSession(ctx context.Context) (string, error) {
	c.seal.mu.Lock()
	defer c.seal.mu.Unlock()
	if c.seal.header != "" && time.Now().Before(c.seal.expiresAt) {
		return c.seal.header, nil
	}
	if c.seal.packageID == "" {
		pkg, err := c.fetchPackageID(ctx)
		if err != nil {
			return "", err
		}
		c.seal.packageID = pkg
	}
	now := c.now()
	header, err := buildSealSession(c.privKey, c.seal.packageID, now, rand.Reader)
	if err != nil {
		return "", err
	}
	c.seal.header = header
	// Refresh a minute before Seal would reject it.
	c.seal.expiresAt = now.Add(sealSessionTTLMin*time.Minute - time.Minute)
	return header, nil
}

func (c *Client) fetchPackageID(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.serverURL+"/config", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("walrus memory /config: %w", err)
	}
	defer resp.Body.Close()
	var cfg struct {
		PackageID string `json:"packageId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil || cfg.PackageID == "" {
		return "", fmt.Errorf("walrus memory /config: no packageId (status %d)", resp.StatusCode)
	}
	return cfg.PackageID, nil
}

type randReader interface {
	Read([]byte) (int, error)
}

func buildSealSession(delegate ed25519.PrivateKey, packageID string, now time.Time, rnd randReader) (string, error) {
	_, sessionPriv, err := ed25519.GenerateKey(rnd)
	if err != nil {
		return "", err
	}
	sessionPub := sessionPriv.Public().(ed25519.PublicKey)

	creation := now.UnixMilli()
	msg := fmt.Sprintf("Accessing keys of package %s for %d mins from %s, session key %s",
		packageID, sealSessionTTLMin,
		time.UnixMilli(creation).UTC().Format("2006-01-02 15:04:05")+" UTC",
		base64.StdEncoding.EncodeToString(sessionPub))

	sig, err := signPersonalMessage(delegate, []byte(msg))
	if err != nil {
		return "", err
	}
	sessionSecret, err := suiPrivKeyBech32(sessionPriv.Seed())
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]interface{}{
		"address":                  suiAddress(delegate.Public().(ed25519.PublicKey)),
		"packageId":                packageID,
		"creationTimeMs":           creation,
		"ttlMin":                   sealSessionTTLMin,
		"personalMessageSignature": sig,
		"sessionKey":               sessionSecret,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(payload), nil
}

// suiAddress is blake2b-256(flag || pubkey) for an Ed25519 key (flag 0x00).
func suiAddress(pub ed25519.PublicKey) string {
	h, _ := blake2b.New256(nil)
	h.Write([]byte{0x00})
	h.Write(pub)
	return "0x" + hex.EncodeToString(h.Sum(nil))
}

// signPersonalMessage produces a Sui serialized signature over a PersonalMessage intent:
// base64(0x00 || ed25519(blake2b256([3,0,0] || bcs(vector<u8> msg))) || pubkey).
func signPersonalMessage(priv ed25519.PrivateKey, msg []byte) (string, error) {
	intent := []byte{3, 0, 0}
	bcs := append(uleb128(uint64(len(msg))), msg...)
	h, err := blake2b.New256(nil)
	if err != nil {
		return "", err
	}
	h.Write(intent)
	h.Write(bcs)
	sig := ed25519.Sign(priv, h.Sum(nil))
	out := make([]byte, 0, 1+len(sig)+ed25519.PublicKeySize)
	out = append(out, 0x00)
	out = append(out, sig...)
	out = append(out, priv.Public().(ed25519.PublicKey)...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func uleb128(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// suiPrivKeyBech32 encodes an Ed25519 seed as "suiprivkey1..." (bech32 of flag||seed).
func suiPrivKeyBech32(seed []byte) (string, error) {
	data, err := convertBits(append([]byte{0x00}, seed...), 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32Encode("suiprivkey", data), nil
}

// decodeSuiPrivKey parses "suiprivkey1..." back into an Ed25519 seed.
func decodeSuiPrivKey(s string) ([]byte, error) {
	hrp, data, err := bech32Decode(s)
	if err != nil {
		return nil, err
	}
	if hrp != "suiprivkey" {
		return nil, fmt.Errorf("unexpected bech32 prefix %q", hrp)
	}
	raw, err := convertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}
	if len(raw) != 33 || raw[0] != 0x00 {
		return nil, fmt.Errorf("not an Ed25519 Sui private key")
	}
	return raw[1:], nil
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, byte(c>>5))
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c&31))
	}
	return out
}

func bech32Encode(hrp string, data []byte) string {
	values := append(bech32HRPExpand(hrp), data...)
	mod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ 1
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, d := range data {
		sb.WriteByte(bech32Charset[d])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(mod>>uint(5*(5-i)))&31])
	}
	return sb.String()
}

func bech32Decode(s string) (string, []byte, error) {
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, fmt.Errorf("invalid bech32 string")
	}
	hrp := s[:pos]
	var data []byte
	for _, c := range s[pos+1:] {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return "", nil, fmt.Errorf("invalid bech32 character %q", c)
		}
		data = append(data, byte(idx))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return "", nil, fmt.Errorf("invalid bech32 checksum")
	}
	return hrp, data[:len(data)-6], nil
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	acc, bits := uint32(0), uint(0)
	maxv := uint32(1)<<to - 1
	var out []byte
	for _, v := range data {
		if uint32(v)>>from != 0 {
			return nil, fmt.Errorf("invalid data range")
		}
		acc = acc<<from | uint32(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, fmt.Errorf("invalid padding")
	}
	return out, nil
}
