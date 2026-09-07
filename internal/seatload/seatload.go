// Package seatload reads how many requests an agent seat holds RIGHT NOW,
// through llama-swap. Two callers share it and must agree on the answer:
//
//   - `gpu reserve --drain` (gpu_drain.go): a measurement must never start over
//     a live request, so the drain polls this until two consecutive zero reads;
//   - the delegator's spread deal (internal/delegate, 0.113.20): a local seat
//     that already holds a request loses its rotation slot to a remote with
//     room, so K delegating sessions do not stack on one seat.
//
// The read is deliberately two-step: llama-swap's /running says whether the
// seat is loaded at all, and ONLY a loaded seat is asked for /metrics (or
// /slots) through /upstream/<model>/… — that path loads a model on demand, so
// probing it on an unloaded seat would do the exact thing a drain exists to
// avoid.
//
// /running lists CANONICAL ids, while the harness binds seats by ALIAS on the
// reference deployment (agent-pool -> qwen3.8-27b-vllm, offload-e4b ->
// gemma-4-e4b). Matching /running by the configured name therefore read an
// alias-bound seat as "not loaded" and the drain returned at once on a seat
// that could be mid-request (silent from 0.113.16 to 0.113.19 on the box whose
// seat is alias-bound; the box whose seat is bound by its id was unaffected,
// which is why the live proofs passed). Inflight resolves the name through the
// roster first (swapclient.Roster.Canonical) and matches /running by BOTH the
// canonical id and the given name; when the roster cannot be read it falls
// back to the bare name — the pre-0.113.20 behaviour, never a refusal.
package seatload

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

	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// InflightGauges are the counters that mean "a request is in flight" on the
// engines the harness fronts: vLLM (running + waiting) and llama-server
// (processing + deferred). Any other gauge is ignored.
var InflightGauges = []string{
	"vllm:num_requests_running", "vllm:num_requests_waiting",
	"llamacpp:requests_processing", "llamacpp:requests_deferred",
}

// rosterTimeout bounds the one /v1/models read that resolves an alias. A
// caller's ctx deadline still wins when shorter.
const rosterTimeout = 3 * time.Second

// Reading is one observation of a seat.
type Reading struct {
	// Inflight is the number of requests the seat holds (running + waiting).
	// Meaningful only when Loaded.
	Inflight int
	// Loaded reports whether llama-swap lists the seat as running. false =
	// idle by definition, and the upstream was NOT probed.
	Loaded bool
	// Canonical is the roster id the name resolved to ("" when the roster
	// could not be read or does not serve the name).
	Canonical string
	// Source names what answered the count: "metrics" (vLLM/llama-server
	// exposition), "slots" (llama-server /slots), or "" when not loaded.
	Source string
	// RosterErr is the roster read's failure, when it failed. The reading then
	// matched /running by the bare name only, which cannot see an alias-bound
	// seat listed under its id.
	RosterErr error
	// RunningOthers is how many models /running listed that did NOT match the
	// seat. With RosterErr set and Loaded false, a non-zero value means the
	// seat may be one of them under a name this reading could not resolve.
	RunningOthers int
	// Ambiguous is exactly that case: not loaded by the bare name, the roster
	// unreadable, and /running not empty. "Could not tell" must never pass as
	// "idle" where idleness is load-bearing (the drain); a caller for whom the
	// reading is only an optimisation (the spread deal) may treat it as idle
	// and say so.
	Ambiguous bool
}

// Inflight reads the seat named `seat` (id or alias) behind the llama-swap at
// endpoint. A metrics fetch that fails on a LOADED seat is an error — "could
// not read" must never pass as "idle".
func Inflight(ctx context.Context, client *http.Client, endpoint, seat string) (Reading, error) {
	base := strings.TrimRight(endpoint, "/")
	rd := Reading{}
	names := []string{seat}
	rctx, cancel := context.WithTimeout(ctx, rosterTimeout)
	roster, rerr := swapclient.FetchRoster(rctx, endpoint, rosterTimeout)
	cancel()
	if rerr != nil {
		rd.RosterErr = rerr
	} else if id, ok := roster.Canonical(seat); ok {
		rd.Canonical = id
		if !strings.EqualFold(id, seat) {
			names = append(names, id)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/running", nil)
	if err != nil {
		return rd, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return rd, fmt.Errorf("llama-swap /running: %w", err)
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
		return rd, fmt.Errorf("llama-swap /running: %w", derr)
	}
	for _, m := range running.Running {
		if m.State == "stopped" || m.State == "shutdown" {
			continue
		}
		matched := false
		for _, n := range names {
			if strings.EqualFold(m.Model, n) {
				matched = true
			}
		}
		if matched {
			rd.Loaded = true
		} else {
			rd.RunningOthers++
		}
	}
	if !rd.Loaded {
		rd.Ambiguous = rd.RosterErr != nil && rd.RunningOthers > 0
		return rd, nil
	}
	// The upstream path is addressed by the name the caller bound (llama-swap
	// resolves aliases there); the canonical id would work too, but the bound
	// name is what every other harness call uses, so a proxy rule keyed on it
	// behaves the same here.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/upstream/"+url.PathEscape(seat)+"/metrics", nil)
	if err != nil {
		return rd, err
	}
	resp, err = client.Do(req)
	if err != nil {
		return rd, fmt.Errorf("seat metrics: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		rd.Inflight, rd.Source = ParseInflight(resp.Body), "metrics"
		return rd, nil
	case http.StatusNotImplemented, http.StatusNotFound:
		// llama-server answers 501 (older builds 404) when it runs WITHOUT
		// --metrics — every llama.cpp seat on this fleet does (2026-09-06: the
		// Lenovo's drain timed out on a warm seat, "seat metrics: status 501").
		// Its /slots endpoint is on by default and reports per-slot
		// is_processing, which is the in-flight count for a slot-based server.
		// Only these two statuses fall back: a 500 or a timeout is "could not
		// read" and must never pass as idle.
		n, serr := slotsInflight(ctx, client, base, seat, resp.StatusCode)
		if serr != nil {
			return rd, serr
		}
		rd.Inflight, rd.Source = n, "slots"
		return rd, nil
	default:
		return rd, fmt.Errorf("seat metrics: status %d", resp.StatusCode)
	}
}

// slotsInflight reads llama-server's GET /slots through llama-swap and counts
// the slots that are processing. A queued request (llama-server's deferred
// task queue) is not listed by /slots — it becomes a processing slot the
// instant one frees — which is why a drain asks for TWO consecutive idle reads
// before it calls the seat drained.
func slotsInflight(ctx context.Context, client *http.Client, base, seat string, metricsStatus int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/upstream/"+url.PathEscape(seat)+"/slots", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("seat slots: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("seat metrics: status %d (no --metrics) and seat slots: status %d", metricsStatus, resp.StatusCode)
	}
	n, perr := ParseSlotsInflight(io.LimitReader(resp.Body, 4<<20))
	if perr != nil {
		return 0, fmt.Errorf("seat slots: %w", perr)
	}
	return n, nil
}

// ParseSlotsInflight counts `is_processing` slots in a llama-server /slots
// body. A body that is not an array is an error, never zero in flight.
func ParseSlotsInflight(r io.Reader) (int, error) {
	var slots []struct {
		Processing bool `json:"is_processing"`
	}
	if err := json.NewDecoder(r).Decode(&slots); err != nil {
		return 0, err
	}
	n := 0
	for _, s := range slots {
		if s.Processing {
			n++
		}
	}
	return n, nil
}

// ParseInflight sums the in-flight gauges of a Prometheus exposition. A
// `_total` counter that merely shares a gauge's prefix is not counted.
func ParseInflight(r io.Reader) int {
	total := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		for _, g := range InflightGauges {
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
