package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	geth "github.com/ethereum/go-ethereum/crypto"
)

// redeemMu prevents concurrent redemption runs (auto-loop + manual button).
var redeemMu sync.Mutex

// redeemClient has a hard timeout so a hung RPC never blocks the goroutine.
var redeemClient = &http.Client{Timeout: 30 * time.Second}

const (
	polygonRPC = "https://polygon-bor-rpc.publicnode.com"

	// ConditionalTokens contract (holds positions).
	ctfContractAddr = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"
	// NegRisk adapter for neg-risk markets.
	negRiskAdapterAddr = "0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296"
	// USDC.e (collateral token on Polygon).
	usdceAddr = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"
)

// Pre-computed function selectors.
var (
	// keccak256("redeemPositions(address,bytes32,bytes32,uint256[])")[:4]
	redeemSelector []byte
	// keccak256("redeemPositions(bytes32,uint256[])")[:4]
	negRiskRedeemSelector []byte
)

func init() {
	redeemSelector = geth.Keccak256([]byte("redeemPositions(address,bytes32,bytes32,uint256[])"))[:4]
	negRiskRedeemSelector = geth.Keccak256([]byte("redeemPositions(bytes32,uint256[])"))[:4]
}

// RedeemConfig holds credentials needed for redemption.
type RedeemConfig struct {
	PrivateKey *ecdsa.PrivateKey
	SignerAddr string // EOA address (also holds positions directly)
}

// RedeemResult describes one redemption attempt.
type RedeemResult struct {
	ConditionID string  `json:"conditionId"`
	Title       string  `json:"title"`
	Shares      float64 `json:"shares"`
	Success     bool    `json:"success"`
	TxHash      string  `json:"txHash,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// RedeemablePosition is a position from the data API that can be redeemed.
type RedeemablePosition struct {
	ConditionID  string  `json:"conditionId"`
	Title        string  `json:"title"`
	Size         float64 `json:"size"`
	NegativeRisk *bool   `json:"negativeRisk"`
	OutcomeIndex int     `json:"outcomeIndex"`
	Redeemable   bool    `json:"redeemable"`
	CurPrice     float64 `json:"curPrice"`
	CurrentValue float64 `json:"currentValue"`
	ProxyWallet  string  `json:"proxyWallet"`
}

// fetchRedeemablePositions pages through ALL redeemable positions (100 per page).
func fetchRedeemablePositions(address string) ([]RedeemablePosition, error) {
	const pageSize = 100
	var all []RedeemablePosition
	for offset := 0; ; offset += pageSize {
		u := fmt.Sprintf("https://data-api.polymarket.com/positions?user=%s&redeemable=true&sizeThreshold=0&limit=%d&offset=%d", address, pageSize, offset)
		resp, err := redeemClient.Get(u)
		if err != nil {
			return nil, fmt.Errorf("fetch positions: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var page []RedeemablePosition
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse positions: %w (body: %s)", err, string(body[:min(len(body), 200)]))
		}
		for _, p := range page {
			if p.Size > 0 {
				all = append(all, p)
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	return all, nil
}

// checkResolution queries CLOB /markets/{conditionId} to check which outcome won.
// Returns the winning outcome index (0 or 1) and true if resolved.
func checkResolution(conditionID string) (winnerIndex int, resolved bool) {
	url := fmt.Sprintf("https://clob.polymarket.com/markets/%s", conditionID)
	resp, err := redeemClient.Get(url)
	if err != nil {
		return -1, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return -1, false
	}

	var result struct {
		Closed bool `json:"closed"`
		Tokens []struct {
			Outcome string `json:"outcome"`
			Winner  bool   `json:"winner"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return -1, false
	}
	if !result.Closed {
		return -1, false
	}
	for i, tok := range result.Tokens {
		if tok.Winner {
			return i, true
		}
	}
	return -1, false
}

// buildRedeemCalldata constructs the ABI-encoded calldata for redeemPositions.
func buildRedeemCalldata(conditionID string, isNegRisk bool, outcomeIndex int, size float64) ([]byte, error) {
	condIDHex := strings.TrimPrefix(conditionID, "0x")
	condBytes, err := hex.DecodeString(condIDHex)
	if err != nil {
		return nil, fmt.Errorf("decode conditionId: %w", err)
	}
	if len(condBytes) != 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(condBytes):], condBytes)
		condBytes = padded
	}

	if isNegRisk {
		// redeemPositions(bytes32 conditionId, uint256[] amounts)
		sizeRaw := new(big.Int).SetInt64(int64(size * 1e6))
		var data []byte
		data = append(data, negRiskRedeemSelector...)
		data = append(data, condBytes...)
		data = append(data, padUint256(big.NewInt(64))...)
		data = append(data, padUint256(big.NewInt(2))...)
		if outcomeIndex == 0 {
			data = append(data, padUint256(sizeRaw)...)
			data = append(data, padUint256(big.NewInt(0))...)
		} else {
			data = append(data, padUint256(big.NewInt(0))...)
			data = append(data, padUint256(sizeRaw)...)
		}
		return data, nil
	}

	// Standard: redeemPositions(address collateralToken, bytes32 parentCollectionId, bytes32 conditionId, uint256[] indexSets)
	var data []byte
	data = append(data, redeemSelector...)
	data = append(data, padAddress(usdceAddr)...)
	data = append(data, make([]byte, 32)...) // parentCollectionId = 0x00...00
	data = append(data, condBytes...)
	data = append(data, padUint256(big.NewInt(128))...) // offset to dynamic array
	data = append(data, padUint256(big.NewInt(2))...)   // array length
	data = append(data, padUint256(big.NewInt(1))...)   // indexSets[0] = 1
	data = append(data, padUint256(big.NewInt(2))...)   // indexSets[1] = 2
	return data, nil
}

// sendRawTransaction signs and submits a raw transaction directly to Polygon.
// nonce must be obtained externally and incremented by the caller after each call.
func sendRawTransaction(ctx context.Context, privKey *ecdsa.PrivateKey, from, to string, calldata []byte, nonce int64) (string, error) {
	// 1. Get gas price.
	gasPrice, err := getGasPrice()
	if err != nil {
		return "", fmt.Errorf("get gas price: %w", err)
	}

	// 3. Estimate gas.
	gasLimit, err := estimateGas(from, to, calldata)
	if err != nil {
		// Fallback to a generous gas limit if estimation fails.
		gasLimit = 300000
		fmt.Printf("[redeem] Gas estimation failed (%v), using fallback %d\n", err, gasLimit)
	}

	// 4. Build and sign the transaction (EIP-155 for Polygon, chainId=137).
	chainID := big.NewInt(137)
	toAddr := common.HexToAddress(to)
	value := big.NewInt(0)

	// EIP-155 transaction encoding: [nonce, gasPrice, gasLimit, to, value, data, chainId, 0, 0]
	txFields := []interface{}{
		uint64(nonce),
		gasPrice,
		uint64(gasLimit),
		toAddr.Bytes(),
		value,
		calldata,
		chainID,
		big.NewInt(0),
		big.NewInt(0),
	}
	encoded := rlpEncode(txFields)
	txHash := geth.Keccak256(encoded)

	sig, err := geth.Sign(txHash, privKey)
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	// EIP-155: v = chainId * 2 + 35 + recovery_id
	v := new(big.Int).SetInt64(int64(sig[64]))
	v.Add(v, new(big.Int).Mul(chainID, big.NewInt(2)))
	v.Add(v, big.NewInt(35))

	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:64])

	// Encode signed transaction: [nonce, gasPrice, gasLimit, to, value, data, v, r, s]
	signedFields := []interface{}{
		uint64(nonce),
		gasPrice,
		uint64(gasLimit),
		toAddr.Bytes(),
		value,
		calldata,
		v,
		r,
		s,
	}
	signedEncoded := rlpEncode(signedFields)
	rawTxHex := "0x" + hex.EncodeToString(signedEncoded)

	// 5. Submit via eth_sendRawTransaction.
	rpcBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["%s"],"id":1}`, rawTxHex)
	resp, err := redeemClient.Post(polygonRPC, "application/json", strings.NewReader(rpcBody))
	if err != nil {
		return "", fmt.Errorf("send raw tx: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var rpcResp struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(body, &rpcResp)
	if rpcResp.Error != nil {
		return "", fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func getEOANonce(addr string) (int64, error) {
	rpcBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getTransactionCount","params":["%s","pending"],"id":1}`, addr)
	resp, err := redeemClient.Post(polygonRPC, "application/json", strings.NewReader(rpcBody))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct{ Result string `json:"result"` }
	json.Unmarshal(body, &r)
	n := new(big.Int)
	n.SetString(strings.TrimPrefix(r.Result, "0x"), 16)
	return n.Int64(), nil
}

func getGasPrice() (*big.Int, error) {
	resp, err := redeemClient.Post(polygonRPC, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_gasPrice","params":[],"id":1}`))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct{ Result string `json:"result"` }
	json.Unmarshal(body, &r)
	price := new(big.Int)
	price.SetString(strings.TrimPrefix(r.Result, "0x"), 16)
	// Add 20% buffer for faster confirmation.
	price.Mul(price, big.NewInt(120))
	price.Div(price, big.NewInt(100))
	return price, nil
}

func estimateGas(from, to string, data []byte) (int64, error) {
	dataHex := "0x" + hex.EncodeToString(data)
	rpcBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_estimateGas","params":[{"from":"%s","to":"%s","data":"%s"}],"id":1}`, from, to, dataHex)
	resp, err := redeemClient.Post(polygonRPC, "application/json", strings.NewReader(rpcBody))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result string `json:"result"`
		Error  *struct{ Message string `json:"message"` } `json:"error"`
	}
	json.Unmarshal(body, &r)
	if r.Error != nil {
		return 0, fmt.Errorf("%s", r.Error.Message)
	}
	gas := new(big.Int)
	gas.SetString(strings.TrimPrefix(r.Result, "0x"), 16)
	// Add 30% buffer.
	gas.Mul(gas, big.NewInt(130))
	gas.Div(gas, big.NewInt(100))
	return gas.Int64(), nil
}

// redeemAll finds and redeems only WINNING redeemable positions via direct on-chain transactions.
func redeemAll(cfg RedeemConfig) ([]RedeemResult, error) {
	// 1. Fetch redeemable positions.
	positions, err := fetchRedeemablePositions(cfg.SignerAddr)
	if err != nil {
		return nil, fmt.Errorf("fetch redeemable: %w", err)
	}
	if len(positions) == 0 {
		return nil, nil
	}

	// 2. Filter to only winning positions by checking market resolution.
	var winningPositions []RedeemablePosition
	for _, pos := range positions {
		winnerIdx, resolved := checkResolution(pos.ConditionID)
		if !resolved {
			continue
		}
		if winnerIdx == pos.OutcomeIndex {
			winningPositions = append(winningPositions, pos)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(winningPositions) == 0 {
		return nil, nil
	}

	fmt.Printf("[redeem] Found %d winning positions to redeem\n", len(winningPositions))

	// Get nonce once; we track it manually to avoid duplicate-nonce errors
	// when multiple txs are sent faster than Polygon confirms them.
	nonce, err := getEOANonce(cfg.SignerAddr)
	if err != nil {
		return nil, fmt.Errorf("get nonce: %w", err)
	}

	ctx := context.Background()
	var results []RedeemResult

	for _, pos := range winningPositions {
		r := RedeemResult{
			ConditionID: pos.ConditionID,
			Title:       pos.Title,
			Shares:      pos.Size,
		}

		isNegRisk := pos.NegativeRisk != nil && *pos.NegativeRisk

		// 3. Build calldata.
		calldata, err := buildRedeemCalldata(pos.ConditionID, isNegRisk, pos.OutcomeIndex, pos.Size)
		if err != nil {
			r.Error = fmt.Sprintf("build calldata: %v", err)
			results = append(results, r)
			continue
		}

		targetContract := ctfContractAddr
		if isNegRisk {
			targetContract = negRiskAdapterAddr
		}

		// 4. Submit direct on-chain transaction.
		fmt.Printf("[redeem] Submitting tx for %s (%s, %.2f shares) nonce=%d\n", pos.Title, pos.ConditionID[:12], pos.Size, nonce)
		txHash, err := sendRawTransaction(ctx, cfg.PrivateKey, cfg.SignerAddr, targetContract, calldata, nonce)
		// Always advance the nonce so the next tx doesn't collide.
		nonce++
		if err != nil {
			r.Error = fmt.Sprintf("tx: %v", err)
			results = append(results, r)
			continue
		}

		r.Success = true
		r.TxHash = txHash
		results = append(results, r)
		fmt.Printf("[redeem] TX sent: %s for %s\n", txHash, pos.Title)

		// Brief pause to avoid flooding the RPC.
		time.Sleep(2 * time.Second)
	}

	return results, nil
}

// RLP encoding for Ethereum transactions.

func rlpEncode(items []interface{}) []byte {
	var encoded []byte
	for _, item := range items {
		encoded = append(encoded, rlpEncodeItem(item)...)
	}
	return rlpEncodeLength(encoded, 0xc0)
}

func rlpEncodeItem(item interface{}) []byte {
	switch v := item.(type) {
	case uint64:
		if v == 0 {
			return []byte{0x80}
		}
		return rlpEncodeBigInt(new(big.Int).SetUint64(v))
	case *big.Int:
		return rlpEncodeBigInt(v)
	case []byte:
		if len(v) == 0 {
			return []byte{0x80}
		}
		if len(v) == 1 && v[0] < 0x80 {
			return v
		}
		return rlpEncodeLength(v, 0x80)
	default:
		return []byte{0x80}
	}
}

func rlpEncodeBigInt(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return []byte{0x80}
	}
	b := v.Bytes()
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return rlpEncodeLength(b, 0x80)
}

func rlpEncodeLength(data []byte, offset byte) []byte {
	if len(data) < 56 {
		return append([]byte{offset + byte(len(data))}, data...)
	}
	lenBytes := bigIntBytes(int64(len(data)))
	return append(append([]byte{offset + 55 + byte(len(lenBytes))}, lenBytes...), data...)
}

func bigIntBytes(v int64) []byte {
	b := new(big.Int).SetInt64(v).Bytes()
	if len(b) == 0 {
		return []byte{0}
	}
	return b
}

// Helpers for ABI encoding.

func padUint256(v *big.Int) []byte {
	b := make([]byte, 32)
	if v != nil {
		vBytes := v.Bytes()
		copy(b[32-len(vBytes):], vBytes)
	}
	return b
}

func padAddress(addr string) []byte {
	b := make([]byte, 32)
	addrBytes := common.HexToAddress(addr).Bytes()
	copy(b[32-len(addrBytes):], addrBytes)
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
