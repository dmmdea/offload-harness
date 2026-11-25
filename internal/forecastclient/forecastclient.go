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
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatwait"
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

// fenceWaitLeft is how long one call may wait at the GPU-lease fence in all. The passthrough STARTS the model when
// it is not loaded, so under a render or an exclusive hold the request waits for the card and, on exhaustion, returns
// the fence's *modelaffinity.LeaseError, which the pipeline defers as capacity.
func (c *Client) fenceWaitLeft() time.Duration {
	budget := c.http.Timeout
	if budget <= 0 {
		budget = fenceBudget
	}
	if c.fenceWaitSet && c.fenceWait < budget {
		budget = c.fenceWait
	}
	return budget
}

// reply is what one forecast got back from the upstream; resent says how often it was sent again ("" when it was not).
type reply struct {
	status int
	body   []byte
	cut    error // the read that was cut off mid-answer
	resent string
}

// silent reports a bare 5xx: llama-swap's answer when its connection to the model drops.
func (r reply) silent() bool { return r.status >= 500 && len(bytes.TrimSpace(r.body)) == 0 }

// exchange sends one forecast behind the fence, marked where other processes can see it (modelaffinity.Call), and sends
// it again when its model went away under it (seatwait.TakenAway, or an answer cut off mid-read). A re-send waits at the
// fence like a new call and stays inside the call's own timeout.
func (c *Client) exchange(ctx context.Context, model string, payload []byte) (reply, error) {
	// Target the subpath directly; the bare /upstream/<model> form 301-redirects.
	call := modelaffinity.Call{Kind: gpuactivity.KindForecast, Endpoint: c.base, Model: model, Path: "/v1/forecast", Fence: c.fenceWaitLeft()}
	again := seatwait.NewResend(c.http.Timeout)
	for {
		var rep reply
		var sentAt time.Time
		rctx, cancel := again.Bound(ctx)
		err := call.Run(rctx, func(url string) error {
			req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			sentAt = time.Now()
			resp, err := c.http.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			rep.status = resp.StatusCode
			rep.body, rep.cut = io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
			return nil
		})
		cancel()
		if err != nil {
			return reply{}, again.Err(err)
		}
		said := strings.TrimSpace(string(rep.body[:min(len(rep.body), 400)]))
		lost := rep.cut != nil || seatwait.TakenAway(rep.status, said)
		if !lost || !again.Again(ctx, sentAt) {
			rep.resent = again.Note()
			return rep, nil
		}
		log.Printf("forecastclient: %s went away under a forecast (HTTP %d %s); sending it again", model, rep.status, said)
	}
}

// Forecast posts payload (the server's request body, a JSON object) to the upstream `model` and
// returns the server's JSON answer verbatim. A 400 or 413 is ErrBadInput carrying the server's
// detail; an empty-body 5xx or a cut-off answer is ErrUpstreamVanished; a held card is the fence's
// lease refusal (modelaffinity.IsLeaseRefusal).
func (c *Client) Forecast(ctx context.Context, model string, payload []byte) (json.RawMessage, error) {
	rep, err := c.exchange(ctx, model, payload)
	if err != nil {
		return nil, err
	}
	b := rep.body
	if rep.cut != nil {
		return nil, fmt.Errorf("%w: forecast server %d: the answer was cut off mid-read (%w): the upstream was stopped or restarted under the call%s", ErrUpstreamVanished, rep.status, rep.cut, rep.resent)
	}
	if rep.status != http.StatusOK {
		switch {
		case rep.silent():
			return nil, fmt.Errorf("%w: forecast server %d (empty body): it was unloaded, swapped, restarted or crashed under the call%s", ErrUpstreamVanished, rep.status, rep.resent)
		case rep.status == http.StatusBadRequest || rep.status == http.StatusRequestEntityTooLarge:
			return nil, fmt.Errorf("%w: %s", ErrBadInput, refusalDetail(rep.status, b))
		}
		return nil, fmt.Errorf("forecast server %d: %s%s", rep.status, refusalDetail(rep.status, b), rep.resent)
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
