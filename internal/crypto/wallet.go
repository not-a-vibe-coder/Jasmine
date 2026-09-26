package crypto

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/mr-tron/base58"
)

type ChainInfo struct {
	Name         string
	Symbol       string
	ChainID      int64
	RPCURL       string
	FallbackRPCs []string
	Explorer     string
}

func (c *ChainInfo) GetAllRPCs() []string {
	urls := []string{c.RPCURL}
	for _, u := range c.FallbackRPCs {
		if u != "" && u != c.RPCURL {
			urls = append(urls, u)
		}
	}
	return urls
}

type Balances struct {
	Solana    string  `json:"solana"`
	Base      string  `json:"base"`
	Ethereum  string  `json:"ethereum"`
	Arbitrum  string  `json:"arbitrum"`
	BNB       string  `json:"bnb"`
	Robinhood string  `json:"robinhood"`
	SolanaVal float64 `json:"solana_val"`
	BaseVal   float64 `json:"base_val"`
	EthVal    float64 `json:"eth_val"`
	ArbVal    float64 `json:"arb_val"`
	BnbVal    float64 `json:"bnb_val"`
	RhVal     float64 `json:"rh_val"`
}

type Service struct {
	svmPubKey  string
	svmPrivKey []byte // 64 bytes ed25519

	evmPubKey  string
	evmPrivKey *ecdsa.PrivateKey

	svmRPCURL      string
	svmFallbackRPC string

	chains map[string]ChainInfo
	client *http.Client
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func NewService(
	svmPub, svmPriv, svmRPC, svmFallback string,
	evmPub, evmPriv string,
	baseRPC, ethRPC, arbRPC, bnbRPC, rhRPC string,
) (*Service, error) {
	s := &Service{
		svmPubKey:      svmPub,
		svmRPCURL:      svmRPC,
		svmFallbackRPC: svmFallback,
		evmPubKey:      evmPub,
		client:         &http.Client{Timeout: 12 * time.Second},
		chains:         make(map[string]ChainInfo),
	}

	// 1. Decode SVM Private Key (base58)
	if svmPriv != "" {
		dec, err := base58.Decode(svmPriv)
		if err == nil && len(dec) == 64 {
			s.svmPrivKey = dec
			if s.svmPubKey == "" {
				s.svmPubKey = base58.Encode(dec[32:])
			}
		}
	}

	// 2. Decode EVM Private Key (hex)
	if evmPriv != "" {
		cleanHex := strings.TrimPrefix(evmPriv, "0x")
		pk, err := gethcrypto.HexToECDSA(cleanHex)
		if err == nil {
			s.evmPrivKey = pk
			if s.evmPubKey == "" {
				pubECDSA, ok := pk.Public().(*ecdsa.PublicKey)
				if ok {
					s.evmPubKey = gethcrypto.PubkeyToAddress(*pubECDSA).Hex()
				}
			}
		}
	}

	// Configure EVM Chains with public fallbacks
	if baseRPC == "" {
		baseRPC = "https://mainnet.base.org"
	}
	if ethRPC == "" {
		ethRPC = "https://eth.llamarpc.com"
	}
	if arbRPC == "" {
		arbRPC = "https://arb1.arbitrum.io/rpc"
	}
	if bnbRPC == "" {
		bnbRPC = "https://binance.llamarpc.com"
	}
	if rhRPC == "" {
		rhRPC = "https://rpc.robinhood.com"
	}

	s.chains["base"] = ChainInfo{
		Name:         "Base",
		Symbol:       "ETH",
		ChainID:      8453,
		RPCURL:       baseRPC,
		FallbackRPCs: []string{"https://base.llamarpc.com", "https://1rpc.io/base", "https://mainnet.base.org"},
		Explorer:     "https://basescan.org/tx/",
	}
	s.chains["ethereum"] = ChainInfo{
		Name:         "Ethereum",
		Symbol:       "ETH",
		ChainID:      1,
		RPCURL:       ethRPC,
		FallbackRPCs: []string{"https://rpc.ankr.com/eth", "https://cloudflare-eth.com", "https://eth.llamarpc.com"},
		Explorer:     "https://etherscan.io/tx/",
	}
	s.chains["arbitrum"] = ChainInfo{
		Name:         "Arbitrum",
		Symbol:       "ETH",
		ChainID:      42161,
		RPCURL:       arbRPC,
		FallbackRPCs: []string{"https://arbitrum.llamarpc.com", "https://rpc.ankr.com/arbitrum", "https://arb1.arbitrum.io/rpc"},
		Explorer:     "https://arbiscan.io/tx/",
	}
	s.chains["bnb"] = ChainInfo{
		Name:         "BNB Smart Chain",
		Symbol:       "BNB",
		ChainID:      56,
		RPCURL:       bnbRPC,
		FallbackRPCs: []string{"https://bsc-dataseed.binance.org", "https://bsc-dataseed1.defibit.io", "https://binance.llamarpc.com"},
		Explorer:     "https://bscscan.com/tx/",
	}

	rhChain := ChainInfo{
		Name:         "Robinhood Chain",
		Symbol:       "ETH",
		ChainID:      4663,
		RPCURL:       rhRPC,
		FallbackRPCs: []string{"https://rpc.robinhood.com"},
		Explorer:     "https://robinhoodchain.blockscout.com/tx/",
	}
	s.chains["robinhood"] = rhChain
	s.chains["rh"] = rhChain

	return s, nil
}

func (s *Service) GetAddresses() (svm string, evm string) {
	return s.svmPubKey, s.evmPubKey
}

// FormatTokenAmount formats crypto token balances cleanly without truncating small values (e.g. 0.00011159 ETH).
func FormatTokenAmount(val *big.Float) string {
	if val == nil {
		return "0.0000"
	}
	f, _ := val.Float64()
	if f == 0 {
		return "0.0000"
	}
	// Up to 8 decimal places
	s := val.Text('f', 8)
	if strings.Contains(s, ".") {
		parts := strings.Split(s, ".")
		dec := strings.TrimRight(parts[1], "0")
		for len(dec) < 4 {
			dec += "0"
		}
		return parts[0] + "." + dec
	}
	return s + ".0000"
}

func (s *Service) GetAllBalances(ctx context.Context) (*Balances, error) {
	b := &Balances{}

	// Solana
	solVal, err := s.GetSVMBalance(ctx)
	if err != nil {
		b.Solana = "Unavailable"
	} else {
		b.Solana = fmt.Sprintf("%s SOL", FormatTokenAmount(solVal))
		b.SolanaVal, _ = solVal.Float64()
	}

	// Base
	baseVal, err := s.GetEVMBalance(ctx, "base")
	if err != nil {
		b.Base = "Unavailable"
	} else {
		b.Base = fmt.Sprintf("%s ETH", FormatTokenAmount(baseVal))
		b.BaseVal, _ = baseVal.Float64()
	}

	// Ethereum
	ethVal, err := s.GetEVMBalance(ctx, "ethereum")
	if err != nil {
		b.Ethereum = "Unavailable"
	} else {
		b.Ethereum = fmt.Sprintf("%s ETH", FormatTokenAmount(ethVal))
		b.EthVal, _ = ethVal.Float64()
	}

	// Arbitrum
	arbVal, err := s.GetEVMBalance(ctx, "arbitrum")
	if err != nil {
		b.Arbitrum = "Unavailable"
	} else {
		b.Arbitrum = fmt.Sprintf("%s ETH", FormatTokenAmount(arbVal))
		b.ArbVal, _ = arbVal.Float64()
	}

	// BNB
	bnbVal, err := s.GetEVMBalance(ctx, "bnb")
	if err != nil {
		b.BNB = "Unavailable"
	} else {
		b.BNB = fmt.Sprintf("%s BNB", FormatTokenAmount(bnbVal))
		b.BnbVal, _ = bnbVal.Float64()
	}

	// Robinhood
	rhVal, err := s.GetEVMBalance(ctx, "robinhood")
	if err != nil {
		b.Robinhood = "Unavailable"
	} else {
		b.Robinhood = fmt.Sprintf("%s ETH", FormatTokenAmount(rhVal))
		b.RhVal, _ = rhVal.Float64()
	}

	return b, nil
}

func (s *Service) GetSVMBalance(ctx context.Context) (*big.Float, error) {
	if s.svmPubKey == "" {
		return nil, fmt.Errorf("solana public key not configured")
	}

	urls := []string{s.svmRPCURL, s.svmFallbackRPC, "https://api.mainnet-beta.solana.com"}
	var lastErr error

	for _, u := range urls {
		if u == "" {
			continue
		}
		val, err := s.fetchSVMBalance(ctx, u, s.svmPubKey)
		if err == nil {
			return val, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("solana balance fetch failed: %v", lastErr)
}

func (s *Service) fetchSVMBalance(ctx context.Context, rpcURL, address string) (*big.Float, error) {
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "getBalance",
		Params:  []interface{}{address},
		ID:      1,
	}
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc status %d", resp.StatusCode)
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	var res struct {
		Value uint64 `json:"value"`
	}
	if err := json.Unmarshal(rpcResp.Result, &res); err != nil {
		return nil, err
	}

	sol := new(big.Float).Quo(new(big.Float).SetUint64(res.Value), big.NewFloat(1e9))
	return sol, nil
}

func (s *Service) GetEVMBalance(ctx context.Context, chainName string) (*big.Float, error) {
	chain, exists := s.chains[strings.ToLower(chainName)]
	if !exists {
		return nil, fmt.Errorf("unknown EVM chain: %s", chainName)
	}

	urls := chain.GetAllRPCs()
	if len(urls) == 0 {
		return nil, fmt.Errorf("RPC URL not configured for %s", chainName)
	}

	var lastErr error
	for _, rpcURL := range urls {
		if rpcURL == "" {
			continue
		}
		val, err := s.fetchEVMBalance(ctx, rpcURL, s.evmPubKey)
		if err == nil {
			return val, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("%s balance fetch failed: %v", chainName, lastErr)
}

func (s *Service) fetchEVMBalance(ctx context.Context, rpcURL, address string) (*big.Float, error) {

	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_getBalance",
		Params:  []interface{}{address, "latest"},
		ID:      1,
	}
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	var hexStr string
	if err := json.Unmarshal(rpcResp.Result, &hexStr); err != nil {
		return nil, err
	}

	hexClean := strings.TrimPrefix(hexStr, "0x")
	val := new(big.Int)
	val.SetString(hexClean, 16)

	fval := new(big.Float).SetInt(val)
	eth := new(big.Float).Quo(fval, big.NewFloat(1e18))
	return eth, nil
}

func (s *Service) SendEVM(ctx context.Context, chainName, toAddress string, amountETH float64) (string, string, error) {
	if s.evmPrivKey == nil {
		return "", "", fmt.Errorf("EVM private key not configured")
	}

	chainKey := strings.ToLower(strings.TrimSpace(chainName))
	if chainKey == "eth" || chainKey == "mainnet" {
		chainKey = "ethereum"
	} else if chainKey == "bsc" {
		chainKey = "bnb"
	}

	chain, exists := s.chains[chainKey]
	if !exists {
		return "", "", fmt.Errorf("unsupported chain '%s' (supported: base, ethereum, arbitrum, bnb)", chainName)
	}

	if !gethcommon.IsHexAddress(toAddress) {
		return "", "", fmt.Errorf("invalid EVM recipient address: %s", toAddress)
	}
	toAddr := gethcommon.HexToAddress(toAddress)

	// Fetch Nonce
	nonce, err := s.getEVMNonce(ctx, chain.RPCURL, s.evmPubKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to get nonce: %w", err)
	}

	// Fetch Gas Price
	gasPrice, err := s.getEVMGasPrice(ctx, chain.RPCURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to get gas price: %w", err)
	}

	// Calculate Wei amount
	fAmount := big.NewFloat(amountETH)
	fWei := new(big.Float).Mul(fAmount, big.NewFloat(1e18))
	weiAmount := new(big.Int)
	fWei.Int(weiAmount)

	gasLimit := uint64(21000)
	tx := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    nonce,
		GasPrice: gasPrice,
		Gas:      gasLimit,
		To:       &toAddr,
		Value:    weiAmount,
		Data:     nil,
	})

	signer := gethtypes.NewEIP155Signer(big.NewInt(chain.ChainID))
	signedTx, err := gethtypes.SignTx(tx, signer, s.evmPrivKey)
	if err != nil {
		return "", "", fmt.Errorf("signing error: %w", err)
	}

	rawBytes, err := signedTx.MarshalBinary()
	if err != nil {
		return "", "", fmt.Errorf("marshal binary error: %w", err)
	}
	rawHex := "0x" + hex.EncodeToString(rawBytes)

	// Broadcast transaction
	txHash, err := s.broadcastEVMTx(ctx, chain.RPCURL, rawHex)
	if err != nil {
		return "", "", fmt.Errorf("broadcast error: %w", err)
	}

	explorerLink := chain.Explorer + txHash
	return txHash, explorerLink, nil
}

func (s *Service) getEVMNonce(ctx context.Context, rpcURL, address string) (uint64, error) {
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_getTransactionCount",
		Params:  []interface{}{address, "pending"},
		ID:      1,
	}
	data, _ := json.Marshal(reqBody)
	respData, err := s.postRPC(ctx, rpcURL, data)
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(respData, &hexStr); err != nil {
		return 0, err
	}
	clean := strings.TrimPrefix(hexStr, "0x")
	nonce, err := strconv.ParseUint(clean, 16, 64)
	if err != nil {
		return 0, err
	}
	return nonce, nil
}

func (s *Service) getEVMGasPrice(ctx context.Context, rpcURL string) (*big.Int, error) {
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_gasPrice",
		Params:  []interface{}{},
		ID:      1,
	}
	data, _ := json.Marshal(reqBody)
	respData, err := s.postRPC(ctx, rpcURL, data)
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(respData, &hexStr); err != nil {
		return nil, err
	}
	clean := strings.TrimPrefix(hexStr, "0x")
	price := new(big.Int)
	price.SetString(clean, 16)
	return price, nil
}

func (s *Service) broadcastEVMTx(ctx context.Context, rpcURL, rawHex string) (string, error) {
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_sendRawTransaction",
		Params:  []interface{}{rawHex},
		ID:      1,
	}
	data, _ := json.Marshal(reqBody)
	respData, err := s.postRPC(ctx, rpcURL, data)
	if err != nil {
		return "", err
	}
	var txHash string
	if err := json.Unmarshal(respData, &txHash); err != nil {
		return "", err
	}
	return txHash, nil
}

func (s *Service) postRPC(ctx context.Context, rpcURL string, data []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func (s *Service) SendSVM(ctx context.Context, toAddress string, amountSOL float64) (string, string, error) {
	if len(s.svmPrivKey) != 64 {
		return "", "", fmt.Errorf("Solana private key not configured")
	}

	toPubBytes, err := base58.Decode(toAddress)
	if err != nil || len(toPubBytes) != 32 {
		return "", "", fmt.Errorf("invalid Solana recipient address: %s", toAddress)
	}

	urls := []string{s.svmRPCURL, s.svmFallbackRPC, "https://api.mainnet-beta.solana.com"}
	var txHash string
	var lastErr error

	for _, u := range urls {
		if u == "" {
			continue
		}
		hash, err := s.executeSVMTransfer(ctx, u, toPubBytes, amountSOL)
		if err == nil {
			txHash = hash
			break
		}
		lastErr = err
	}

	if txHash == "" {
		return "", "", fmt.Errorf("failed to send SOL: %v", lastErr)
	}

	explorerLink := fmt.Sprintf("https://solscan.io/tx/%s", txHash)
	return txHash, explorerLink, nil
}

func (s *Service) executeSVMTransfer(ctx context.Context, rpcURL string, toPub []byte, amountSOL float64) (string, error) {
	// 1. Get recent blockhash
	blockhash, err := s.getSVMLatestBlockhash(ctx, rpcURL)
	if err != nil {
		return "", err
	}

	blockhashBytes, err := base58.Decode(blockhash)
	if err != nil || len(blockhashBytes) != 32 {
		return "", fmt.Errorf("invalid blockhash format")
	}

	fromPub := s.svmPrivKey[32:] // last 32 bytes of ed25519 keypair
	lamports := uint64(amountSOL * 1e9)

	// Solana System Program ID: 11111111111111111111111111111111 (32 zeroes)
	systemProgramID := make([]byte, 32)

	// Instruction data: index 2 (Transfer) as uint32 le + lamports as uint64 le
	instData := make([]byte, 12)
	binary.LittleEndian.PutUint32(instData[0:4], 2)
	binary.LittleEndian.PutUint64(instData[4:12], lamports)

	// Build raw Solana transaction wire format:
	// Message format:
	// Header: 1 req signers, 0 non-voting signers, 1 non-voting readonly
	// Account keys: [fromPub (signer, writable), toPub (writable), systemProgram (readonly)]
	// Recent blockhash (32 bytes)
	// Instructions: [program_id_index=2, accounts=[0, 1], data=instData]

	var msgBuf bytes.Buffer
	// Header: numRequiredSignatures=1, numReadonlySigned=0, numReadonlyUnsigned=1
	msgBuf.WriteByte(1)
	msgBuf.WriteByte(0)
	msgBuf.WriteByte(1)

	// Accounts compact-u16 length = 3
	encodeCompactU16(&msgBuf, 3)
	msgBuf.Write(fromPub)
	msgBuf.Write(toPub)
	msgBuf.Write(systemProgramID)

	// Recent blockhash
	msgBuf.Write(blockhashBytes)

	// Instructions compact-u16 count = 1
	encodeCompactU16(&msgBuf, 1)
	// Program ID index = 2 (system program)
	msgBuf.WriteByte(2)
	// Account indices count = 2 [0, 1]
	encodeCompactU16(&msgBuf, 2)
	msgBuf.WriteByte(0) // fromPub
	msgBuf.WriteByte(1) // toPub
	// Data length + data
	encodeCompactU16(&msgBuf, uint16(len(instData)))
	msgBuf.Write(instData)

	msgBytes := msgBuf.Bytes()

	// Sign message with ed25519
	privKey := ed25519.PrivateKey(s.svmPrivKey)
	signature := ed25519.Sign(privKey, msgBytes)

	// Full wire transaction:
	// Signatures compact-u16 count = 1
	// Signature (64 bytes)
	// Message bytes
	var txBuf bytes.Buffer
	encodeCompactU16(&txBuf, 1)
	txBuf.Write(signature)
	txBuf.Write(msgBytes)

	txEncoded := base58.Encode(txBuf.Bytes())

	// Send transaction via RPC
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "sendTransaction",
		Params: []interface{}{
			txEncoded,
			map[string]interface{}{
				"encoding":            "base58",
				"skipPreflight":       false,
				"preflightCommitment": "confirmed",
			},
		},
		ID: 1,
	}
	body, _ := json.Marshal(reqBody)
	respData, err := s.postRPC(ctx, rpcURL, body)
	if err != nil {
		return "", err
	}

	var sig string
	if err := json.Unmarshal(respData, &sig); err != nil {
		return "", err
	}
	return sig, nil
}

func (s *Service) getSVMLatestBlockhash(ctx context.Context, rpcURL string) (string, error) {
	reqBody := rpcRequest{
		JSONRPC: "2.0",
		Method:  "getLatestBlockhash",
		Params: []interface{}{
			map[string]string{"commitment": "confirmed"},
		},
		ID: 1,
	}
	data, _ := json.Marshal(reqBody)
	respData, err := s.postRPC(ctx, rpcURL, data)
	if err != nil {
		return "", err
	}

	var res struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := json.Unmarshal(respData, &res); err != nil {
		return "", err
	}
	return res.Value.Blockhash, nil
}

func encodeCompactU16(buf *bytes.Buffer, val uint16) {
	for {
		elem := uint8(val & 0x7f)
		val >>= 7
		if val == 0 {
			buf.WriteByte(elem)
			break
		} else {
			elem |= 0x80
			buf.WriteByte(elem)
		}
	}
}
