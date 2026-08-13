// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package mirror holds the epoch-aware llama-swap activity/event mirror and the
// typed client the spine commands share.
//
// The client is deliberately separate from internal/client (the generated
// endpoint-mirror transport): the mirror needs exact control over caching,
// deadlines, and status-code visibility (a 404 from /slots is a documented
// fallback signal, not an error), which the generated client abstracts away.
package mirror

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the loopback address llama-swap listens on for this
// deployment. Always an IP: resolving a hostname can stall ~21s on an ::1
// first-try in this environment, which is why the house rule bans the name.
const DefaultBaseURL = "http://127.0.0.1:11436"

// Client is a typed, read-mostly llama-swap client.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a client bound to baseURL with a per-request timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) do(ctx context.Context, method, path string, params map[string]string) (int, []byte, error) {
	u := c.BaseURL + path
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			if v != "" {
				q.Set(k, v)
			}
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func (c *Client) getJSON(ctx context.Context, path string, params map[string]string, out any) error {
	status, body, err := c.do(ctx, http.MethodGet, path, params)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if status < 200 || status >= 300 {
		return &HTTPError{Status: status, Path: path, Body: truncate(string(body), 400)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

// HTTPError carries the status code so callers can branch on 404
// (endpoint absent → documented fallback) versus 5xx (unobservable → fail closed).
type HTTPError struct {
	Status int
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %s", e.Status, e.Path, e.Body)
}

// RunningEntry is one row of GET /running.
type RunningEntry struct {
	Model       string `json:"model"`
	State       string `json:"state"`
	Cmd         string `json:"cmd"`
	Proxy       string `json:"proxy"`
	TTL         int    `json:"ttl"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Running lists the models currently holding VRAM.
func (c *Client) Running(ctx context.Context) ([]RunningEntry, error) {
	var out struct {
		Running []RunningEntry `json:"running"`
	}
	if err := c.getJSON(ctx, "/running", nil, &out); err != nil {
		return nil, err
	}
	return out.Running, nil
}

// RosterEntry is one roster model from GET /v1/models, alias-resolved.
type RosterEntry struct {
	ID      string
	Name    string
	Aliases []string
	State   string
}

// Models returns the full roster including meta.llamaswap.aliases.
func (c *Client) Models(ctx context.Context) ([]RosterEntry, error) {
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Meta struct {
				Llamaswap struct {
					Aliases []string `json:"aliases"`
				} `json:"llamaswap"`
			} `json:"meta"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/models", nil, &out); err != nil {
		return nil, err
	}
	roster := make([]RosterEntry, 0, len(out.Data))
	for _, m := range out.Data {
		roster = append(roster, RosterEntry{
			ID:      m.ID,
			Name:    m.Name,
			Aliases: m.Meta.Llamaswap.Aliases,
			State:   m.Status.Value,
		})
	}
	return roster, nil
}

// ActivityTokens mirrors the nested token block. -1 means "not measured".
type ActivityTokens struct {
	CacheTokens     int     `json:"cache_tokens"`
	DraftTokens     int     `json:"draft_tokens"`
	DraftAccTokens  int     `json:"draft_acc_tokens"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// ActivityRow is one /api/metrics/activity record.
type ActivityRow struct {
	ID             int64          `json:"id"`
	Timestamp      string         `json:"timestamp"`
	Model          string         `json:"model"`
	ReqPath        string         `json:"req_path"`
	RespStatusCode int            `json:"resp_status_code"`
	Tokens         ActivityTokens `json:"tokens"`
	DurationMS     int64          `json:"duration_ms"`
	HasCapture     bool           `json:"has_capture"`
}

// Terminal reports whether the request reached an observable outcome. A row
// with no HTTP status was still in flight; when an epoch is sealed such rows
// are marked CENSORED, never failed.
func (r ActivityRow) Terminal() bool { return r.RespStatusCode >= 100 }

// Fingerprint identifies a row's CONTENT independent of its id. After a proxy
// restart the id space restarts at 1, so id equality proves nothing; comparing
// fingerprints at overlapping ids is what detects a restart that raced past the
// previous cursor.
func (r ActivityRow) Fingerprint() string {
	return strings.Join([]string{
		r.Timestamp, r.Model, r.ReqPath,
		strconv.Itoa(r.RespStatusCode), strconv.FormatInt(r.DurationMS, 10),
	}, "|")
}

// ActivityPage is one page of the activity ring.
type ActivityPage struct {
	Data       []ActivityRow `json:"data"`
	Page       int           `json:"page"`
	Limit      int           `json:"limit"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// ActivityOpts selects a page of the ring.
type ActivityOpts struct {
	Model string
	Page  int
	Limit int
	Sort  string
	Order string
}

// Activity fetches one page.
func (c *Client) Activity(ctx context.Context, opts ActivityOpts) (ActivityPage, error) {
	params := map[string]string{}
	if opts.Model != "" {
		params["model"] = opts.Model
	}
	if opts.Page > 0 {
		params["page"] = strconv.Itoa(opts.Page)
	}
	if opts.Limit > 0 {
		params["limit"] = strconv.Itoa(opts.Limit)
	}
	if opts.Sort != "" {
		params["sort"] = opts.Sort
	}
	if opts.Order != "" {
		params["order"] = opts.Order
	}
	var page ActivityPage
	if err := c.getJSON(ctx, "/api/metrics/activity", params, &page); err != nil {
		return ActivityPage{}, err
	}
	return page, nil
}

// Stats is GET /api/metrics/stats. TotalRequests is CUMULATIVE and survives
// ring eviction; it is the independent restart witness.
type Stats struct {
	TotalRequests     int64 `json:"total_requests"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	TotalCacheTokens  int64 `json:"total_cache_tokens"`
}

// Stats reads the cumulative counters.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	if err := c.getJSON(ctx, "/api/metrics/stats", nil, &s); err != nil {
		return Stats{}, err
	}
	return s, nil
}

// Slot is one llama-server slot. IsProcessing is the drain signal.
type Slot struct {
	ID           int  `json:"id"`
	NCtx         int  `json:"n_ctx"`
	IsProcessing bool `json:"is_processing"`
	IDTask       int  `json:"id_task"`
}

// Slots reads /upstream/{model}/slots. The status code is returned alongside
// the error so callers can distinguish 404 (endpoint absent — llama-server
// started without --slots, use the documented fallback) from 5xx / timeout
// (unobservable — fail closed and unload nothing).
//
// Callers MUST check /running first: any /upstream/{model}/* request
// AUTO-STARTS a stopped model, so a "probe" can trigger a multi-GB load.
func (c *Client) Slots(ctx context.Context, model string) ([]Slot, int, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/upstream/"+url.PathEscape(model)+"/slots", nil)
	if err != nil {
		return nil, status, err
	}
	if status < 200 || status >= 300 {
		return nil, status, &HTTPError{Status: status, Path: "/upstream/" + model + "/slots", Body: truncate(string(body), 200)}
	}
	var slots []Slot
	if err := json.Unmarshal(body, &slots); err != nil {
		return nil, status, fmt.Errorf("decode slots for %s: %w", model, err)
	}
	return slots, status, nil
}

// Props reads /upstream/{model}/props (same auto-start caveat as Slots).
func (c *Client) Props(ctx context.Context, model string) (map[string]any, int, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/upstream/"+url.PathEscape(model)+"/props", nil)
	if err != nil {
		return nil, status, err
	}
	if status < 200 || status >= 300 {
		return nil, status, &HTTPError{Status: status, Path: "/upstream/" + model + "/props", Body: truncate(string(body), 200)}
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, status, err
	}
	return out, status, nil
}

// UpstreamHealth probes a RUNNING model's own /health through the passthrough.
// Never call it for a model absent from /running (auto-start trap).
func (c *Client) UpstreamHealth(ctx context.Context, model string) (int, error) {
	status, _, err := c.do(ctx, http.MethodGet, "/upstream/"+url.PathEscape(model)+"/health", nil)
	return status, err
}

// UnloadModel unloads one model by CANONICAL id. Returns the HTTP status so a
// 404 can drive the legacy-route fallback.
func (c *Client) UnloadModel(ctx context.Context, model string) (int, error) {
	status, _, err := c.do(ctx, http.MethodPost, "/api/models/unload/"+url.PathEscape(model), nil)
	return status, err
}

// UnloadAll unloads EVERY model including keep-set residents. Non-selective:
// callers must apply the keep-set policy before reaching for it.
func (c *Client) UnloadAll(ctx context.Context) (int, error) {
	status, _, err := c.do(ctx, http.MethodPost, "/api/models/unload", nil)
	return status, err
}

// LegacyUnloadAll is the pre-v2xx GET /unload route, kept for version drift.
// Also non-selective.
func (c *Client) LegacyUnloadAll(ctx context.Context) (int, error) {
	status, _, err := c.do(ctx, http.MethodGet, "/unload", nil)
	return status, err
}

// RawEvent is one decoded SSE frame from /api/events.
type RawEvent struct {
	ReceivedAt time.Time
	Type       string
	Data       json.RawMessage
	// Text carries the decoded string payload for frames whose "data" field is
	// a JSON string (the logData shape), so callers don't re-unquote it.
	Text string
}

// DrainEvents connects to the SSE stream for a bounded window and returns the
// frames received. Foreground and time-boxed by design: this CLI never
// daemonizes a listener (house rule — no unattended watchers).
func (c *Client) DrainEvents(ctx context.Context, window time.Duration) ([]RawEvent, error) {
	if window <= 0 {
		window = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The stream never ends on its own; the per-request timeout must come from
	// the context, not the http.Client, or every drain returns an error.
	streamer := &http.Client{Transport: c.HTTP.Transport}
	resp, err := streamer.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil // window elapsed before the server answered: no frames, not an error
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Path: "/api/events"}
	}

	var events []RawEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		ev := RawEvent{ReceivedAt: time.Now()}
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			continue
		}
		ev.Type = envelope.Type
		ev.Data = envelope.Data
		var text string
		if json.Unmarshal(envelope.Data, &text) == nil {
			ev.Text = text
		}
		events = append(events, ev)
	}
	// A deadline-cancelled read is the expected exit, not a failure.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return events, err
	}
	return events, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
