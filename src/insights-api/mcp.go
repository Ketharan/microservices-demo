package main

// A minimal MCP (Model Context Protocol) client, standard library only.
//
// WHY HAND-ROLLED. The official Go SDK would pull a module tree, and the image
// for this service is built inside the cluster by a pipeline that cannot be
// relied on to reach a module proxy. The protocol surface we need is three
// calls -- initialize, notifications/initialized, tools/call -- so a dependency
// would cost more than it saves.
//
// TRANSPORT. Streamable HTTP: JSON-RPC 2.0 over POST. A server may answer with
// either application/json or text/event-stream, and the gateway in front of the
// observer is free to choose, so both are parsed. An SSE answer carries the
// JSON-RPC envelope in its `data:` lines.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// The protocol revision the observer's SDK speaks. Sent in initialize; a
	// server that wants a different one says so in its reply and we proceed
	// anyway, because every call we make is stable across these revisions.
	protocolVersion = "2024-11-05"
	mcpTimeout      = 25 * time.Second
)

type mcpClient struct {
	endpoint string
	http     *http.Client

	// component is sent as x-openchoreo-component on every request. The
	// ai-mcp-federation trait's HTTPRoute matches on exactly that header --
	// its path match is PathPrefix "/", so the header is the ONLY selector.
	// That is how several components share one gateway port. Omit it and the
	// gateway has no route for the request and answers 404 "route not found",
	// which reads as the gateway being unconfigured rather than the client
	// being unidentified. Cost: several hours, 2026-10-29.
	component string

	mu        sync.Mutex
	nextID    int64
	sessionID string
	ready     bool
}

func newMCPClient(endpoint, component string) *mcpClient {
	return &mcpClient{
		endpoint:  endpoint,
		component: component,
		http:      &http.Client{Timeout: mcpTimeout},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// toolResult is the shape the observer returns: the handler JSON-marshals its
// response and hands it back as a single text content block, so the payload we
// actually want is a JSON document carried inside a string.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (c *mcpClient) call(ctx context.Context, method string, params any, notification bool) (json.RawMessage, error) {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	if !notification {
		c.mu.Lock()
		c.nextID++
		id := c.nextID
		c.mu.Unlock()
		req.ID = &id
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Both are advertised because the server picks; see the transport note above.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("MCP-Protocol-Version", protocolVersion)
	if c.component != "" {
		httpReq.Header.Set("x-openchoreo-component", c.component)
	}
	c.mu.Lock()
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A session id may be minted on any response, not just initialize.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted || notification {
		// Notifications get 202 with no body, which is success.
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := make([]byte, 512)
		n, _ := resp.Body.Read(snippet)
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet[:n])))
	}

	envelope, err := readRPCEnvelope(resp)
	if err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, envelope.Error
	}
	return envelope.Result, nil
}

// readRPCEnvelope copes with both reply encodings. For SSE it returns the first
// data line that parses as a JSON-RPC response carrying a result or an error --
// servers may interleave comments, pings and progress notifications, and those
// have no id and must not be mistaken for the answer.
func readRPCEnvelope(resp *http.Response) (*rpcResponse, error) {
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		var out rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("mcp decode: %w", err)
		}
		return &out, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var out rpcResponse
		if err := json.Unmarshal([]byte(payload), &out); err != nil {
			continue
		}
		if out.Result == nil && out.Error == nil {
			continue // a notification, not our answer
		}
		return &out, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp stream: %w", err)
	}
	return nil, fmt.Errorf("mcp stream ended with no result")
}

// ensureReady performs the handshake once. It is tried again on the next call
// if it fails, because the gateway may come up after this service does and a
// permanently-poisoned client would need a pod restart to recover.
func (c *mcpClient) ensureReady(ctx context.Context) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if ready {
		return nil
	}

	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "insights-api", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	// Best effort: a server that does not require the notification will ignore
	// it, and one that does would reject the first tools/call without it.
	_, _ = c.call(ctx, "notifications/initialized", map[string]any{}, true)

	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	return nil
}

// callTool runs one MCP tool and returns the JSON document the observer put
// inside the text content block.
func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if err := c.ensureReady(ctx); err != nil {
		return nil, fmt.Errorf("mcp handshake: %w", err)
	}

	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	var result toolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: decode result: %w", name, err)
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("%s: empty result", name)
	}
	text := result.Content[0].Text
	if result.IsError {
		return nil, fmt.Errorf("%s: tool reported an error: %s", name, strings.TrimSpace(text))
	}
	return json.RawMessage(text), nil
}

// listTools backs the debug endpoint. It exists because the first thing anyone
// needs to know in-cluster is whether the gateway is reachable at all, and
// which tools it is actually fronting -- far quicker than inferring it from a
// failing question.
func (c *mcpClient) listTools(ctx context.Context) ([]string, error) {
	if err := c.ensureReady(ctx); err != nil {
		return nil, fmt.Errorf("mcp handshake: %w", err)
	}
	raw, err := c.call(ctx, "tools/list", map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Tools))
	for _, t := range out.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}
