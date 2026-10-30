// Package forecastclient calls the Timestral forecast server (internal/forecastserve) over
// llama-swap's /upstream/<model>/v1/forecast passthrough: one JSON object in, one JSON object
// out. The payload is the server's own request body, so this client reads none of it; it fences
// the call behind the GPU lease, types the failures the pipeline files differently, and returns
// the answer bytes untouched. Pure net/http; no cgo, no cloud.
package forecastclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// ErrBadInput marks a request the server refused as given: a 400 (the model refuses the shape,
// the values or the horizon) or a 413 (past the body or point caps). The wrapped message carries
// the server's own detail. It is the caller's request that is wrong, so no node and no retry
// changes it; tell it from every other failure with errors.Is.
var ErrBadInput = errors.New("forecast input refused")

// ErrUpstreamVanished marks a call whose upstream went away under it: an empty-body 5xx (llama-swap's
// bare answer when its connection to the model drops) or an answer cut off mid-read. An unload, a
// swap, a restart and a crash all look like that, so it is a failed call a caller can retry, never a
// verdict about the series.
var ErrUpstreamVanished = errors.New("forecast upstream: the upstream vanished mid-request")

// fenceBudget is how long a call waits for a fenced card when the client carries no timeout of its own.
const fenceBudget = 120 * time.Second

// maxAnswer bounds what is read back: a 4,096-step, 21-level, multi-series answer is megabytes of
// JSON, never more than the server's own 64 MiB request cap by a wide margin.
const maxAnswer = 128 << 20

// serverError is the {"error","detail"} body the forecast server answers a refusal with.
type serverError struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// Client posts forecast payloads to the forecast server through llama-swap on base (e.g.
// http://127.0.0.1:11436). The server is a threaded HTTP server that batches inside one request, so
// unlike whisper-server there is no single-slot lock, and no unload after a burst: the seat's ttl
// frees the card.
type Client struct {
	base string
	http *http.Client
	// fenceWait is the budget for waiting out a fenced card when fenceWaitSet; see WithFenceWait.
	fenceWait    time.Duration
	fenceWaitSet bool
}

// New builds a client. timeout bounds one request, the cold load of the model included.
func New(base string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

// WithFenceWait sets how long a call waits for a card a render holds and returns the client, so it
// chains at construction: New(...).WithFenceWait(p.gpuWait()). The client's timeout only caps it.
// Zero is a value, not "unset": gpu_wait_ms 0 is a single inspection of the lease.
func (c *Client) WithFenceWait(d time.Duration) *Client {
	c.fenceWait, c.fenceWaitSet = d, true
	return c
}

// fencedURL builds the forecast upstream's URL behind the GPU-lease fence. The passthrough STARTS the
// model when it is not loaded, so under a render or an exclusive hold the request waits for the card
// and, on exhaustion, returns the fence's *modelaffinity.LeaseError, which the pipeline defers as capacity.
func (c *Client) fencedURL(ctx context.Context, model, path string) (string, error) {
	budget := c.http.Timeout
	if budget <= 0 {
		budget = fenceBudget
	}
	if c.fenceWaitSet && c.fenceWait < budget {
		budget = c.fenceWait
	}
	return modelaffinity.AwaitUpstream(ctx, c.base, model, path, time.Now().Add(budget))
}

// Forecast posts payload (the server's request body, a JSON object) to the upstream `model` and
// returns the server's JSON answer verbatim. A 400 or 413 is ErrBadInput carrying the server's
// detail; an empty-body 5xx or a cut-off answer is ErrUpstreamVanished; a held card is the fence's
// lease refusal (modelaffinity.IsLeaseRefusal).
func (c *Client) Forecast(ctx context.Context, model string, payload []byte) (json.RawMessage, error) {
	// Target the subpath directly; the bare /upstream/<model> form 301-redirects.
	url, err := c.fencedURL(ctx, model, "/v1/forecast")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode != http.StatusOK {
		switch {
		case rerr != nil:
			return nil, fmt.Errorf("%w: forecast server %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call", ErrUpstreamVanished, resp.StatusCode, rerr)
		case resp.StatusCode >= 500 && len(bytes.TrimSpace(b)) == 0:
			return nil, fmt.Errorf("%w: forecast server %d (empty body): it was unloaded, swapped, restarted or crashed under the call", ErrUpstreamVanished, resp.StatusCode)
		case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
			return nil, fmt.Errorf("%w: %s", ErrBadInput, refusalDetail(resp.StatusCode, b))
		}
		return nil, fmt.Errorf("forecast server %d: %s", resp.StatusCode, refusalDetail(resp.StatusCode, b))
	}
	if rerr != nil {
		return nil, fmt.Errorf("%w: forecast server %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call", ErrUpstreamVanished, resp.StatusCode, rerr)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("forecastclient: the answer is not JSON (%d bytes)", len(b))
	}
	return json.RawMessage(b), nil
}

// refusalDetail is the server's {"detail"} when the body carries one, else the first bytes of the body.
func refusalDetail(status int, body []byte) string {
	var se serverError
	if json.Unmarshal(body, &se) == nil && se.Detail != "" {
		if se.Error != "" {
			return se.Error + ": " + se.Detail
		}
		return se.Detail
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 400 {
		s = s[:400]
	}
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return s
}
