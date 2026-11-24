package pipeline

// The agent seat measures itself (internal/seatmeasure).
//
// The seat's host need was two typed config numbers (agent_seat_host_ram_gib, agent_seat_host_commit_gib). Every cold load of the
// seat that this harness starts now reads what it cost the host, and the figures are kept in the footprint store beside the
// renders' host peaks: key = the seat's model name as the config binds it, the model id that name resolved to when the load was
// measured (the quant slot) and task "agent-seat-load", the working set the load took from the available memory in the resident
// slot and the commit charge it added in the private slot, so the store's 20-run window and 90th percentile apply unchanged.
// hostneed.SeatNeedMeasured turns them into the need the three callers judge a load by (SeatNeedFor), the config figures being
// the seed until three loads exist. The model id is in the key because a seat's name outlives its model: a name bound to another
// model starts a window of its own, with the seed as its floor again, and the reader takes the key measured last.
//
// A measurement is a side effect of a load and never part of it: nothing here returns an error, delays the load past the 0.6 s
// the baseline takes, or can panic out (seatLoadMeter.end recovers). A load is recorded only when this call started it (the seat had no
// row in /running and no other model was mid-swap, read again just before the warm request goes out), it completed, the result is
// clean, no lease that declares host RAM was live at the start or the end (such a lane's own growth is in the same counters) and
// llama-swap lists the same other models, all ready, at the end as at the start: a model that unloaded or loaded while the seat
// loaded moved the same counters.
//
// What the counters cannot tell apart stays in a record: another process of the host that grew or shrank during the load. The
// store's percentile is what answers that (one low run never lowers it, one high run speaks until ten loads exist).

import (
	"context"
	"sort"
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
	"github.com/dmmdea/offload-harness/internal/swapclient"
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
	// seatOthersTimeout bounds each read of llama-swap's lists around a measurement. Chosen: llama-swap answers in milliseconds
	// when it is up, and a measurement that cannot read them in this long records nothing.
	seatOthersTimeout = 5 * time.Second
)

// seatMeasureCfg is the config the loads started through warmSeatWith are measured under: the one the process built its pipeline
// with. warmSeatWith has no config of its own, and both contract doors and the kv-slot rewarm call it.
var seatMeasureCfg atomic.Pointer[config.Config]

func registerSeatMeasure(cfg config.Config) { seatMeasureCfg.Store(&cfg) }

// seatRecordMu serialises the same-load check and the record, so two contracts that finish together record once.
var seatRecordMu sync.Mutex

// seatLoadMeter is the measurement of one warm-up. The zero value measures nothing.
type seatLoadMeter struct{ finish func(loaded bool) }

// begin starts the measurement when seat is the registered config's agent seat and reads absent now: the caller looked before
// it waited for the card (the lease fence), so the seat is read again here, just before the warm request, and the baseline is of
// this moment. A seat that another call began loading in between, or whose model id cannot be resolved, is not measured.
func (s *seatLoadMeter) begin(ctx context.Context, endpoint, seat string) {
	cfg := seatMeasureCfg.Load()
	if cfg == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, seatOthersTimeout)
	rd, err := seatload.Running(rctx, warmClient, endpoint, seat)
	cancel()
	if err != nil || rd.Loaded || rd.Starting || rd.Ambiguous {
		return
	}
	s.finish = BeginSeatLoadMeasure(*cfg, endpoint, seat, rd.Canonical)
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
// the measurement (loaded = the load completed). The caller has just established that the seat is absent, and canonical is the
// model id llama-swap's roster resolved the seat's name to in that reading. For a seat that is not cfg's agent seat, a name the
// roster did not resolve, a platform with no host-memory reader, a lease directory that cannot be read, a lane that declared host
// RAM and is live, or another model that is listed and not ready, the returned function does nothing. Never nil, never panics.
func BeginSeatLoadMeasure(cfg config.Config, endpoint, seat, canonical string) (finish func(loaded bool)) {
	nop := func(bool) {}
	defer func() {
		if recover() != nil {
			finish = nop
		}
	}()
	model := strings.TrimSpace(cfg.AgentPlannerModel(""))
	canonical = strings.TrimSpace(canonical)
	if model == "" || canonical == "" || !strings.EqualFold(strings.TrimSpace(seat), model) || !gpuprobe.HostMemorySupported {
		return nop
	}
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil || hostLaneLive(m) {
		return nop
	}
	before, ok := otherModels(endpoint, model, canonical)
	if !ok {
		return nop
	}
	run := seatmeasure.Begin(gpuprobe.ReadHostMemory, seatMeasureEvery)
	return func(loaded bool) {
		defer func() { _ = recover() }()
		res := run.Finish(loaded)
		if !loaded || !res.Clean || hostLaneLive(m) {
			return
		}
		if now, ok := otherModels(endpoint, model, canonical); !ok || now != before {
			return
		}
		recordSeatLoad(cfg, model, canonical, res)
	}
}

// otherModels is what llama-swap lists besides the seat, as one sorted line of model ids. ok is false when the list cannot be
// read or any of those models is not ready (loading, stopping): a load measured across that is not the seat's alone.
func otherModels(endpoint, seat, canonical string) (ids string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), seatOthersTimeout)
	defer cancel()
	rows, err := seatload.Occupants(ctx, warmClient, swapclient.BaseURL(endpoint))
	if err != nil {
		return "", false
	}
	var others []string
	for _, r := range rows {
		switch {
		case r.State == "stopped" || r.State == "shutdown":
		case strings.EqualFold(r.Model, seat) || strings.EqualFold(r.Model, canonical):
		case r.State != "ready":
			return "", false
		default:
			others = append(others, r.Model)
		}
	}
	sort.Strings(others)
	return strings.Join(others, " "), true
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

// recordSeatLoad keeps a clean result in the footprint store, once per load, under the model id the seat's name resolved to.
func recordSeatLoad(cfg config.Config, model, canonical string, res seatmeasure.Result) {
	store := seatStore(cfg)
	if store == nil {
		return
	}
	seatRecordMu.Lock()
	defer seatRecordMu.Unlock()
	if hp, ok := store.HostPeak(model, canonical, seatLoadTask); ok && time.Since(hp.Updated).Abs() <= seatLoadSameWindow {
		return // the load another contract (or process) already recorded
	}
	store.RecordHost(model, canonical, seatLoadTask, res.CommitPeakGiB, res.TouchedPeakGiB)
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
// its measured loads, and exactly hostneed.SeatNeed(cfg) for a seat that has none. runs is how many loads are recorded. It reads
// the store alone (no serving layer is asked which model the seat's name means now): the loads are those of the model id measured
// last under that name, so a name bound to another model is judged by the old model's figures until its first load is recorded,
// and by the new window, the seed as its floor, from then on.
func SeatNeedFor(cfg config.Config) (need hostneed.Need, runs int) {
	if model := strings.TrimSpace(cfg.AgentPlannerModel("")); model != "" {
		if store := seatStore(cfg); store != nil {
			if id, ok := store.LatestHostQuant(model, seatLoadTask); ok {
				if hp, ok := store.HostPeak(model, id, seatLoadTask); ok {
					return hostneed.SeatNeedMeasured(cfg, hp.ResidentGiB, hp.PrivateGiB, hp.Runs), hp.Runs
				}
			}
		}
	}
	return hostneed.SeatNeed(cfg), 0
}
