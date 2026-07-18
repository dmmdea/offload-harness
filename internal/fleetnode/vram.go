// Package fleetnode implements the node side of fleet-dispatcher CONTRACT.md
// v2: health / ack-then-poll dispatch / job status, with measured VRAM
// footprints. See docs/superpowers/specs/2026-07-17-fleet-node-server-design.md.
//
// vram.go is the two-source VRAM sampler's portable half: the nvidia-smi
// global snapshot (feeds /fleet/health every 2s) and the pure helper
// ParseSmiMemory. The Windows-only PDH per-process source lives in
// vram_windows.go.
package fleetnode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ParseSmiMemory parses `nvidia-smi --query-gpu=memory.total,memory.used
// --format=csv,noheader,nounits` output ("16384, 1234", MiB) into GiB values.
// Whitespace and CRLF are tolerated (nvidia-smi emits \r\n on Windows); on a
// multi-GPU box the first line (GPU 0) wins. A total <= 0 or a negative used
// is an error: the contract treats vram_total_gb <= 0 as a failed probe, so a
// zero-total parse must never become a publishable snapshot.
func ParseSmiMemory(out string) (totalGiB, usedGiB float64, err error) {
	line := out
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, 0, fmt.Errorf("nvidia-smi memory query: empty output")
	}
	fields := strings.Split(line, ",")
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("nvidia-smi memory query: want 2 CSV fields, got %d in %q", len(fields), line)
	}
	totalMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("nvidia-smi memory.total %q: %w", strings.TrimSpace(fields[0]), err)
	}
	usedMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("nvidia-smi memory.used %q: %w", strings.TrimSpace(fields[1]), err)
	}
	if totalMiB <= 0 {
		return 0, 0, fmt.Errorf("nvidia-smi memory.total %v MiB is not a working GPU (contract: total <= 0 = failed probe)", totalMiB)
	}
	if usedMiB < 0 {
		return 0, 0, fmt.Errorf("nvidia-smi memory.used %v MiB is negative", usedMiB)
	}
	return totalMiB / 1024, usedMiB / 1024, nil
}

// Snapshot is the cached global VRAM state /fleet/health reads. GiB = 2^30,
// per the contract.
type Snapshot struct {
	TotalGiB float64
	FreeGiB  float64
	At       time.Time
}

// Sampler publishes the latest good Snapshot via an atomic.Value so the health
// handler never blocks on (or spawns) nvidia-smi.
type Sampler struct {
	snap atomic.Value // Snapshot
}

// Load returns the latest snapshot. ok is false until a sample has succeeded —
// callers must treat that as "probe not working", never as zero VRAM.
func (s *Sampler) Load() (Snapshot, bool) {
	v := s.snap.Load()
	if v == nil {
		return Snapshot{}, false
	}
	return v.(Snapshot), true
}

func (s *Sampler) sample(run func() (string, error)) {
	out, err := run()
	if err != nil {
		return // keep the last good snapshot
	}
	total, used, err := ParseSmiMemory(out)
	if err != nil {
		return // ditto — never publish a bad parse
	}
	s.snap.Store(Snapshot{TotalGiB: total, FreeGiB: total - used, At: time.Now()})
}

// StartGlobalSampler samples run() (an injected nvidia-smi invocation) once
// synchronously — so a successful probe is Load-able the moment this returns —
// then keeps refreshing every interval until ctx is done. Runner or parse
// failures leave the previous snapshot in place: the sampler degrades to stale
// data, never to zeros. Staleness is bounded downstream — the health handler
// refuses (503) any snapshot older than maxSnapshotAge, so a persistently
// failing nvidia-smi (driver reset) cannot serve hours-stale 200s.
func StartGlobalSampler(ctx context.Context, interval time.Duration, run func() (string, error)) *Sampler {
	s := &Sampler{}
	s.sample(run)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sample(run)
			}
		}
	}()
	return s
}
