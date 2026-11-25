package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

type rpcCaller interface {
	call(context.Context, string, any, any) error
}

type nodeEndpoint string

type rpcRejection struct {
	Code uint64 `json:"code"`
}

func (e *rpcRejection) Error() string {
	return fmt.Sprintf("Keryx RPC rejected the call (code %d)", e.Code)
}

func (endpoint nodeEndpoint) call(parent context.Context, method string, params any, result any) error {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: 4 * time.Second}
	conn, _, err := dialer.DialContext(ctx, string(endpoint), nil)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadLimit(16 << 20)
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	if err := conn.WriteJSON(struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{1, method, params}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	var response struct {
		ID     uint64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  *rpcRejection   `json:"error"`
	}
	if err := conn.ReadJSON(&response); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if response.ID != 1 || (response.Method != "" && response.Method != method) {
		return errors.New("unexpected Keryx RPC response")
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Params) == 0 || string(response.Params) == "null" {
		return errors.New("Keryx RPC response does not contain data")
	}
	return json.Unmarshal(response.Params, result)
}
