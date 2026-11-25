package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const (
	aiRequestSubnet  = "0300000000000000000000000000000000000000"
	aiResponseSubnet = "0400000000000000000000000000000000000000"
	vaultScript      = "00006a0761697661756c74"
	minimumFee       = 30_000_000
)

var envelopeMagic = []byte{0, 'K', 'X', 'P', 1}

type model struct {
	Tier           int    `json:"tier"`
	Name           string `json:"name"`
	ID             string `json:"model_id"`
	Reward         uint64 `json:"reward_sompi"`
	Fee            uint64 `json:"fee_sompi"`
	Cost           uint64 `json:"cost_sompi"`
	EligibleMiners int    `json:"eligible_miners"`
}

// Model ordering and rewards come from service_tiers(true) and INFERENCE_REWARD_MINIMUMS_FLAT.
var models = []model{
	{Tier: 0, Name: "Qwen3.5-9B", ID: "bd34568cd89f5f19c6c3a6e1a61b929bc868709409eaad8e672d85f3c1eb5710", Reward: 50_000_000},
	{Tier: 1, Name: "GLM-4-9B", ID: "fa2f13be0850e26c5ce86c7ac79da85e300c1da8b3290f9a18d47105f1f2140a", Reward: 100_000_000},
	{Tier: 2, Name: "Gemma-4-12B", ID: "399984045600f7d58d1b2cf01e6a4bf466fa15c7ac31bd0dd1a71e003b617cc6", Reward: 150_000_000},
	{Tier: 3, Name: "Qwen3.8-27B", ID: "73740b443bdc00afda5fa34eb9999d3fea77dcc3f6de238fab701394cdc96fb3", Reward: 200_000_000},
	{Tier: 4, Name: "Kimi-Linear-48B", ID: "3dc09358ad75c6ef0c9c86ee4f47c4d6acda961fecbd0e4f9cf55e8f0fdffddb", Reward: 250_000_000},
}

type outpoint struct {
	TransactionID string `json:"transactionId"`
	Index         uint32 `json:"index"`
}

type txInput struct {
	Outpoint        outpoint `json:"previousOutpoint"`
	SignatureScript string   `json:"signatureScript"`
	Sequence        uint64   `json:"sequence"`
	SigOpCount      uint8    `json:"sigOpCount"`
}

type txOutput struct {
	Value           uint64 `json:"value"`
	ScriptPublicKey string `json:"scriptPublicKey"`
}

type transaction struct {
	Version      uint16     `json:"version"`
	Inputs       []txInput  `json:"inputs"`
	Outputs      []txOutput `json:"outputs"`
	LockTime     uint64     `json:"lockTime"`
	SubnetworkID string     `json:"subnetworkId"`
	Gas          uint64     `json:"gas"`
	Payload      string     `json:"payload"`
	Mass         uint64     `json:"mass"`
}

func hashText(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil && strings.ToLower(value) == value
}

// This is the TransactionID serialization for a normal signed Keryx transaction.
// Signatures, sigop counts and mass are excluded. The node independently checks the ID and signatures.
func (tx transaction) requestID() (string, error) {
	if tx.SubnetworkID != aiRequestSubnet || tx.Version != 0 || tx.Gas != 0 {
		return "", errors.New("expected a signed Keryx AiRequest (subnet 03)")
	}
	if len(tx.Inputs) < 1 || len(tx.Inputs) > 32 || len(tx.Outputs) != 2 {
		return "", errors.New("expected 1 to 32 signed inputs, one change output and one vault output")
	}
	payload, err := hex.DecodeString(tx.Payload)
	if err != nil || len(payload) > 65536 || len(payload) < 52+51+80+16 {
		return "", errors.New("invalid AiRequest payload")
	}
	fee := binary.LittleEndian.Uint64(payload[44:52])
	reward := binary.LittleEndian.Uint64(payload[36:44])
	tokens := binary.LittleEndian.Uint32(payload[32:36])
	if tokens == 0 || tokens > 4096 || fee < minimumFee ||
		tx.Outputs[0].Value == 0 || tx.Outputs[1].Value != reward ||
		strings.ToLower(tx.Outputs[1].ScriptPublicKey) != vaultScript {
		return "", errors.New("invalid request limits, fee or inference vault")
	}
	modelID := hex.EncodeToString(payload[:32])
	matched := false
	for _, model := range models {
		if model.ID == modelID && reward >= model.Reward {
			matched = true
			break
		}
	}
	if !matched {
		return "", errors.New("unsupported model or reward below the model minimum")
	}
	envelope := payload[52:]
	recipients := int(envelope[50])
	if !bytes.HasPrefix(envelope, envelopeMagic) || (envelope[5] != 2 && envelope[5] != 3) ||
		recipients < 1 || recipients > 128 || len(envelope) < 51+80*recipients+16 {
		return "", errors.New("expected an encrypted AiRequest envelope")
	}
	for index := 1; index < recipients; index++ {
		offset := 51 + 80*index
		if bytes.Compare(envelope[offset-80:offset-48], envelope[offset:offset+32]) >= 0 {
			return "", errors.New("invalid responder order")
		}
	}
	return tx.id(payload)
}

func (tx transaction) id(payload []byte) (string, error) {
	le := binary.LittleEndian
	data := le.AppendUint16(nil, tx.Version)
	data = le.AppendUint64(data, uint64(len(tx.Inputs)))
	seen := make(map[outpoint]struct{}, len(tx.Inputs))
	for _, input := range tx.Inputs {
		if !hashText(input.Outpoint.TransactionID) {
			return "", errors.New("invalid input transaction ID")
		}
		if _, duplicate := seen[input.Outpoint]; duplicate {
			return "", errors.New("duplicate input")
		}
		seen[input.Outpoint] = struct{}{}
		signature, err := hex.DecodeString(input.SignatureScript)
		if err != nil || len(signature) == 0 || len(signature) > 2048 {
			return "", errors.New("expected a signed input")
		}
		data, _ = hex.AppendDecode(data, []byte(input.Outpoint.TransactionID))
		data = le.AppendUint32(data, input.Outpoint.Index)
		data = le.AppendUint64(data, 0)
		data = le.AppendUint64(data, input.Sequence)
	}
	data = le.AppendUint64(data, uint64(len(tx.Outputs)))
	for _, output := range tx.Outputs {
		script, err := hex.DecodeString(output.ScriptPublicKey)
		if err != nil || len(script) < 3 || len(script) > 2048 {
			return "", errors.New("invalid output script")
		}
		data = le.AppendUint64(data, output.Value)
		data = le.AppendUint16(data, binary.BigEndian.Uint16(script[:2]))
		data = le.AppendUint64(data, uint64(len(script)-2))
		data = append(data, script[2:]...)
	}
	data = le.AppendUint64(data, tx.LockTime)
	subnet, err := hex.DecodeString(tx.SubnetworkID)
	if err != nil || len(subnet) != 20 {
		return "", errors.New("invalid subnetwork")
	}
	data = append(data, subnet...)
	data = le.AppendUint64(data, tx.Gas)
	data = le.AppendUint64(data, uint64(len(payload)))
	data = append(data, payload...)
	hasher, err := blake2b.New256([]byte("TransactionID"))
	if err != nil {
		return "", err
	}
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type blockchainAnswer struct {
	TransactionID  string `json:"transaction_id"`
	AcceptingBlock string `json:"accepting_block"`
	EscrowKey      string `json:"escrow_key"`
	EncryptedBody  string `json:"encrypted_body"`
	ReportedTokens uint32 `json:"reported_tokens"`
}

// The outer V2 AiResponse carries the request ID, signer, and a private-body extension.
func parseAnswer(payload json.RawMessage, txid, block string) (string, blockchainAnswer, error) {
	var data []byte
	if len(payload) > 0 && payload[0] == '"' {
		var encoded string
		if err := json.Unmarshal(payload, &encoded); err != nil {
			return "", blockchainAnswer{}, err
		}
		var err error
		data, err = hex.DecodeString(encoded)
		if err != nil {
			return "", blockchainAnswer{}, err
		}
	} else if err := json.Unmarshal(payload, &data); err != nil {
		return "", blockchainAnswer{}, err
	}
	if len(data) < 179 || data[174] != 0x80 {
		return "", blockchainAnswer{}, errors.New("missing private AiResponse body")
	}
	encryptedSize := int(binary.LittleEndian.Uint32(data[175:179]))
	if encryptedSize < 33 || encryptedSize > 48*1024 || encryptedSize > len(data)-179 {
		return "", blockchainAnswer{}, fmt.Errorf("invalid AiResponse body size %d", encryptedSize)
	}
	body := data[179 : 179+encryptedSize]
	if !bytes.HasPrefix(body, envelopeMagic) {
		return "", blockchainAnswer{}, errors.New("invalid AiResponse envelope")
	}
	return hex.EncodeToString(data[:32]), blockchainAnswer{
		TransactionID: txid, AcceptingBlock: block, EscrowKey: hex.EncodeToString(data[78:110]),
		EncryptedBody: hex.EncodeToString(body), ReportedTokens: binary.LittleEndian.Uint32(data[74:78]),
	}, nil
}
