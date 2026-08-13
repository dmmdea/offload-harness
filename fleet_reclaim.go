package main

import (
	"context"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"llamaswap-pp-cli/pkg/llamaswap"
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
// whether any RECLAIMABLE seat is among them. /running is authoritative for WHAT
// is loaded — it is the process that loaded them — but NOT for which of them are
// deliberately resident.
//
// That distinction used to be read off each row's `ttl`, and the server misreports
// it: a seat configured `ttl: -1` (never unload) is published on /running as
// `ttl: 0` (verified live on llama-swap v249 — both mem0-stack seats read ttl:0
// there today). The old rule happened to survive that because it also treated 0 as
// resident, but it was resting on a value the server is known to get wrong, and it
// mis-filed the opposite case: a support seat given a real TTL was counted as
// reclaimable, over-stating capacity by the size of an embedder.
//
// The keep-set now comes from the CONFIG instead — pkg/llamaswap reads the
// llama-swap YAML (ttl:-1 / ttl:0 seats, plus their aliases) and never the API.
// Matching is by the canonical id, which is exactly what /running reports and what
// the YAML keys each seat by.
func llamaSwapHasSwappable(endpoint string) (bool, bool) {
	c, err := swapclient.New(endpoint, 3*time.Second)
	if err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	running, err := c.Running(ctx)
	if err != nil {
		return false, false
	}
	keepSet := c.KeepSet()
	return anyReclaimable(running, func(id string) bool {
		_, protected := c.IsProtected(id)
		return protected
	}, len(keepSet.Members) > 0), true
}

// anyReclaimable classifies a /running snapshot. Split out from the transport so
// the classification — the part that has been wrong — is directly testable.
//
// keepSetKnown=false means no llama-swap YAML and no keep-set config could be read
// on this box, so "which seats are deliberately resident" has no config answer. It
// falls back to the server's ttl field rather than to "nothing is protected":
// on a node whose config simply is not where the loader looks, the permissive
// answer would fold a resident embedder into the idle BASELINE and make the node
// under-advertise reclaimable VRAM forever — the exact failure this file exists to
// avoid, arrived at from the other side.
func anyReclaimable(running []llamaswap.RunningModel, protected func(string) bool, keepSetKnown bool) bool {
	for _, m := range running {
		// "stopped"/"stopping" seats hold no VRAM; anything else does.
		if s := strings.ToLower(m.State); s == "stopped" || s == "stopping" {
			continue
		}
		if keepSetKnown {
			if protected(m.ID) {
				continue // deliberately resident: part of the baseline, not capacity
			}
		} else if m.TTL == 0 {
			continue
		}
		return true
	}
	return false
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
