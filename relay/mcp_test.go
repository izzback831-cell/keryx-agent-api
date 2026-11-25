package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type hostedWalletReplay struct {
	id        string
	public    json.RawMessage
	generated map[string]bool
	lost      bool
}

func (wallet *hostedWalletReplay) run(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
	switch args[0] {
	case "prepare-job":
		if wallet.generated == nil {
			wallet.generated = map[string]bool{}
		}
		wallet.generated[args[1]] = true
		if !wallet.lost {
			wallet.lost = true
			return nil, errors.New("simulated disconnect after saving the signed transaction")
		}
		return json.Marshal(map[string]any{"transaction_id": wallet.id, "cost_sompi": 80_000_000})
	case "export":
		return wallet.public, nil
	case "decode":
		return json.Marshal(map[string]any{"transaction_id": wallet.id, "state": "answered", "answer": map[string]string{"text": "Replayed Keryx answer"}})
	}
	return nil, errors.New("unexpected wallet operation")
}

func TestHostedMCPAccountRetryAndIsolation(t *testing.T) {
	raw, err := os.ReadFile("testdata/native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Submit json.RawMessage
		ID     string
		Chain  json.RawMessage
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	registry, err := openRegistry(filepath.Join(root, "relay.db"), "mainnet", 64)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.db.Close()
	rpc := &replay{txid: fixture.ID, chain: fixture.Chain, targets: 1}
	s := &server{rpc: rpc, network: "mainnet", activation: 140_115_000, registry: registry, inFlight: make(chan struct{}, 16)}
	h, err := newHosted(s, root, "unused-test-binary")
	if err != nil {
		t.Fatal(err)
	}
	wallet := &hostedWalletReplay{id: fixture.ID, public: fixture.Submit}
	h.agent = wallet
	s.hosted = h
	tokens := map[string]string{}
	for _, name := range []string{"alice", "bob"} {
		token := "kx_" + name + "_" + strings.Repeat("1", 64)
		digest := sha256.Sum256([]byte(token))
		a := account{ID: name, Network: "mainnet", TokenHash: hex.EncodeToString(digest[:]), FundingAddress: "keryx:test-fixture", MaxRequest: 80_000_000, MaxDaily: 80_000_000}
		data, _ := json.Marshal(a)
		if err := os.WriteFile(filepath.Join(root, "accounts", name+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		tokens[name] = token
	}
	api := s.routes()
	call := func(token, method string, params any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		request := httptest.NewRequest("POST", "/mcp", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", "Bearer "+token)
		reply := httptest.NewRecorder()
		api.ServeHTTP(reply, request)
		return reply
	}
	if res := call("invalid", "initialize", map[string]any{"protocolVersion": "2025-11-25"}); res.Code != 401 {
		t.Fatalf("bad key accepted: %d", res.Code)
	}
	res := call(tokens["alice"], "initialize", map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]string{"name": "codex-compatible-test", "version": "1"}, "capabilities": map[string]any{}})
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("handshake: %s", res.Body)
	}
	res = call(tokens["alice"], "tools/list", map[string]any{})
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"name":"keryx_ask"`) {
		t.Fatalf("tools: %s", res.Body)
	}
	args := map[string]any{"tier": 0, "prompt": "question", "max_tokens": 128, "max_cost_sompi": 80_000_000, "request_key": "same-question"}
	request := map[string]any{"name": "keryx_ask", "arguments": args}
	res = call(tokens["alice"], "tools/call", request)
	if !strings.Contains(res.Body.String(), `"isError":true`) {
		t.Fatalf("expected saved preparation interruption: %s", res.Body)
	}
	// A reconnect reuses the task, wallet preparation key and spending reservation.
	h2, err := newHosted(s, root, "unused-test-binary")
	if err != nil {
		t.Fatal(err)
	}
	h2.agent = wallet
	s.hosted = h2
	api = s.routes()
	res = call(tokens["alice"], "tools/call", request)
	if !strings.Contains(res.Body.String(), `"state":"submitted"`) || len(wallet.generated) != 1 {
		t.Fatalf("retry: %s", res.Body)
	}
	var response struct {
		Result struct {
			Structured struct {
				ID string `json:"request_id"`
			} `json:"structuredContent"`
		}
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id := response.Result.Structured.ID
	res = call(tokens["alice"], "tools/call", map[string]any{"name": "keryx_result", "arguments": map[string]any{"request_id": id}})
	if !strings.Contains(res.Body.String(), "Replayed Keryx answer") {
		t.Fatalf("answer: %s", res.Body)
	}
	res = call(tokens["bob"], "tools/call", map[string]any{"name": "keryx_result", "arguments": map[string]any{"request_id": id}})
	if !strings.Contains(res.Body.String(), `"isError":true`) {
		t.Fatalf("cross-account result access: %s", res.Body)
	}
	res = call(tokens["alice"], "tools/call", map[string]any{"name": "keryx_account", "arguments": map[string]any{}})
	if !strings.Contains(res.Body.String(), `"reserved_today_sompi":80000000`) || strings.Contains(res.Body.String(), tokens["alice"]) {
		t.Fatalf("budget or key leakage: %s", res.Body)
	}
	args["prompt"] = "changed-question"
	res = call(tokens["alice"], "tools/call", request)
	if !strings.Contains(res.Body.String(), "different request") {
		t.Fatalf("idempotency conflict: %s", res.Body)
	}
	args["request_key"] = "second-key"
	res = call(tokens["alice"], "tools/call", request)
	if !strings.Contains(res.Body.String(), "daily spending limit") || rpc.targets != 2 {
		t.Fatalf("budget bypass: %s", res.Body)
	}
	badOrigin := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	badOrigin.Header.Set("Origin", "https://untrusted.example")
	badOrigin.Header.Set("Authorization", "Bearer "+tokens["alice"])
	blocked := httptest.NewRecorder()
	api.ServeHTTP(blocked, badOrigin)
	if blocked.Code != 403 {
		t.Fatalf("untrusted origin accepted: %d", blocked.Code)
	}
}
