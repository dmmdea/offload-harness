// Package hostsample publishes a coarse host CPU % and RAM used/total for
// /fleet/health. It is a background sampler like fleetnode's VRAM Sampler:
// the health handler only ever Loads the last value.
package hostsample

import (
	"context"
	"sync/atomic"
	"time"
)

type Sample struct {
	CPUPct      int // 0-100
	RAMUsedGiB  float64
	RAMTotalGiB float64
	At          time.Time
	Known       bool
}

// cpuTimes is one cumulative CPU-time reading (any unit; ratios only).
type cpuTimes struct{ idle, total float64 }

// cpuPct is busy % between two readings; -1 = no elapsed time (unknown).
func cpuPct(prev, cur cpuTimes) int {
	dt := cur.total - prev.total
	if dt <= 0 {
		return -1
	}
	busy := dt - (cur.idle - prev.idle)
	p := int(busy/dt*100 + 0.5)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

type Sampler struct{ v atomic.Value }

func (s *Sampler) Load() (Sample, bool) {
	x := s.v.Load()
	if x == nil {
		return Sample{}, false
	}
	return x.(Sample), true
}

// Start samples every interval. The first CPU % needs two readings, so the
// first publish happens at the second tick; RAM is read on every tick.
func Start(ctx context.Context, interval time.Duration) *Sampler {
	s := &Sampler{}
	go func() {
		prev, ok := readCPU()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, cok := readCPU()
			used, total, rok := readRAM()
			if !cok || !rok || !ok {
				prev, ok = cur, cok
				continue
			}
			if p := cpuPct(prev, cur); p >= 0 {
				s.v.Store(Sample{CPUPct: p, RAMUsedGiB: used, RAMTotalGiB: total, At: time.Now(), Known: true})
			}
			prev = cur
		}
	}()
	return s
}
