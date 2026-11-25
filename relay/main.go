package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "--version" {
			fmt.Println("Keryx Agent API 0.3.0")
			return
		}
		if os.Args[1] == "--help" || os.Args[1] == "-h" {
			fmt.Println("Keryx Agent API 0.3.0\nRun without arguments to serve the public API and MCP endpoint.\naccount-create NAME MAX_REQUEST_SOMPI MAX_DAILY_SOMPI creates a hosted account.")
			return
		}
		if os.Args[1] == "account-create" {
			if err := createAccount(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		}
		log.Fatal("usage: keryx-agent-api [--help|--version]")
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}

func run() error {
	netName := env("KERYX_NETWORK", "mainnet")
	activation, rpcPort := uint64(140_115_000), "24110"
	switch netName {
	case "mainnet":
	case "testnet-10":
		activation, rpcPort = 3_500, "24210"
	default:
		return errors.New("KERYX_NETWORK must be mainnet or testnet-10")
	}
	network := netName
	rpcURL := env("KERYX_RPC_URL", "ws://127.0.0.1:"+rpcPort)
	u, err := url.Parse(rpcURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" {
		return errors.New("KERYX_RPC_URL must be a JSON wRPC ws:// or wss:// endpoint")
	}
	port, err := strconv.Atoi(env("KERYX_API_PORT", "8787"))
	if err != nil || port < 1 || port > 65535 {
		return errors.New("KERYX_API_PORT must be a TCP port number")
	}
	dir := env("KERYX_API_DATA_DIR", "keryx-relay-"+netName)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	registry, err := openRegistry(filepath.Join(dir, "requests.db"), network, 4096)
	if err != nil {
		return err
	}
	defer registry.db.Close()
	if err := registry.prune(time.Now()); err != nil {
		return err
	}
	s := &server{
		rpc: nodeEndpoint(rpcURL), network: network, activation: activation, registry: registry,
		trustProxy: env("KERYX_TRUST_CADDY", "0") == "1", inFlight: make(chan struct{}, 16),
	}
	binary, err := agentBinary()
	if err != nil {
		return err
	}
	s.hosted, err = newHosted(s, dir, binary)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	httpServer := &http.Server{
		Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second,
		IdleTimeout: 20 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	log.Printf("Keryx Agent API %s listening on http://%s", network, addr)
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdown)
		case now := <-ticker.C:
			if err := registry.prune(now); err != nil {
				log.Printf("Request registry cleanup: %v", err)
			}
		}
	}
}
