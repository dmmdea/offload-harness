// Package accelclient calls an accelerator's HTTP sidecar over loopback — the
// Hailo-8L's (server/http_server.py in the Hailo repo) and the Coral Edge TPU's
// (accelerators/coral/server.py here), which speak one wire contract. It is the
// harness's accelerator lane (ADR 0024): LOCAL and free like llama-swap, never
// cloud — but a separate device with its own process, so it gets its own client
// rather than riding the OpenAI shape. Mirrors nimclient: pure net/http, no SDK,
// a result is a map the caller shapes.
//
// It was `hailoclient` until the second device arrived (Coral design D2): the
// package held nothing Hailo-specific but its name and error strings, so it was
// renamed and given a Device label rather than copied into a second package
// that would diverge from day one.
package accelclient

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

// Client targets one sidecar base (scheme://host:port, no path) for one device.
type Client struct {
	device string
	base   string
	http   *http.Client
}

// New builds a Hailo-8L client — the original constructor, kept so the Hailo
// path and its tests are unchanged. timeout bounds ONE call including a cold
// HEF load.
func New(base string, timeout time.Duration) *Client {
	return NewDevice("hailo-8l", base, timeout)
}

// NewDevice builds a client for the named accelerator (an id from
// config.Accelerators, e.g. "hailo-8l", "coral-edgetpu"). The label is carried
// on every error so a defer reads "<device>: ..." whichever lane produced it.
func NewDevice(device, base string, timeout time.Duration) *Client {
	return &Client{device: device, base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

// Base is the configured endpoint, for status reporting.
func (c *Client) Base() string { return c.base }

// Device is the accelerator id this client speaks for.
func (c *Client) Device() string { return c.device }

// Health returns the sidecar's /health dict (hailo_status() on the Hailo
// sidecar; {enabled, device, status, temp_c, loaded, ...} on the Coral one). An unreachable sidecar is an
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
		return nil, fmt.Errorf("%s sidecar unreachable at %s: %w", c.device, c.base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s sidecar %d: %s", c.device, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s sidecar returned non-JSON: %w", c.device, err)
	}
	return out, nil
}
