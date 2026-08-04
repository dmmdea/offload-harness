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
	"os"
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

// GPUDevice is one parsed nvidia-smi device line: the index/uuid/name
// nvidia-smi itself reports plus its GiB memory figures. JSON tags match the
// gpu_devices[] shape documented in docs/FLEET-NODE.md. UUID is what
// primary_gpu_uuid pins against (config.Config.PrimaryGPUUUID /
// SelectHeadlineDevice) — an operator reads it straight off a running node's
// /fleet/health gpu_devices[] to fill that config key in.
type GPUDevice struct {
	Index    int     `json:"index"`
	UUID     string  `json:"uuid"`
	Name     string  `json:"name"`
	TotalGiB float64 `json:"vram_total_gb"`
	FreeGiB  float64 `json:"vram_free_gb"`
}

// ParseSmiMemoryDevices parses `nvidia-smi --query-gpu=index,uuid,name,
// memory.total,memory.used --format=csv,noheader,nounits` output — one line
// per GPU, e.g. "0, GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97, NVIDIA GeForce
// RTX 5060 Ti, 16311, 867" — into an ordered []GPUDevice (nvidia-smi's own
// enumeration order, i.e. PCI bus order; NOT necessarily CUDA device order —
// see HeadlineDevice's doc comment for why that distinction is the whole bug
// this parser exists to fix, and SelectHeadlineDevice/primary_gpu_uuid for
// the deterministic override). This is the devices-aware sibling of
// ParseSmiMemory: that function is kept unchanged (first-line-wins, 2-field
// CSV) because it has a caller outside the snapshot/health path (the
// pipeline's per-process footprint delta sampler), so its behavior cannot
// change out from under that caller. This is a separate function over a
// separate (5-field) query instead.
//
// CRLF and surrounding whitespace are tolerated (nvidia-smi emits \r\n on
// Windows). Blank lines and lines that don't parse as "index, uuid, name,
// total, used" are SKIPPED rather than failing the whole probe — a single
// garbled/partial row (a transient nvidia-smi hiccup) must not take down
// every other card's reading. A line whose total is <= 0 or whose used is
// negative is likewise skipped (not a working GPU line). If NO line parses
// into a valid device, that IS an error: the contract treats vram_total_gb
// <= 0 as a failed probe, so a would-be zero-device snapshot must never reach
// the caller as success.
func ParseSmiMemoryDevices(out string) ([]GPUDevice, error) {
	var devices []GPUDevice
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		uuid := strings.TrimSpace(fields[1])
		name := strings.TrimSpace(fields[2])
		totalMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		if err != nil {
			continue
		}
		usedMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
		if err != nil {
			continue
		}
		if totalMiB <= 0 || usedMiB < 0 {
			continue
		}
		freeMiB := totalMiB - usedMiB
		if freeMiB < 0 {
			freeMiB = 0
		}
		devices = append(devices, GPUDevice{
			Index:    idx,
			UUID:     uuid,
			Name:     name,
			TotalGiB: totalMiB / 1024,
			FreeGiB:  freeMiB / 1024,
		})
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("nvidia-smi multi-device memory query: no valid GPU lines parsed (contract: vram_total_gb <= 0 = failed probe)")
	}
	return devices, nil
}

// HeadlineDevice picks which parsed device the health payload's single
// vram_total_gb/vram_free_gb pair describes. A single render job binds to
// exactly ONE CUDA device — so the admission-relevant number is the biggest
// device a job could actually land on, never an arbitrary enumeration index.
// That distinction is the whole bug this fixes: nvidia-smi enumerates in PCI
// bus order, which has no relationship to which device a CUDA app actually
// computes on (CUDA_DEVICE_ORDER=FASTEST_FIRST can bind cuda:0 to nvidia-smi
// index 1) — "index 0 wins" was an artifact of enumeration order dressed up
// as a measurement, and it silently mis-sizes admission whenever the compute
// card isn't index 0.
//
// Rule: the device with the LARGEST TotalGiB wins. An exact tie in total is
// broken by whichever has more FreeGiB right now (more headroom to admit a
// job against). This is deliberately NOT a sum across devices — summing free
// VRAM would let the dispatcher admit a job that no single card can actually
// hold, since a render binds to one device, not the fleet of them combined.
//
// Documented limitation: on a box with two near-identical-capacity cards
// (see TestParseSmiMemoryDevices_NodeBShape: 16311 MiB vs 16303 MiB total, an
// 8 MiB gap from per-SKU driver/firmware reserve, not a real capacity
// difference), this rule has no way to know which index a CUDA app's device
// -order policy will actually pick — nvidia-smi carries no such signal. It
// picks the device with more raw capacity, which is a defensible,
// deterministic, non-arbitrary choice, but is NOT guaranteed to be "the
// compute card" on near-twin hardware. The full per-device breakdown in
// Snapshot.Devices / the health payload's gpu_devices[] exists precisely so
// a caller that needs to reason about this edge case (or a smarter
// dispatcher) has the real numbers for every card, not just the headline
// guess.
//
// devices must be non-empty — callers only reach this after a successful
// parse, which never returns an empty slice.
//
// This is the FALLBACK rule — SelectHeadlineDevice applies it only when no
// primary_gpu_uuid is pinned (or the pinned UUID isn't present), because
// total VRAM alone cannot reliably identify "the" card either: two
// same-SKU-size cards can be a near-tie (16311 vs 16303 MiB on <node-b>) that this
// rule breaks by raw capacity, not by which one is actually doing the compute
// work.
func HeadlineDevice(devices []GPUDevice) GPUDevice {
	head := devices[0]
	for _, d := range devices[1:] {
		if d.TotalGiB > head.TotalGiB || (d.TotalGiB == head.TotalGiB && d.FreeGiB > head.FreeGiB) {
			head = d
		}
	}
	return head
}

// SelectHeadlineDevice is the full headline-selection rule the health sampler
// uses: an operator-pinned primary_gpu_uuid (config.Config.PrimaryGPUUUID)
// takes priority over HeadlineDevice's largest-total heuristic, because UUID
// is the ONLY identifier this codebase can rely on across a reboot/reseat.
// Canonical guidance (CMP tier notes): pin by GPU UUID, NEVER index —
// verified live on <node-b> 2026-08-04: nvidia-smi's own index 0 is the RTX 5060
// Ti, but ComfyUI's CUDA ordering (CUDA_DEVICE_ORDER=FASTEST_FIRST) binds
// cuda:0 to the RTX 5070 Ti at index 1, and the two cards can't be told apart
// by total VRAM either (16311 vs 16303 MiB — HeadlineDevice's rule picks
// whichever has 8 MiB more, not whichever is faster). A UUID survives all of
// that: it is burned into the card, unaffected by enumeration order, CUDA
// ordering, or which PCI slot it's currently seated in.
//
// Behavior:
//   - primaryUUID == "": HeadlineDevice(devices), no warning — today's
//     behavior, byte-identical.
//   - primaryUUID set and found among devices: that device wins outright,
//     regardless of its total/free VRAM — no warning.
//   - primaryUUID set but NOT found (typo, or the pinned card is actually
//     gone): falls back to HeadlineDevice(devices), AND returns a non-empty
//     warning naming the missing UUID. A silent fallback here would hide a
//     typo'd config (or a genuinely missing card) forever, which is worse
//     than the original enumeration-order bug this whole file fixes — at
//     least that one was consistently wrong in a discoverable way. The
//     caller (Sampler.sampleDevices) is responsible for actually emitting
//     the warning (once, not once per 2s tick) — this function stays pure
//     and side-effect-free so it's trivially unit-testable.
//
// devices must be non-empty, same precondition as HeadlineDevice.
func SelectHeadlineDevice(devices []GPUDevice, primaryUUID string) (head GPUDevice, warning string) {
	if primaryUUID != "" {
		for _, d := range devices {
			if d.UUID == primaryUUID {
				return d, ""
			}
		}
		warning = fmt.Sprintf("primary_gpu_uuid %q not found among %d parsed GPU device(s) — falling back to the largest-total-VRAM rule; check for a typo, or a card that was removed/replaced", primaryUUID, len(devices))
	}
	return HeadlineDevice(devices), warning
}

// Snapshot is the cached global VRAM state /fleet/health reads. GiB = 2^30,
// per the contract.
type Snapshot struct {
	TotalGiB float64
	FreeGiB  float64
	// Devices is the full per-device breakdown (nvidia-smi multi-device
	// source only, in nvidia-smi's own enumeration order — see
	// ParseSmiMemoryDevices/HeadlineDevice). Nil when the source cannot
	// enumerate devices (windows-generic, or any plain single-value MemProbe
	// source): TotalGiB/FreeGiB alone describe that box exactly as they did
	// before this field existed, and a nil/empty Devices is never itself an
	// error condition — additive only, per ADR-style compatibility with every
	// existing single-GPU node.
	Devices []GPUDevice
	At      time.Time
}

// Sampler publishes the latest good Snapshot via an atomic.Value so the health
// handler never blocks on (or spawns) nvidia-smi.
type Sampler struct {
	snap atomic.Value // Snapshot
	// primaryGPUUUID and warnedMissingUUID back SelectHeadlineDevice's UUID
	// pin for the devices-aware path (sampleDevices/StartDeviceProbeSampler)
	// only; the plain MemProbe path (sample/StartProbeSampler) never touches
	// them. Both fields are read/written ONLY from sampleDevices, which runs
	// on a single-goroutine timeline (one synchronous call, then serialized
	// ticks — see runSamplerLoop), so no lock/atomic is needed here even
	// though snap itself is atomic.
	primaryGPUUUID    string
	warnedMissingUUID bool
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

func (s *Sampler) sample(probe MemProbe) {
	total, used, err := probe()
	if err != nil {
		return // keep the last good snapshot — never publish a bad probe
	}
	free := total - used
	if free < 0 {
		// The windows-generic source sums usage over ALL adapters while total is
		// the largest adapter (single-GPU is the target shape; ADR 0014) — on a
		// hybrid box the difference can dip negative. Publish an honest floor.
		free = 0
	}
	s.snap.Store(Snapshot{TotalGiB: total, FreeGiB: free, At: time.Now()})
}

// sampleDevices is sample's devices-aware sibling: it publishes the FULL
// per-device breakdown alongside a headline TotalGiB/FreeGiB derived by
// SelectHeadlineDevice — s.primaryGPUUUID when pinned and present, else
// HeadlineDevice's largest-total rule, never raw enumeration order. A missing
// pinned UUID logs ONE stderr warning for the process lifetime (not one per
// 2s tick — warnedMissingUUID latches after the first) naming the UUID, so a
// typo'd config is loud once rather than either silent or a spam flood.
func (s *Sampler) sampleDevices(probe DeviceProbe) {
	devices, err := probe()
	if err != nil {
		return // keep the last good snapshot — never publish a bad probe
	}
	head, warning := SelectHeadlineDevice(devices, s.primaryGPUUUID)
	if warning != "" && !s.warnedMissingUUID {
		fmt.Fprintf(os.Stderr, "[fleet-serve] warning: %s\n", warning)
		s.warnedMissingUUID = true
	}
	s.snap.Store(Snapshot{TotalGiB: head.TotalGiB, FreeGiB: head.FreeGiB, Devices: devices, At: time.Now()})
}

// runSamplerLoop is the shared scaffolding both StartProbeSampler and
// StartDeviceProbeSampler ride: sample once synchronously (so a successful
// probe is Load-able the moment the caller returns), then keep resampling on
// interval until ctx is done.
func runSamplerLoop(ctx context.Context, interval time.Duration, sampleOnce func()) {
	sampleOnce()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sampleOnce()
			}
		}
	}()
}

// StartGlobalSampler samples run() (an injected nvidia-smi invocation) once
// synchronously, then keeps refreshing — kept as the nvidia-smi convenience
// wrapper over StartProbeSampler (J3 made the memory SOURCE a provider; this
// signature predates that and existing callers/tests keep working).
func StartGlobalSampler(ctx context.Context, interval time.Duration, run func() (string, error)) *Sampler {
	return StartProbeSampler(ctx, interval, SmiProbe(run))
}

// StartProbeSampler samples probe once synchronously — so a successful probe
// is Load-able the moment this returns — then keeps refreshing every interval
// until ctx is done. Probe failures leave the previous snapshot in place: the
// sampler degrades to stale data, never to zeros. Staleness is bounded
// downstream — the health handler refuses (503) any snapshot older than
// maxSnapshotAge, so a persistently failing source (driver reset) cannot
// serve hours-stale 200s.
func StartProbeSampler(ctx context.Context, interval time.Duration, probe MemProbe) *Sampler {
	s := &Sampler{}
	runSamplerLoop(ctx, interval, func() { s.sample(probe) })
	return s
}

// StartDeviceProbeSampler is StartProbeSampler's devices-aware sibling: same
// sync-then-tick contract, but over a DeviceProbe, publishing the full
// per-device breakdown (Snapshot.Devices) alongside a SelectHeadlineDevice-
// derived TotalGiB/FreeGiB — primaryGPUUUID (config.Config.PrimaryGPUUUID,
// "" = disabled) pins the headline to one card by UUID, overriding the
// largest-total fallback; see SelectHeadlineDevice's doc comment for the full
// rule and why UUID (not index, not total VRAM) is the only stable pin. Used
// for the nvidia-smi multi-device source; the windows-generic source has no
// per-device signal (vram_windows.go) and stays on StartProbeSampler, so its
// Snapshot.Devices is always nil — exactly today's single-number behavior,
// and a primaryGPUUUID has no effect there (there is nothing to match it
// against).
func StartDeviceProbeSampler(ctx context.Context, interval time.Duration, probe DeviceProbe, primaryGPUUUID string) *Sampler {
	s := &Sampler{primaryGPUUUID: primaryGPUUUID}
	runSamplerLoop(ctx, interval, func() { s.sampleDevices(probe) })
	return s
}
