package main

import (
	"context"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/displaywatch"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// startDisplayWatch starts the display layer's post-admission guard inside fleet-serve (ADR 0075) and
// returns the function that stops it and waits for it to finish.
//
// fleet-serve is where it lives because it is the one always-on process of a node: it already holds the
// 2 s device sampler the floor reads (so the check execs no nvidia-smi of its own), it runs in the
// console session where presence in `auto` mode can read the desk, and there is exactly one of it. The
// MCP server is one process per editor session and exists only while an editor is open.
//
// It is inert (the returned stop does nothing) on a box with no display layer that carries a desktop
// guard, and when display_watch_sec is negative. While no display twin is loaded it costs one local GET
// per period.
func startDisplayWatch(ctx context.Context, cfg config.Config, sampler *fleetnode.Sampler) func() {
	devices := func() ([]gpuprobe.Device, time.Time, bool) {
		snap, ok := sampler.Load()
		if !ok {
			return nil, time.Time{}, false
		}
		return snap.Devices, snap.At, true
	}
	return startDisplayWatchWith(ctx, cfg, displaywatch.Production(cfg, devices))
}

// startDisplayWatchWith is startDisplayWatch over the caller's readings and writes, so a test drives the
// same start and stop the node runs.
func startDisplayWatchWith(ctx context.Context, cfg config.Config, deps displaywatch.Deps) func() {
	w := displaywatch.New(cfg, deps)
	if w == nil {
		return func() {}
	}
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		w.RunEvery(wctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}
