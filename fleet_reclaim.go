package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The reclaimable-VRAM advertisement needs one fact the node can only learn by
// asking: is anything of OURS currently on the card? Two owners qualify —
// llama-swap, which holds every text/vision/STT seat, and the GPU lease, which a
// render job takes for the duration of its work.
//
// Getting this wrong in the permissive direction is the expensive mistake: if we
// believe nothing is loaded while a model actually is, that moment is recorded as
// the idle BASELINE, the baseline absorbs our own model, and the node then
// advertises less reclaimable VRAM forever. So an unreadable probe is reported as
// "cannot tell" and simply does not update anything.

// oursLoaded reports whether this harness holds RECLAIMABLE GPU memory. ok=false
// means the question could not be answered, which is never treated as "idle".
//
// "Reclaimable" excludes permanently resident seats. The measured topology keeps
// an embedder and a reranker co-resident on purpose (ttl 0 — no auto-unload), and
// unloading them defeats the reason they are resident: with them in the swapping
// tier, a single RAG query paid three full model loads. So they are not capacity a
// job can take — they belong in the baseline exactly like the desktop does.
//
// Without this distinction the feature would report "unknown" forever on precisely
// the nodes that are configured correctly, because a node with an always-resident
// seat never reaches "nothing of ours loaded".
func oursLoaded(cfg config.Config) (loaded bool, ok bool) {
	swapLoaded, swapOK := llamaSwapHasSwappable(cfg.Endpoint)
	leaseHeld := gpuLeaseHeld(cfg)
	switch {
	case leaseHeld:
		return true, true // a render owns the card; no ambiguity
	case swapOK:
		return swapLoaded, true
	default:
		return false, false
	}
}

// llamaSwapHasSwappable asks llama-swap what it currently has loaded and reports
// whether any SWAPPABLE seat is among them. /running is authoritative — it is the
// process that loaded them — and its per-seat ttl is what distinguishes the two
// kinds: ttl 0 means no auto-unload, i.e. a deliberately resident seat.
//
// The bounded error is in the safe direction for the common case and small in the
// other: a support seat configured with a TTL instead of 0 is counted as
// reclaimable, over-stating by the size of an embedder (~0.5 GiB).
func llamaSwapHasSwappable(endpoint string) (bool, bool) {
	base := strings.TrimRight(endpoint, "/")
	if i := strings.Index(base, "/v1"); i > 0 {
		base = base[:i]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/running", nil)
	if err != nil {
		return false, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var body struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
			TTL   int    `json:"ttl"`
		} `json:"running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, false
	}
	for _, m := range body.Running {
		// "stopped"/"stopping" seats hold no VRAM; anything else does.
		if s := strings.ToLower(m.State); s == "stopped" || s == "stopping" {
			continue
		}
		if m.TTL == 0 {
			continue // deliberately resident: part of the baseline, not capacity
		}
		return true, true
	}
	return false, true
}

// gpuLeaseHeld reports whether a render currently owns the single GPU slot. A
// failure to read the lease is reported as NOT held: the llama-swap answer still
// governs, and claiming "held" on an unreadable lease would suppress every
// baseline observation on a box whose lease dir is misconfigured.
func gpuLeaseHeld(cfg config.Config) bool {
	dir, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return false
	}
	return gpulease.InspectDir(dir).Held
}

// startReclaimTracking samples the idle baseline in the background and returns the
// accessor the health payload calls. Sampling is decoupled from /fleet/health on
// purpose: the handler must never block on an HTTP call to llama-swap.
func startReclaimTracking(ctx context.Context, cfg config.Config, snapshot func() (fleetnode.Snapshot, bool), every time.Duration) func(freeGiB, totalGiB float64) fleetnode.ReclaimVerdict {
	tracker := &fleetnode.BaselineTracker{}
	var lastLoaded, lastOK = false, false

	observe := func() {
		snap, have := snapshot()
		if !have {
			return
		}
		loaded, ok := oursLoaded(cfg)
		lastLoaded, lastOK = loaded, ok
		if !ok {
			return // cannot tell: never record a baseline on a guess
		}
		tracker.Observe(snap.TotalGiB-snap.FreeGiB, loaded, time.Now())
	}
	observe()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				observe()
			}
		}
	}()

	return func(freeGiB, totalGiB float64) fleetnode.ReclaimVerdict {
		if !lastOK {
			return fleetnode.ReclaimVerdict{
				Source: "unknown: could not determine whether this harness holds GPU memory (llama-swap unreachable and no lease readable)",
			}
		}
		return tracker.Verdict(totalGiB-freeGiB, lastLoaded)
	}
}
