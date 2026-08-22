// Package hailoclient calls the Hailo-8L HTTP sidecar (server/http_server.py in
// the Hailo repo) over loopback. It is the harness's accelerator lane (ADR 0024):
// LOCAL and free like llama-swap, never cloud — but a separate device with its
// own process, so it gets its own client rather than riding the OpenAI shape.
// Mirrors nimclient: pure net/http, no SDK, a result is a map the caller shapes.
package hailoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client targets one sidecar base (scheme://host:port, no path).
type Client struct {
	base string
	http *http.Client
}

// New builds a client. timeout bounds ONE call including a cold HEF load.
func New(base string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

// Base is the configured endpoint, for status reporting.
func (c *Client) Base() string { return c.base }

// Health returns the sidecar's hailo_status() dict. An unreachable sidecar is an
// error — the caller decides whether to spawn it (Sidecar.Ensure) or defer.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// Call POSTs args to /v1/<tool> and returns the tool's dict. A 200 carrying
// {"error":true,...} is a STRUCTURED RESULT (the tool refused the input), so it
// comes back as the map with a nil error — the MCP handler passes it through
// verbatim. Only transport failures and non-200 statuses are errors.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/"+tool, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) do(req *http.Request) (map[string]any, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hailo sidecar unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hailo sidecar %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("hailo sidecar returned non-JSON: %w", err)
	}
	return out, nil
}
