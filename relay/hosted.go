package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var hostedBucket = []byte("hosted-tasks-v1")

type account struct {
	ID             string `json:"id"`
	Network        string `json:"network"`
	TokenHash      string `json:"token_sha256"`
	FundingAddress string `json:"funding_address"`
	MaxRequest     uint64 `json:"max_request_sompi"`
	MaxDaily       uint64 `json:"max_daily_sompi"`
	Disabled       bool   `json:"disabled"`
}

type askInput struct {
	Key    string `json:"request_key"`
	Tier   int    `json:"tier"`
	Prompt string `json:"prompt"`
	Tokens int    `json:"max_tokens"`
	Cap    uint64 `json:"max_cost_sompi"`
}

type hostedTask struct {
	ID            string    `json:"id"`
	Fingerprint   string    `json:"fingerprint"`
	TransactionID string    `json:"transaction_id"`
	Cost          uint64    `json:"cost_sompi"`
	Created       time.Time `json:"created_at"`
}

type agentCommands interface {
	run(context.Context, string, []string, []byte) ([]byte, error)
}

type hostedService struct {
	server  *server
	root    string
	agent   agentCommands
	locks   [64]sync.Mutex
	origins map[string]bool
}

type nativeAgent struct {
	binary  string
	network string
}

// Arguments are fixed commands and generated IDs. Prompts travel through stdin.
func (agent nativeAgent) run(ctx context.Context, wallet string, args []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, agent.binary, args...)
	command.Env = append(os.Environ(), "KERYX_AGENT_DATA_DIR="+wallet, "KERYX_NETWORK="+agent.network)
	command.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("wallet operation %s failed; check node readiness, wallet funding and reserved inputs", args[0])
	}
	if output.Len() > 2<<20 {
		return nil, errors.New("wallet response too large")
	}
	return output.Bytes(), nil
}

func validAccount(id string) bool {
	if len(id) == 0 || len(id) > 48 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func newHosted(s *server, root, binary string) (*hostedService, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "accounts"), 0700); err != nil {
		return nil, err
	}
	if err := s.registry.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(hostedBucket)
		return err
	}); err != nil {
		return nil, err
	}
	origins := map[string]bool{}
	for _, origin := range strings.Split(env("KERYX_MCP_ORIGINS", ""), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins[origin] = true
		}
	}
	return &hostedService{server: s, root: abs, agent: nativeAgent{binary: binary, network: s.network}, origins: origins}, nil
}

func agentBinary() (string, error) {
	if path := os.Getenv("KERYX_AGENT_BINARY"); path != "" {
		return filepath.Abs(path)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "keryx-agent"
	if strings.HasSuffix(strings.ToLower(executable), ".exe") {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(executable), name), nil
}

func createAccount(args []string) error {
	if len(args) != 3 || !validAccount(args[0]) {
		return errors.New("usage: keryx-agent-api account-create NAME MAX_REQUEST_SOMPI MAX_DAILY_SOMPI")
	}
	perRequest, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return err
	}
	perDay, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil || perRequest < 80_000_000 || perRequest > perDay {
		return errors.New("limits must cover one request and max_daily must cover max_request")
	}
	network := env("KERYX_NETWORK", "mainnet")
	if network != "mainnet" && network != "testnet-10" {
		return errors.New("unsupported KERYX_NETWORK")
	}
	root, err := filepath.Abs(env("KERYX_API_DATA_DIR", "keryx-relay-"+network))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0700); err != nil {
		return err
	}
	path := filepath.Join(root, "accounts", args[0]+".json")
	if _, err := os.Stat(path); err == nil {
		return errors.New("account already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	binary, err := agentBinary()
	if err != nil {
		return err
	}
	agent := nativeAgent{binary: binary, network: network}
	output, err := agent.run(context.Background(), filepath.Join(root, "wallets", args[0]), []string{"init"}, nil)
	if err != nil {
		return err
	}
	var initialized struct {
		Address string `json:"funding_address"`
	}
	if err := json.Unmarshal(output, &initialized); err != nil || initialized.Address == "" {
		return errors.New("wallet did not return its funding address")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	token := "kx_" + args[0] + "_" + hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	account := account{ID: args[0], Network: network, TokenHash: hex.EncodeToString(hash[:]), FundingAddress: initialized.Address, MaxRequest: perRequest, MaxDaily: perDay}
	data, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"account": account.ID, "api_key": token, "funding_address": account.FundingAddress, "network": network})
}

func (h *hostedService) authenticate(r *http.Request) (*account, error) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errors.New("unauthorized")
	}
	token := parts[1]
	if !strings.HasPrefix(token, "kx_") {
		return nil, errors.New("unauthorized")
	}
	last := strings.LastIndexByte(token, '_')
	if last < 3 || !validAccount(token[3:last]) || len(token[last+1:]) != 64 {
		return nil, errors.New("unauthorized")
	}
	bytes, err := os.ReadFile(filepath.Join(h.root, "accounts", token[3:last]+".json"))
	if err != nil {
		return nil, errors.New("unauthorized")
	}
	var account account
	if json.Unmarshal(bytes, &account) != nil || account.Disabled || account.Network != h.server.network || account.ID != token[3:last] {
		return nil, errors.New("unauthorized")
	}
	saved, err := hex.DecodeString(account.TokenHash)
	actual := sha256.Sum256([]byte(token))
	if err != nil || subtle.ConstantTimeCompare(saved, actual[:]) != 1 {
		return nil, errors.New("unauthorized")
	}
	return &account, nil
}

func (h *hostedService) wallet(a *account) string { return filepath.Join(h.root, "wallets", a.ID) }

func taskKey(a *account, id string) []byte { return []byte(a.ID + ":" + id) }

func (h *hostedService) reserve(a *account, input askInput, cost uint64) (*hostedTask, error) {
	raw, _ := json.Marshal(input)
	fingerprint := sha256.Sum256(raw)
	id := sha256.Sum256([]byte(a.ID + ":" + input.Key))
	entry := &hostedTask{ID: hex.EncodeToString(id[:]), Fingerprint: hex.EncodeToString(fingerprint[:]), Cost: cost, Created: time.Now().UTC()}
	err := h.server.registry.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(hostedBucket)
		key := taskKey(a, entry.ID)
		if saved := bucket.Get(key); saved != nil {
			var previous hostedTask
			if err := json.Unmarshal(saved, &previous); err != nil {
				return err
			}
			if previous.Fingerprint != entry.Fingerprint {
				return errors.New("request_key already belongs to a different request")
			}
			entry = &previous
			return nil
		}
		if bucket.Stats().KeyN >= 4096 {
			return errors.New("hosted request registry is full")
		}
		used, err := dailyUsage(bucket, a, entry.Created)
		if err != nil {
			return err
		}
		if cost > a.MaxDaily || used > a.MaxDaily-cost {
			return errors.New("the daily spending limit is reached")
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
	return entry, err
}

func dailyUsage(bucket *bolt.Bucket, a *account, now time.Time) (uint64, error) {
	var total uint64
	prefix := []byte(a.ID + ":")
	cursor := bucket.Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		var task hostedTask
		if err := json.Unmarshal(value, &task); err != nil {
			return 0, err
		}
		if task.Created.UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
			if ^uint64(0)-total < task.Cost {
				return 0, errors.New("daily usage overflow")
			}
			total += task.Cost
		}
	}
	return total, nil
}

func (h *hostedService) getTask(a *account, id string) (*hostedTask, error) {
	if !hashText(id) {
		return nil, errors.New("unknown request ID")
	}
	var task hostedTask
	err := h.server.registry.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(hostedBucket).Get(taskKey(a, id))
		if value == nil {
			return errors.New("unknown request ID")
		}
		return json.Unmarshal(value, &task)
	})
	return &task, err
}

func (h *hostedService) saveTask(a *account, task *hostedTask) error {
	return h.server.registry.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(task)
		if err != nil {
			return err
		}
		return tx.Bucket(hostedBucket).Put(taskKey(a, task.ID), raw)
	})
}

func (h *hostedService) ask(ctx context.Context, a *account, input askInput) (any, error) {
	if input.Tokens == 0 {
		input.Tokens = 256
	}
	if input.Tier < 0 || input.Tier >= len(models) || strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 65536 || input.Tokens < 1 || input.Tokens > 4096 || len(input.Key) < 1 || len(input.Key) > 128 {
		return nil, errors.New("provide a request_key, prompt, tier 0 to 4 and max_tokens 1 to 4096")
	}
	cost := models[input.Tier].Reward + minimumFee
	if input.Cap < cost || cost > a.MaxRequest {
		return nil, errors.New("the request exceeds its spending limit")
	}
	// Serialize wallet access and task transitions for this account.
	guard := h.accountLock(a)
	if !guard.TryLock() {
		return nil, errors.New("another request uses this account; retry with the same request_key")
	}
	defer guard.Unlock()
	task, err := h.reserve(a, input, cost)
	if err != nil {
		return nil, err
	}
	if task.TransactionID == "" {
		preparedInput, _ := json.Marshal(map[string]any{"tier": input.Tier, "prompt": input.Prompt, "max_tokens": input.Tokens, "max_cost_sompi": cost})
		raw, err := h.agent.run(ctx, h.wallet(a), []string{"prepare-job", task.ID}, preparedInput)
		if err != nil {
			return nil, err
		}
		var prepared struct {
			ID   string `json:"transaction_id"`
			Cost uint64 `json:"cost_sompi"`
		}
		if json.Unmarshal(raw, &prepared) != nil || !hashText(prepared.ID) || prepared.Cost != cost {
			return nil, errors.New("wallet returned an unexpected signed request")
		}
		task.TransactionID = prepared.ID
		if err := h.saveTask(a, task); err != nil {
			return nil, err
		}
	}
	public, err := h.agent.run(ctx, h.wallet(a), []string{"export", task.TransactionID}, nil)
	if err != nil {
		return nil, err
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay/v1/requests", bytes.NewReader(public))
	response := &internalResponse{header: make(http.Header)}
	h.server.submit(response, r)
	if response.status != http.StatusAccepted {
		return nil, errors.New("the node has not accepted the request; retry with the same request_key")
	}
	var state struct{ State string }
	if json.Unmarshal(response.body.Bytes(), &state) != nil {
		return nil, errors.New("invalid relay submission response")
	}
	return map[string]any{"request_id": task.ID, "transaction_id": task.TransactionID, "state": state.State, "cost_sompi": task.Cost, "poll_after_seconds": 3}, nil
}

func (h *hostedService) accountLock(a *account) *sync.Mutex {
	hash := sha256.Sum256([]byte(a.ID))
	return &h.locks[int(hash[0])%len(h.locks)]
}

func (h *hostedService) result(ctx context.Context, a *account, id string) (any, error) {
	guard := h.accountLock(a)
	if !guard.TryLock() {
		return nil, errors.New("account is busy; retry shortly")
	}
	defer guard.Unlock()
	task, err := h.getTask(a, id)
	if err != nil {
		return nil, err
	}
	if task.TransactionID == "" {
		return map[string]any{"request_id": id, "state": "preparing", "retry": "retry keryx_ask with the original request_key and prompt"}, nil
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://relay/v1/requests/"+task.TransactionID, nil)
	r.SetPathValue("id", task.TransactionID)
	response := &internalResponse{header: make(http.Header)}
	h.server.status(response, r)
	if response.status != http.StatusOK {
		return nil, errors.New("request chain data is temporarily unavailable or outside the relay retention window")
	}
	decoded, err := h.agent.run(ctx, h.wallet(a), []string{"decode", task.TransactionID}, response.body.Bytes())
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(decoded, &result); err != nil {
		return nil, err
	}
	result["request_id"] = id
	result["poll_after_seconds"] = 3
	return result, nil
}

func (h *hostedService) accountInfo(a *account) (any, error) {
	var used uint64
	err := h.server.registry.db.View(func(tx *bolt.Tx) error {
		var err error
		used, err = dailyUsage(tx.Bucket(hostedBucket), a, time.Now())
		return err
	})
	return map[string]any{"account": a.ID, "network": a.Network, "funding_address": a.FundingAddress, "max_request_sompi": a.MaxRequest, "max_daily_sompi": a.MaxDaily, "reserved_today_sompi": used, "wallet_custody": "server", "day_timezone": "UTC"}, err
}

type internalResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *internalResponse) Header() http.Header { return r.header }
func (r *internalResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *internalResponse) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(data)
}

func readObject(r *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 300<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
