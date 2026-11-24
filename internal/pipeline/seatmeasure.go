package pipeline

// The agent seat measures itself (internal/seatmeasure).
//
// The seat's host need was two typed config numbers (agent_seat_host_ram_gib, agent_seat_host_commit_gib). Every cold load of the
// seat that this harness starts now reads what it cost the host, and the figures are kept in the footprint store beside the
// renders' host peaks: key = the seat's model name as the config binds it, task "agent-seat-load", the working set the load
// took from the available memory in the resident slot and the commit charge it added in the private slot, so the store's
// 20-run window and 90th percentile apply unchanged. hostneed.SeatNeedMeasured turns them into the need the three callers
// judge a load by (SeatNeedFor), the config figures being the seed until three loads exist.
//
// A measurement is a side effect of a load and never part of it: nothing here returns an error, delays the load past the 0.6 s
// the baseline takes, or can panic out (seatLoadMeter.end recovers). A load is recorded only when this call started it (the seat had no
// row in /running and no other model was mid-swap when the call began), it completed, the result is clean and no lease that
// declares host RAM was live at the start or the end: such a lane's own growth is in the same counters.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/seatmeasure"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

const (
	// seatLoadTask is the task label of the seat's cold-load measurements in the footprint store.
	seatLoadTask = "agent-seat-load"
	// seatMeasureEvery is the sampling interval: 500 ms is 560 to 800 samples over the 281 to 399 s loads measured on the reference box.
	seatMeasureEvery = 500 * time.Millisecond
	// seatLoadTail bounds how long a measurement follows a load that outlived the warm-up's budget, seatLoadTailPoll is how often
	// it asks whether the seat is ready, and seatLoadTailMisses how many unreadable answers in a row end it. Chosen: twice the
	// slowest cold load measured on the reference box (399 s) rounded up; a second, so a short contract's process is still alive
	// when the seat turns ready; and a handful of misses, since llama-swap answers /running in milliseconds when it is up.
	seatLoadTail       = 15 * time.Minute
	seatLoadTailPoll   = time.Second
	seatLoadTailMisses = 5
	// seatLoadSameWindow: two recordings of one seat that end within this window are one load seen by several contracts (the
	// window internal/seatrate merges cold-load observations in): a seat cannot unload and load again in ten seconds.
	seatLoadSameWindow = 10 * time.Second
)

// seatMeasureCfg is the config the loads started through warmSeatWith are measured under: the one the process built its pipeline
// with. warmSeatWith has no config of its own, and both contract doors and the kv-slot rewarm call it.
var seatMeasureCfg atomic.Pointer[config.Config]

func registerSeatMeasure(cfg config.Config) { seatMeasureCfg.Store(&cfg) }

// seatRecordMu serialises the same-load check and the record, so two contracts that finish together record once.
var seatRecordMu sync.Mutex

// seatLoadMeter is the measurement of one warm-up. The zero value measures nothing.
type seatLoadMeter struct{ finish func(loaded bool) }

// begin starts the measurement when seat is the registered config's agent seat.
func (s *seatLoadMeter) begin(seat string) {
	if cfg := seatMeasureCfg.Load(); cfg != nil {
		s.finish = BeginSeatLoadMeasure(*cfg, seat)
	}
}

// end finishes it; loaded says the load completed.
func (s *seatLoadMeter) end(loaded bool) {
	if s.finish != nil {
		s.finish(loaded)
	}
}

// endWhenReady is end for a load that outlived the warm-up's budget: it returns at once and finishes the measurement from a
// goroutine, as loaded when the seat reads ready and as not loaded when the seat's row is gone (the load died), the seat's state
// cannot be read seatLoadTailMisses times running, or seatLoadTail has passed. The contract is not held: a process that ends
// before the seat is ready records nothing, which loses one sample and nothing else.
func (s *seatLoadMeter) endWhenReady(endpoint, seat string) {
	finish := s.finish
	if finish == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), seatLoadTail)
		defer cancel()
		misses := 0
		for {
			rd, err := seatload.Running(ctx, warmClient, endpoint, seat)
			switch {
			case err == nil && rd.Loaded && !rd.Starting: // a row that reads starting is listed as loaded too (seatload.Reading)
				finish(true)
				return
			case err == nil && !rd.Loaded && !rd.Ambiguous:
				finish(false) // no row: the load ended without a ready seat
				return
			case err != nil:
				if misses++; misses >= seatLoadTailMisses {
					finish(false)
					return
				}
			default:
				misses = 0
			}
			if seatwait.Sleep(ctx, seatLoadTailPoll) != nil {
				finish(false)
				return
			}
		}
	}()
}

// BeginSeatLoadMeasure starts measuring a cold load of seat that the caller is about to start, and returns the function that ends
// the measurement (loaded = the load completed). The caller has already established that the seat is absent. For a seat that is
// not cfg's agent seat, a platform with no host-memory reader, a lease directory that cannot be read, or a lane that declared host
// RAM and is live, the returned function does nothing. Never nil, never panics.
func BeginSeatLoadMeasure(cfg config.Config, seat string) (finish func(loaded bool)) {
	nop := func(bool) {}
	defer func() {
		if recover() != nil {
			finish = nop
		}
	}()
	model := strings.TrimSpace(cfg.AgentPlannerModel(""))
	if model == "" || !strings.EqualFold(strings.TrimSpace(seat), model) || !gpuprobe.HostMemorySupported {
		return nop
	}
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil || hostLaneLive(m) {
		return nop
	}
	run := seatmeasure.Begin(gpuprobe.ReadHostMemory, seatMeasureEvery)
	return func(loaded bool) {
		defer func() { _ = recover() }()
		res := run.Finish(loaded)
		if !loaded || !res.Clean || hostLaneLive(m) {
			return
		}
		recordSeatLoad(cfg, model, res)
	}
}

// hostLaneLive says a lease that declares host RAM is live (the test SeatColdLoadAdmission applies).
func hostLaneLive(m *gpulease.Manager) bool {
	for _, l := range m.Leases() {
		if l.DeclaresHostRAM() {
			return true
		}
	}
	return false
}

// recordSeatLoad keeps a clean result in the footprint store, once per load.
func recordSeatLoad(cfg config.Config, model string, res seatmeasure.Result) {
	store := seatStore(cfg)
	if store == nil {
		return
	}
	seatRecordMu.Lock()
	defer seatRecordMu.Unlock()
	if hp, ok := store.HostPeak(model, "", seatLoadTask); ok && time.Since(hp.Updated).Abs() <= seatLoadSameWindow {
		return // the load another contract (or process) already recorded
	}
	store.RecordHost(model, "", seatLoadTask, res.CommitPeakGiB, res.TouchedPeakGiB)
}

// seatStore opens the footprint store the config names, fresh: a process that only reads or records the seat's loads holds no
// stale copy of another process's window.
func seatStore(cfg config.Config) *fleetnode.Footprints {
	path := (&Pipeline{cfg: cfg}).footprintsPath()
	if path == "" {
		return nil
	}
	return fleetnode.OpenFootprints(path)
}

// SeatNeedFor is the host need of a cold load of cfg's agent seat: hostneed.SeatNeedMeasured over what the footprint store holds of
// its measured loads, and exactly hostneed.SeatNeed(cfg) for a seat that has none. runs is how many loads are recorded.
func SeatNeedFor(cfg config.Config) (need hostneed.Need, runs int) {
	if model := strings.TrimSpace(cfg.AgentPlannerModel("")); model != "" {
		if store := seatStore(cfg); store != nil {
			if hp, ok := store.HostPeak(model, "", seatLoadTask); ok {
				return hostneed.SeatNeedMeasured(cfg, hp.ResidentGiB, hp.PrivateGiB, hp.Runs), hp.Runs
			}
		}
	}
	return hostneed.SeatNeed(cfg), 0
}
