package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type replay struct {
	txid     string
	chain    json.RawMessage
	targets  int
	tipReads int
}

func (r *replay) call(_ context.Context, method string, params any, result any) error {
	switch method {
	case "getServerInfo":
		return json.Unmarshal([]byte(`{"networkId":"mainnet","isSynced":true,"virtualDaaScore":153150000}`), result)
	case "getServiceProviders":
		data := map[string]any{"providers": []map[string]any{
			{"tier": 0, "modelId": models[0].ID, "escrowPubkey": strings.Repeat("b", 64)},
			{"tier": 0, "modelId": models[0].ID, "escrowPubkey": strings.Repeat("b", 64)},
		}}
		raw, _ := json.Marshal(data)
		return json.Unmarshal(raw, result)
	case "getSink":
		r.tipReads++
		return json.Unmarshal([]byte(`{"sink":"`+strings.Repeat("a", 64)+`"}`), result)
	case "submitTransaction":
		r.targets++
		if params.(map[string]any)["allowOrphan"] != false {
			return errors.New("expected normal mempool submission")
		}
		if r.targets == 1 {
			return errors.New("simulated lost acknowledgment")
		}
		return json.Unmarshal([]byte(`{"transactionId":"`+r.txid+`"}`), result)
	case "getMempoolEntry":
		return errors.New("transaction not visible before retry")
	case "getVirtualChainFromBlockV2":
		call := params.(map[string]any)
		if call["startHash"] != strings.Repeat("a", 64) || call["minConfirmationCount"] != 10 {
			return errors.New("expected persisted pre-broadcast cursor and minimum confirmations")
		}
		return json.Unmarshal(r.chain, result)
	default:
		return errors.New("unexpected RPC call: " + method)
	}
}

func TestSubmitRetryAndFetchNativeEncryptedAnswer(t *testing.T) {
	raw, err := os.ReadFile("testdata/native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Submit    json.RawMessage
		ID        string
		AnswerID  string `json:"answer_id"`
		Escrow    string
		Encrypted string
		Chain     json.RawMessage
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(t.TempDir(), "relay.db")
	rpc := &replay{txid: fixture.ID, chain: fixture.Chain}
	start := func() (*registry, http.Handler) {
		saved, err := openRegistry(db, "mainnet", 32)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { saved.db.Close() })
		s := &server{rpc: rpc, network: "mainnet", activation: 140_115_000, registry: saved, inFlight: make(chan struct{}, 4)}
		return saved, s.routes()
	}
	saved, handler := start()
	submit := func(body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/requests", bytes.NewReader(body)))
		return rec
	}
	first := submit(fixture.Submit)
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"state":"broadcast_unconfirmed"`) {
		t.Fatalf("expected uncertain broadcast, got %d %s", first.Code, first.Body)
	}
	if err := saved.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, handler = start()

	for _, input := range []struct {
		body []byte
		want string
	}{
		{fixture.Submit, "submitted"},
		{fixture.Submit, "submitted"},
	} {
		reply := submit(input.body)
		if reply.Code != http.StatusAccepted || !strings.Contains(reply.Body.String(), `"state":"`+input.want+`"`) ||
			reply.Header().Get("Location") != "/v1/requests/"+fixture.ID {
			t.Fatalf("retry returned %d %s", reply.Code, reply.Body)
		}
	}
	if rpc.targets != 2 || rpc.tipReads != 1 {
		t.Fatalf("expected one retry and original cursor, got %d broadcasts and %d tip reads", rpc.targets, rpc.tipReads)
	}

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest("GET", "/v1/requests/"+fixture.ID, nil))
	var result struct {
		State   string
		Answers []blockchainAnswer
	}
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &result) != nil ||
		result.State != "answered" || len(result.Answers) != 1 {
		t.Fatalf("GET returned %d %s", got.Code, got.Body)
	}
	if result.Answers[0].TransactionID != fixture.AnswerID || result.Answers[0].EscrowKey != fixture.Escrow ||
		result.Answers[0].EncryptedBody != fixture.Encrypted {
		t.Fatal("relay altered the native encrypted answer")
	}

	catalog := httptest.NewRecorder()
	handler.ServeHTTP(catalog, httptest.NewRequest("GET", "/v1/models", nil))
	if catalog.Code != http.StatusOK || bytes.Count(catalog.Body.Bytes(), []byte(`"eligible_miners":1`)) != 1 {
		t.Fatalf("catalog returned %d %s", catalog.Code, catalog.Body)
	}
	invalidSubnet := bytes.Replace(fixture.Submit, []byte(aiRequestSubnet), []byte(aiResponseSubnet), 1)
	if res := submit(invalidSubnet); res.Code != http.StatusBadRequest || rpc.targets != 2 {
		t.Fatalf("unexpected forwarding of a non-inference request: %d %s", res.Code, res.Body)
	}
	wrongNetwork := bytes.Replace(fixture.Submit, []byte("mainnet"), []byte("testnet-10"), 1)
	if res := submit(wrongNetwork); res.Code != http.StatusBadRequest || rpc.targets != 2 {
		t.Fatalf("unexpected cross-network forwarding: %d %s", res.Code, res.Body)
	}
}

func TestReorgDropsAnOrphanedAnswer(t *testing.T) {
	previousBlock := strings.Repeat("a", 64)
	request := &record{
		ID: strings.Repeat("b", 64), Cursor: previousBlock, ConfirmedIn: previousBlock,
		Answers: []blockchainAnswer{{AcceptingBlock: previousBlock}}, Submitted: true, Created: time.Now(),
	}
	if err := applyBatch(request, chainBatch{Removed: []string{previousBlock}}); err != nil {
		t.Fatal(err)
	}
	if request.ConfirmedIn != "" || len(request.Answers) != 0 || !request.Submitted {
		t.Fatal("the answer and confirmation should follow the accepted chain")
	}
}
