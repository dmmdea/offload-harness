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
// /slots) — at the seat's OWN address (the `proxy` /running reports for it),
// never through /upstream/<model>/…. That path is wrong twice over: it loads a
// model on demand, so probing it on an unloaded seat would do the exact thing
// a drain exists to avoid; and llama-swap counts every /upstream request as
// activity, so a gauge read through it resets the seat's idle timer — a
// reader that polls (the PAIR seat watcher, a status line, a drain) would keep
// an idle seat resident forever and defeat the 5-minute idle unload.
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
	"errors"
	"fmt"
	"io"
	"net"
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
	// Starting reports that llama-swap lists the seat in state `starting` (or
	// `stopping`): Loaded is true, Inflight is NOT known (0 here means "not
	// asked"), and the upstream was NOT probed — llama-swap would hold that
	// request until the load completes (register D-92). A load in progress is
	// work in flight for the drain and "not yet a target" for the spread deal.
	Starting bool
	// Proxy is the seat's own address as llama-swap's /running reports it —
	// the `proxy:` of the model entry that is running, after macro expansion.
	// It is what Inflight reads the gauge from. Empty when the seat is not
	// listed or the llama-swap build does not report it.
	Proxy string
}

// ErrNoSeatAddress is Inflight's answer for a LOADED seat whose /running entry
// carries no `proxy`: the gauge has nowhere to be read from except
// /upstream/<seat>/…, and that path resets the seat's idle timer (see the
// package comment), so the reader reports "could not read" instead.
var ErrNoSeatAddress = errors.New("llama-swap /running reports no proxy address for the seat; its gauge is read at the seat itself, never through /upstream (which resets the idle unload timer)")

// SeatURL resolves the address a seat's gauges are read at from the `proxy`
// llama-swap's /running reports for it. The proxy is written from llama-swap's
// point of view, so a loopback or unspecified host is re-pointed at the host
// of the llama-swap endpoint when that endpoint is not itself loopback (the
// seat and its llama-swap share a box). The port is never guessed: two seats
// may share one port, and only the one /running lists as running owns it now.
func SeatURL(endpoint, proxy string) (string, error) {
	p := strings.TrimSpace(proxy)
	if p == "" {
		return "", ErrNoSeatAddress
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("seat proxy %q is not an http(s) URL", proxy)
	}
	host := u.Hostname()
	if isLocalHost(host) {
		target := ""
		if e, eerr := url.Parse(swapclient.BaseURL(endpoint)); eerr == nil && e.Hostname() != "" && !isLocalHost(e.Hostname()) {
			target = e.Hostname()
		} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			target = "127.0.0.1"
		}
		if target != "" {
			if port := u.Port(); port != "" {
				u.Host = net.JoinHostPort(target, port)
			} else if strings.Contains(target, ":") {
				u.Host = "[" + target + "]"
			} else {
				u.Host = target
			}
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// isLocalHost reports a loopback or unspecified host (the forms a llama-swap
// `proxy:` uses for a seat on its own box).
func isLocalHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// Running answers the FIRST half of Inflight and stops there: what llama-swap's
// /running says about the seat (Loaded, Starting, Canonical, and the roster's
// own failure), with no /upstream read at all.
//
// It exists because a caller can need the seat's LOAD STATE without being
// allowed to touch the seat: `/upstream/<seat>/…` starts an unloaded model on
// demand (register C-05 — 54 measured status probes each blocked ~186 s
// because asking whether the seat was up STARTED it), so anything on a health
// path may read /running and nothing else. Inflight is this call plus the
// gauge read that only a LOADED seat can answer.
//
// Alias-awareness is the whole reason this is not two lines at the call site:
// /running lists CANONICAL ids while the harness binds seats by ALIAS, so a
// bare-name match reads a loaded seat as absent (the silent 0.113.16-19 drain
// defect). An unreadable roster falls back to the bare name and says so in
// RosterErr — never a refusal.
func Running(ctx context.Context, client *http.Client, endpoint, seat string) (Reading, error) {
	rd, _, err := running(ctx, client, endpoint, seat)
	return rd, err
}

// Inflight reads the seat named `seat` (id or alias) behind the llama-swap at
// endpoint. A metrics fetch that fails on a LOADED seat is an error — "could
// not read" must never pass as "idle".
func Inflight(ctx context.Context, client *http.Client, endpoint, seat string) (Reading, error) {
	base := strings.TrimRight(endpoint, "/")
	rd, done, err := running(ctx, client, endpoint, seat)
	if err != nil || done {
		return rd, err
	}
	// The gauge is read at the seat's own address, never through
	// /upstream/<seat>/…: llama-swap counts an /upstream request as activity,
	// so a polled read there would keep an idle seat loaded past its ttl.
	seatBase, err := SeatURL(base, rd.Proxy)
	if err != nil {
		return rd, fmt.Errorf("seat metrics: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, seatBase+"/metrics", nil)
	if err != nil {
		return rd, err
	}
	resp, err := client.Do(req)
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
		n, serr := slotsInflight(ctx, client, seatBase, resp.StatusCode)
		if serr != nil {
			return rd, serr
		}
		rd.Inflight, rd.Source = n, "slots"
		return rd, nil
	default:
		return rd, fmt.Errorf("seat metrics: status %d", resp.StatusCode)
	}
}

// running is the shared /running read behind Running and Inflight. `done`
// reports that the reading is FINAL — the seat is not listed, or it is
// starting/stopping — so an upstream gauge read would be either meaningless or
// a request llama-swap holds until the load completes.
func running(ctx context.Context, client *http.Client, endpoint, seat string) (Reading, bool, error) {
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
		return rd, true, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return rd, true, fmt.Errorf("llama-swap /running: %w", err)
	}
	var listed struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
			Proxy string `json:"proxy"`
		} `json:"running"`
	}
	derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&listed)
	resp.Body.Close()
	if derr != nil {
		return rd, true, fmt.Errorf("llama-swap /running: %w", derr)
	}
	for _, m := range listed.Running {
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
			rd.Proxy = m.Proxy
			// A seat that is STARTING or STOPPING is listed, but its upstream is
			// not there to ask: llama-swap holds `/upstream/<seat>/…` until the
			// load completes (4m08s on the 27B TP2 seat, 2026-09-11, llama-swap
			// log line 420399), which is longer than any drain window and is
			// exactly how the H-24 gate's first run aborted with "0 in flight
			// (confirming)". The reading says so instead of blocking, and the
			// caller polls /running until the state settles (register D-92).
			if st := strings.ToLower(m.State); st == "starting" || st == "stopping" {
				rd.Starting = true
				rd.Source = "running-state:" + st
			}
		} else {
			rd.RunningOthers++
		}
	}
	if !rd.Loaded {
		rd.Ambiguous = rd.RosterErr != nil && rd.RunningOthers > 0
		return rd, true, nil
	}
	return rd, rd.Starting, nil
}

// slotsInflight reads llama-server's GET /slots at the seat's own address
// (seatBase, from SeatURL — never /upstream) and counts the slots that are
// processing. A queued request (llama-server's deferred
// task queue) is not listed by /slots — it becomes a processing slot the
// instant one frees — which is why a drain asks for TWO consecutive idle reads
// before it calls the seat drained.
func slotsInflight(ctx context.Context, client *http.Client, seatBase string, metricsStatus int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, seatBase+"/slots", nil)
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
