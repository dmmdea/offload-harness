package main

// `gpu reserve --drain [--unload-seat]` and `gpu release --warm-seat`: the
// maintenance half of the lease (0.113.16).
//
// Taking a TEXT lease makes this node a non-target — the delegator skips it
// (health "lease"), the node refuses new dispatches (503, re-placeable) — but
// the seat can still be mid-request for work already placed. --drain waits for
// that work to finish before the holder touches the cards: it polls the agent
// seat's own counters through llama-swap and returns when nothing is running or
// waiting, or errors at the deadline (the lease is released on that path; a
// measurement must never start over a live request). --unload-seat then frees
// the cards through llama-swap's API, and `gpu release --warm-seat` (or the
// wrapper form's exit) loads the seat back so the next contract does not pay a
// cold start.
//
// The read is deliberately two-step: llama-swap's /running says whether the
// seat is loaded at all, and ONLY a loaded seat is asked for /metrics through
// /upstream/<model>/… — that path loads a model on demand, so probing it on an
// unloaded seat would do the exact thing a drain exists to avoid.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// seatInflightGauges are the counters that mean "a request is in flight" on
// the engines the harness fronts: vLLM (running + waiting) and llama-server
// (processing + deferred). Any other gauge is ignored.
var seatInflightGauges = []string{
	"vllm:num_requests_running", "vllm:num_requests_waiting",
	"llamacpp:requests_processing", "llamacpp:requests_deferred",
}

// seatInflight reads how many requests the seat holds right now. loaded=false
// means llama-swap does not list the model as running (idle by definition, and
// the upstream is NOT probed). A metrics fetch that fails on a loaded seat is
// an error — "could not read" must never pass as "idle".
func seatInflight(ctx context.Context, client *http.Client, endpoint, model string) (inflight int, loaded bool, err error) {
	base := strings.TrimRight(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/running", nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("llama-swap /running: %w", err)
	}
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&running)
	resp.Body.Close()
	if derr != nil {
		return 0, false, fmt.Errorf("llama-swap /running: %w", derr)
	}
	for _, m := range running.Running {
		if strings.EqualFold(m.Model, model) && m.State != "stopped" && m.State != "shutdown" {
			loaded = true
		}
	}
	if !loaded {
		return 0, false, nil
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/upstream/"+url.PathEscape(model)+"/metrics", nil)
	if err != nil {
		return 0, true, err
	}
	resp, err = client.Do(req)
	if err != nil {
		return 0, true, fmt.Errorf("seat metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, true, fmt.Errorf("seat metrics: status %d", resp.StatusCode)
	}
	return parseInflight(resp.Body), true, nil
}

// parseInflight sums the in-flight gauges out of a Prometheus text exposition.
func parseInflight(r io.Reader) int {
	total := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		for _, g := range seatInflightGauges {
			if !strings.HasPrefix(line, g) {
				continue
			}
			rest := line[len(g):]
			if rest == "" || (rest[0] != '{' && rest[0] != ' ') {
				continue // a different gauge sharing the prefix
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err == nil && v > 0 {
				total += int(v + 0.5)
			}
		}
	}
	return total
}

// drainSeat polls until the seat reports zero in-flight requests on two
// consecutive reads (a request can land between a zero and the unload) or the
// deadline passes. It returns nil when drained; an error names what it last
// saw. every is the poll interval (the CLI uses 2 s; tests use milliseconds).
func drainSeat(ctx context.Context, client *http.Client, endpoint, model string, timeout, every time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	zeros := 0
	last := ""
	for {
		n, loaded, err := seatInflight(ctx, client, endpoint, model)
		switch {
		case err != nil:
			last = err.Error()
			zeros = 0
		case !loaded:
			return nil // not loaded: nothing to drain
		case n == 0:
			zeros++
			if zeros >= 2 {
				return nil
			}
			last = "0 in flight (confirming)"
		default:
			zeros = 0
			last = fmt.Sprintf("%d in flight", n)
			if out != nil {
				fmt.Fprintf(out, "gpu reserve: draining %s: %s\n", model, last)
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("drain of %s did not finish within %s (last: %s)", model, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// unloadSeat frees the seat through llama-swap: the current API first, the
// legacy GET as a fallback for an older llama-swap.
func unloadSeat(ctx context.Context, client *http.Client, endpoint, model string) error {
	base := strings.TrimRight(endpoint, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/models/unload/"+url.PathEscape(model), nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/unload?model="+url.QueryEscape(model), nil)
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("unload %s: %w", model, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unload %s: status %d", model, resp.StatusCode)
	}
	return nil
}

// warmSeat loads the seat back through llama-swap's on-demand path (/upstream)
// by asking the upstream for its health; llama-swap loads the model to answer.
// The client's timeout bounds the load (a 27B tp2 seat takes ~3 min).
func warmSeat(ctx context.Context, client *http.Client, endpoint, model string) error {
	base := strings.TrimRight(endpoint, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/upstream/"+url.PathEscape(model)+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("warm %s: %w", model, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("warm %s: status %d", model, resp.StatusCode)
	}
	return nil
}

// seatTarget resolves the seat the maintenance verbs act on: the config's
// llama-swap endpoint and agent seat alias.
func seatTarget(cfg config.Config) (endpoint, model string, err error) {
	endpoint = strings.TrimSpace(cfg.Endpoint)
	model = strings.TrimSpace(cfg.AgentPlannerModel(""))
	if endpoint == "" || model == "" {
		return "", "", errors.New("drain/unload need the config's endpoint and agent seat (agent_model)")
	}
	return endpoint, model, nil
}

// maintenanceClient is the HTTP client the drain/unload/warm verbs share:
// loopback llama-swap, generous timeout so a warm-back (a model load) fits.
var maintenanceClient = &http.Client{Timeout: 15 * time.Minute}

// maintainSeat runs the --drain / --unload-seat steps against the config's
// seat. Errors are returned as-is; the caller decides what to do with the lease.
func maintainSeat(cfg config.Config, drain bool, drainTimeout time.Duration, unload bool) error {
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if drain {
		if err := drainSeat(ctx, maintenanceClient, endpoint, model, drainTimeout, 2*time.Second, os.Stderr); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: %s drained (no request in flight)\n", model)
	}
	if unload {
		if err := unloadSeat(ctx, maintenanceClient, endpoint, model); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: %s unloaded\n", model)
	}
	return nil
}

// warmBack reloads the config's seat and reports the outcome on stderr; a
// failed warm-back is loud but never fatal (the lease release must still run).
func warmBack(cfg config.Config) {
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return
	}
	if err := warmSeat(context.Background(), maintenanceClient, endpoint, model); err != nil {
		fmt.Fprintf(os.Stderr, "gpu: warm-back of %s failed: %v\n", model, err)
		return
	}
	fmt.Fprintf(os.Stderr, "gpu: %s warmed back\n", model)
}
