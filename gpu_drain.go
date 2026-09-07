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
// The in-flight read itself lives in internal/seatload since 0.113.20 (the
// delegator's spread deal shares it): two-step — llama-swap's /running says
// whether the seat is loaded, ONLY a loaded seat is asked for /metrics or
// /slots through /upstream/<model>/… (that path loads a model on demand) — and
// alias-aware, because /running lists canonical ids while the harness binds
// seats by alias (the drain was a silent no-op on such a seat before).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// drainSeat polls until the seat reports zero in-flight requests on two
// consecutive reads (a request can land between a zero and the unload) or the
// deadline passes. It returns nil when drained; an error names what it last
// saw. every is the poll interval (the CLI uses 2 s; tests use milliseconds).
func drainSeat(ctx context.Context, client *http.Client, endpoint, model string, timeout, every time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	zeros := 0
	last := ""
	for {
		rd, err := seatload.Inflight(ctx, client, endpoint, model)
		n, loaded := rd.Inflight, rd.Loaded
		switch {
		case err != nil:
			last = err.Error()
			zeros = 0
		case !loaded && rd.Ambiguous:
			// The bare name is not in /running, the roster could not say what
			// id the seat is listed under, and /running is not empty: the seat
			// may be one of those entries mid-request. "Could not tell" is not
			// "idle" — keep polling, and name the cause at the deadline.
			last = fmt.Sprintf("cannot tell whether %s is loaded: roster unreadable (%v) and /running lists %d other model(s)", model, rd.RosterErr, rd.RunningOthers)
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
