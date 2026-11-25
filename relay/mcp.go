package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema any             `json:"inputSchema"`
	Annotations map[string]bool `json:"annotations"`
}

func objectSchema(properties map[string]any, required ...string) any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func toolCatalog() []mcpTool {
	read := map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true}
	return []mcpTool{
		{Name: "keryx_models", Description: "List Keryx inference models, current eligible miners and minimum request costs. One KRX is 100000000 sompi.", InputSchema: objectSchema(map[string]any{}), Annotations: read},
		{Name: "keryx_account", Description: "Read this API key's funding address, request limits and today's reserved spend. This dedicated Keryx wallet is managed by the service operator.", InputSchema: objectSchema(map[string]any{}), Annotations: read},
		{Name: "keryx_ask", Description: "Submit a paid inference request to Keryx. Prompt plaintext is processed by this hosted service. Supply a stable unique request_key and spending cap in sompi. Retry the SAME key and arguments after a timeout to reuse the same transaction. Returns request_id for keryx_result.", InputSchema: objectSchema(map[string]any{
			"request_key":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"tier":           map[string]any{"type": "integer", "minimum": 0, "maximum": 4},
			"prompt":         map[string]any{"type": "string", "minLength": 1, "maxLength": 65536},
			"max_tokens":     map[string]any{"type": "integer", "minimum": 1, "maximum": 4096, "default": 256},
			"max_cost_sompi": map[string]any{"type": "integer", "minimum": 1},
		}, "request_key", "tier", "prompt", "max_cost_sompi"), Annotations: map[string]bool{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true}},
		{Name: "keryx_result", Description: "Get a Keryx request's status and any decrypted answer for this account. Poll no faster than every three seconds. Request ID comes from keryx_ask.", InputSchema: objectSchema(map[string]any{"request_id": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}}, "request_id"), Annotations: read},
	}
}

func protocol(version string) bool {
	return version == "2025-03-26" || version == "2025-06-18" || version == "2025-11-25"
}

func (h *hostedService) mcp(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !h.origins[origin] {
		problem(w, http.StatusForbidden, "origin_forbidden", "This origin is not allowed.")
		return
	}
	a, err := h.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"keryx\"")
		problem(w, http.StatusUnauthorized, "unauthorized", "Provide your Keryx API key as a bearer token.")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		problem(w, http.StatusUnsupportedMediaType, "content_type", "Use application/json.")
		return
	}
	if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		problem(w, http.StatusNotAcceptable, "accept", "Accept application/json and text/event-stream.")
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !protocol(version) {
		problem(w, http.StatusBadRequest, "unsupported_protocol", "Unsupported MCP protocol version.")
		return
	}
	var message mcpRequest
	r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
	if err := readObject(r, &message); err != nil {
		mcpError(w, nil, -32700, "Parse error")
		return
	}
	if message.JSONRPC != "2.0" || message.Method == "" || (len(message.ID) > 0 && !validRPCID(message.ID)) {
		mcpError(w, nil, -32600, "Invalid Request")
		return
	}
	if len(message.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch message.Method {
	case "initialize":
		var init struct {
			Version string `json:"protocolVersion"`
		}
		if json.Unmarshal(message.Params, &init) != nil || init.Version == "" {
			mcpError(w, message.ID, -32602, "Provide protocolVersion")
			return
		}
		version := init.Version
		if !protocol(version) {
			version = "2025-11-25"
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "keryx", "version": "0.3.0"}, "instructions": "Use keryx_models to check current costs, keryx_account for funding and limits, keryx_ask to submit with a stable request_key and explicit spending cap, and keryx_result to poll. Reuse the same request_key after an interrupted submission."}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": toolCatalog()}
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal(message.Params, &call) != nil || call.Name == "" {
			mcpError(w, message.ID, -32602, "Provide a tool name and arguments")
			return
		}
		value, err := h.callTool(r, a, call.Name, call.Arguments)
		if err != nil {
			result = map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}}
		} else {
			encoded, _ := json.Marshal(value)
			result = map[string]any{"isError": false, "structuredContent": value, "content": []any{map[string]string{"type": "text", "text": string(encoded)}}}
		}
	default:
		mcpError(w, message.ID, -32601, "Method not found")
		return
	}
	reply(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
}

func validRPCID(raw []byte) bool {
	var id any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&id) != nil {
		return false
	}
	switch id.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func mcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	reply(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func toolArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("tool arguments must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (h *hostedService) callTool(r *http.Request, a *account, name string, args json.RawMessage) (any, error) {
	switch name {
	case "keryx_models":
		if err := toolArguments(args, &struct{}{}); err != nil {
			return nil, err
		}
		response := &internalResponse{header: make(http.Header)}
		h.server.modelList(response, r)
		if response.status != http.StatusOK {
			return nil, errors.New("Keryx model catalog is temporarily unavailable")
		}
		var data map[string]any
		if err := json.Unmarshal(response.body.Bytes(), &data); err != nil {
			return nil, err
		}
		return data, nil
	case "keryx_account":
		if err := toolArguments(args, &struct{}{}); err != nil {
			return nil, err
		}
		return h.accountInfo(a)
	case "keryx_ask":
		var input askInput
		if err := toolArguments(args, &input); err != nil {
			return nil, err
		}
		return h.ask(r.Context(), a, input)
	case "keryx_result":
		var input struct {
			ID string `json:"request_id"`
		}
		if err := toolArguments(args, &input); err != nil {
			return nil, err
		}
		return h.result(r.Context(), a, input.ID)
	default:
		return nil, errors.New("unknown Keryx tool")
	}
}

func (h *hostedService) rest(handler func(http.ResponseWriter, *http.Request, *account)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := h.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer realm=\"keryx\"")
			problem(w, http.StatusUnauthorized, "unauthorized", "Provide your API key.")
			return
		}
		handler(w, r, a)
	}
}

func (h *hostedService) restAsk(w http.ResponseWriter, r *http.Request, a *account) {
	var input askInput
	r.Body = http.MaxBytesReader(w, r.Body, 300<<10)
	if err := readObject(r, &input); err != nil {
		problem(w, 400, "invalid_request", "Provide JSON with prompt, tier, request_key and max_cost_sompi.")
		return
	}
	value, err := h.ask(r.Context(), a, input)
	if err != nil {
		problem(w, 422, "request_failed", err.Error())
		return
	}
	reply(w, http.StatusAccepted, value)
}

func (h *hostedService) restResult(w http.ResponseWriter, r *http.Request, a *account) {
	value, err := h.result(r.Context(), a, r.PathValue("id"))
	if err != nil {
		problem(w, 422, "result_unavailable", err.Error())
		return
	}
	reply(w, http.StatusOK, value)
}
