package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

//go:embed openapi.json
var openAPI []byte

const scanInterval = 3 * time.Second

type server struct {
	rpc        rpcCaller
	network    string
	activation uint64
	registry   *registry
	trustProxy bool
	rates      rates
	inFlight   chan struct{}
	locks      [64]sync.Mutex
	hosted     *hostedService
}

type rates struct {
	mu     sync.Mutex
	minute int64
	counts map[netip.Addr]rateCount
}

type rateCount struct {
	reads  int
	writes int
}

func (r *rates) take(ip netip.Addr, writing bool) bool {
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	minute := time.Now().Unix() / 60
	if r.counts == nil || minute != r.minute {
		r.minute = minute
		r.counts = make(map[netip.Addr]rateCount)
	}
	count, tracked := r.counts[ip]
	if !tracked && len(r.counts) >= 8192 {
		return false
	}
	if writing {
		if count.writes >= 12 {
			return false
		}
		count.writes++
	} else {
		if count.reads >= 120 {
			return false
		}
		count.reads++
	}
	r.counts[ip] = count
	return true
}

func (s *server) routes() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openAPI)
	})
	router.HandleFunc("GET /v1/models", s.admit(s.modelList))
	router.HandleFunc("POST /v1/requests", s.admit(s.submit))
	router.HandleFunc("GET /v1/requests/{id}", s.admit(s.status))
	if s.hosted != nil {
		router.HandleFunc("/mcp", s.admit(s.hosted.mcp))
		router.HandleFunc("POST /v1/agent/requests", s.admit(s.hosted.rest(s.hosted.restAsk)))
		router.HandleFunc("GET /v1/agent/requests/{id}", s.admit(s.hosted.rest(s.hosted.restResult)))
		router.HandleFunc("GET /v1/account", s.admit(s.hosted.rest(func(w http.ResponseWriter, r *http.Request, a *account) {
			value, err := s.hosted.accountInfo(a)
			if err != nil {
				storeError(w, err)
				return
			}
			reply(w, http.StatusOK, value)
		})))
	}
	return router
}

func (s *server) admit(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip, _ := netip.ParseAddr(host)
		if s.trustProxy && ip.Unmap().IsLoopback() {
			if forwarded, err := netip.ParseAddr(r.Header.Get("X-Keryx-Client-IP")); err == nil {
				ip = forwarded
			}
		}
		if !s.rates.take(ip.Unmap(), r.Method == http.MethodPost && r.URL.Path != "/mcp") {
			w.Header().Set("Retry-After", "60")
			problem(w, http.StatusTooManyRequests, "rate_limited", "Wait before sending more requests.")
			return
		}
		select {
		case s.inFlight <- struct{}{}:
			defer func() { <-s.inFlight }()
		default:
			w.Header().Set("Retry-After", "3")
			problem(w, http.StatusServiceUnavailable, "busy", "The relay is busy. Retry shortly.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 65*time.Second)
		defer cancel()
		handler(w, r.WithContext(ctx))
	}
}

type nodeInfo struct {
	Network string `json:"networkId"`
	Synced  bool   `json:"isSynced"`
	DAA     uint64 `json:"virtualDaaScore"`
}

func (s *server) info(ctx context.Context) (nodeInfo, error) {
	var info nodeInfo
	err := s.rpc.call(ctx, "getServerInfo", struct{}{}, &info)
	if err == nil && info.Network != s.network {
		err = errors.New("Keryx RPC network mismatch")
	}
	return info, err
}

func (s *server) modelList(w http.ResponseWriter, r *http.Request) {
	info, err := s.info(r.Context())
	if err != nil {
		upstreamError(w, err)
		return
	}
	active := info.DAA >= s.activation
	list := append([]model(nil), models...)
	if active && info.Synced {
		var response struct {
			Providers []struct {
				Tier   int    `json:"tier"`
				Model  string `json:"modelId"`
				Escrow string `json:"escrowPubkey"`
			} `json:"providers"`
		}
		err := s.rpc.call(r.Context(), "getServiceProviders", struct{}{}, &response)
		if err != nil {
			upstreamError(w, err)
			return
		}
		seen := make(map[[2]string]struct{})
		for _, provider := range response.Providers {
			if provider.Tier < 0 || provider.Tier >= len(list) || provider.Model != list[provider.Tier].ID || !hashText(provider.Escrow) {
				continue
			}
			key := [2]string{provider.Model, provider.Escrow}
			if _, duplicate := seen[key]; !duplicate {
				list[provider.Tier].EligibleMiners++
				seen[key] = struct{}{}
			}
		}
	}
	for i := range list {
		list[i].Fee = minimumFee
		list[i].Cost = list[i].Reward + list[i].Fee
	}
	reply(w, http.StatusOK, map[string]any{
		"network": info.Network, "synced": info.Synced, "inference_active": active,
		"virtual_daa_score": info.DAA, "models": list,
	})
}

func (s *server) lock(id string) *sync.Mutex {
	hasher := fnv.New32a()
	hasher.Write([]byte(id))
	return &s.locks[hasher.Sum32()%uint32(len(s.locks))]
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	const limit = 300 << 10
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var received struct {
		Network     string          `json:"network"`
		Transaction json.RawMessage `json:"transaction"`
	}
	if err := decoder.Decode(&received); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, "request_too_large", "The body limit is 300 KiB.")
			return
		}
		problem(w, http.StatusBadRequest, "invalid_json", "Supply a network and a signed Keryx transaction.")
		return
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		problem(w, http.StatusBadRequest, "invalid_json", "Expected a single JSON object.")
		return
	}
	if received.Network != s.network {
		problem(w, http.StatusBadRequest, "network_mismatch", "Use the network reported by GET /v1/models.")
		return
	}
	var tx transaction
	if err := json.Unmarshal(received.Transaction, &tx); err != nil {
		problem(w, http.StatusBadRequest, "invalid_transaction", "Use the native Keryx JSON transaction format.")
		return
	}
	id, err := tx.requestID()
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid_transaction", err.Error())
		return
	}
	guard := s.lock(id)
	if !guard.TryLock() {
		processing(w)
		return
	}
	defer guard.Unlock()
	entry, err := s.registry.get(id, time.Now())
	if err != nil {
		storeError(w, err)
		return
	}
	if entry != nil && (entry.Submitted || entry.ConfirmedIn != "" || len(entry.Answers) > 0) {
		publishStatus(w, http.StatusAccepted, entry, s.network)
		return
	}
	ctx := r.Context()
	info, err := s.info(ctx)
	if err != nil {
		upstreamError(w, err)
		return
	}
	if !info.Synced || info.DAA < s.activation {
		problem(w, http.StatusServiceUnavailable, "inference_unavailable", "The node must be synced past the inference activation.")
		return
	}
	registeredNow := entry == nil
	if registeredNow {
		var tip struct{ Sink string }
		if err := s.rpc.call(ctx, "getSink", struct{}{}, &tip); err != nil || !hashText(tip.Sink) {
			upstreamError(w, err)
			return
		}
		entry = &record{ID: id, Cursor: tip.Sink, Created: time.Now().UTC(), Answers: []blockchainAnswer{}}
		if err := s.registry.save(entry); err != nil {
			storeError(w, err)
			return
		}
	}
	var receipt struct {
		ID string `json:"transactionId"`
	}
	submitError := s.rpc.call(ctx, "submitTransaction", map[string]any{
		"transaction": tx, "allowOrphan": false,
	}, &receipt)
	if submitError == nil && receipt.ID == id {
		entry.Submitted = true
	} else {
		var existing struct {
			Entry json.RawMessage `json:"mempoolEntry"`
		}
		err := s.rpc.call(ctx, "getMempoolEntry", map[string]any{
			"transactionId": id, "includeOrphanPool": false, "filterTransactionPool": false,
		}, &existing)
		entry.Submitted = err == nil && len(existing.Entry) > 0 && string(existing.Entry) != "null"
		var submitRejection, mempoolRejection *rpcRejection
		if registeredNow && !entry.Submitted && errors.As(submitError, &submitRejection) && errors.As(err, &mempoolRejection) {
			if err := s.registry.discardRejected(id); err != nil {
				storeError(w, err)
				return
			}
			problem(w, http.StatusUnprocessableEntity, "transaction_rejected", "The Keryx node rejected this transaction.")
			return
		}
	}
	if entry.Submitted {
		if err := s.registry.save(entry); err != nil {
			storeError(w, err)
			return
		}
	}
	publishStatus(w, http.StatusAccepted, entry, s.network)
}

type chainBatch struct {
	Removed []string `json:"removedChainBlockHashes"`
	Added   []string `json:"addedChainBlockHashes"`
	Blocks  []struct {
		Transactions []struct {
			Subnet  string          `json:"subnetworkId"`
			Payload json.RawMessage `json:"payload"`
			Verbose struct {
				ID string `json:"transactionId"`
			} `json:"verboseData"`
		} `json:"acceptedTransactions"`
	} `json:"chainBlockAcceptedTransactions"`
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hashText(id) {
		problem(w, http.StatusNotFound, "request_not_found", "Request ID is unknown or expired.")
		return
	}
	guard := s.lock(id)
	if !guard.TryLock() {
		processing(w)
		return
	}
	defer guard.Unlock()
	now := time.Now().UTC()
	entry, err := s.registry.get(id, now)
	if err != nil {
		storeError(w, err)
		return
	}
	if entry == nil {
		problem(w, http.StatusNotFound, "request_not_found", "Request ID is unknown or expired.")
		return
	}
	if now.Sub(entry.LastScan) >= scanInterval {
		if err := s.scan(r.Context(), entry); err != nil {
			upstreamError(w, err)
			return
		}
		entry.LastScan = now
		if err := s.registry.save(entry); err != nil {
			storeError(w, err)
			return
		}
	}
	publishStatus(w, http.StatusOK, entry, s.network)
}

func (s *server) scan(ctx context.Context, entry *record) error {
	info, err := s.info(ctx)
	if err != nil {
		return err
	}
	if !info.Synced {
		return errors.New("node is not synchronized")
	}
	var chain chainBatch
	if err := s.rpc.call(ctx, "getVirtualChainFromBlockV2", map[string]any{
		"startHash": entry.Cursor, "dataVerbosityLevel": "Full", "minConfirmationCount": 10,
	}, &chain); err != nil {
		return err
	}
	return applyBatch(entry, chain)
}

func applyBatch(entry *record, chain chainBatch) error {
	if len(chain.Added) != len(chain.Blocks) {
		return errors.New("node returned unmatched chain and transaction lists")
	}
	removed := make(map[string]bool, len(chain.Removed))
	for _, id := range chain.Removed {
		removed[id] = true
	}
	if removed[entry.ConfirmedIn] {
		entry.ConfirmedIn = ""
	}
	answers := entry.Answers[:0]
	seen := make(map[string]bool)
	for _, answer := range entry.Answers {
		if !removed[answer.AcceptingBlock] {
			answers = append(answers, answer)
			seen[answer.TransactionID] = true
		}
	}
	entry.Answers = answers
	for i, block := range chain.Blocks {
		id := chain.Added[i]
		if !hashText(id) {
			return errors.New("invalid accepting block ID")
		}
		for _, tx := range block.Transactions {
			if tx.Verbose.ID == entry.ID {
				entry.ConfirmedIn = id
				entry.Submitted = true
			}
			if tx.Subnet != aiResponseSubnet || !hashText(tx.Verbose.ID) || seen[tx.Verbose.ID] || len(entry.Answers) == 16 {
				continue
			}
			request, answer, err := parseAnswer(tx.Payload, tx.Verbose.ID, id)
			if err == nil && request == entry.ID {
				seen[answer.TransactionID] = true
				entry.Answers = append(entry.Answers, answer)
			}
		}
		entry.Cursor = id
	}
	return nil
}

func publishStatus(w http.ResponseWriter, httpStatus int, entry *record, network string) {
	state := "broadcast_unconfirmed"
	switch {
	case len(entry.Answers) > 0:
		state = "answered"
	case entry.ConfirmedIn != "":
		state = "confirmed"
	case entry.Submitted:
		state = "submitted"
	}
	w.Header().Set("Location", "/v1/requests/"+entry.ID)
	w.Header().Set("Retry-After", "3")
	reply(w, httpStatus, map[string]any{
		"transaction_id": entry.ID, "network": network, "state": state,
		"confirmed_in": entry.ConfirmedIn, "answers": entry.Answers,
	})
}

func processing(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "3")
	problem(w, http.StatusTooManyRequests, "request_busy", "The relay is already processing this request.")
}

func upstreamError(w http.ResponseWriter, err error) {
	if err != nil {
		log.Printf("Keryx RPC lookup failed: %v", err)
	}
	problem(w, http.StatusServiceUnavailable, "node_unavailable", "The relay could not obtain the required chain data.")
}

func storeError(w http.ResponseWriter, err error) {
	log.Printf("Request registry operation failed: %v", err)
	problem(w, http.StatusServiceUnavailable, "registry_unavailable", "The request registry is temporarily unavailable.")
}

func problem(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
