package placement

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// gpuProbeTimeout bounds the one nvidia-smi exec a snapshot makes: a wedged
// driver can hang nvidia-smi for tens of seconds and an admission decision
// must not wait behind it.
const gpuProbeTimeout = 5 * time.Second

// seatProbeTimeout bounds the seatload read of one seat (two loopback GETs,
// three with the roster) — the same budget the delegate's busy probe uses.
const seatProbeTimeout = 4 * time.Second

// DefaultSnapshotTTL is how long one reading serves: 2 s, so a 32-subtask
// spread deciding per subtask per tick execs nvidia-smi once, not 32 times.
const DefaultSnapshotTTL = 2 * time.Second

// Snapshot is the memoised Live of the local box. Every reader is read at most
// once per ttl — one gpuprobe.Read serves every DeviceFree/DeviceIndex, one
// HostFreeRAMGiB every HostFree, one ProbePresence every Presence, one
// seatload.Inflight per (layer, role) — because the table asks several
// questions per decision and a spread makes many decisions per tick. A
// snapshot never loads a model: seatload.Inflight stops at /running for a
// cold seat, and windows come from config.
type Snapshot struct {
	cfg config.Config
	ttl time.Duration

	// injectable for tests; production wiring in NewSnapshot
	now          func() time.Time
	readGPU      func(ctx context.Context) ([]gpuprobe.Device, error)
	readRAM      func() (float64, bool)
	readPresence func(mode string, idle time.Duration) Presence
	readSeat     func(ctx context.Context, endpoint, seat string) (seatload.Reading, error)

	mu     sync.Mutex
	gpuAt  time.Time
	gpu    []gpuprobe.Device
	gpuOK  bool
	ramAt  time.Time
	ram    float64
	ramOK  bool
	presAt time.Time
	pres   Presence
	seats  map[string]seatMemo
}

type seatMemo struct {
	at    time.Time
	state SeatState
}

var seatClient = &http.Client{Timeout: seatProbeTimeout}

// NewSnapshot builds the local box's memoised readers over its config: the
// layers say which seats exist, Endpoint says where llama-swap answers for
// them (a layer seat is pinned to a local device, so it is never behind a
// seat_endpoints override), operator_presence/operator_idle_sec drive the
// presence probe. ttl ≤ 0 reads as DefaultSnapshotTTL.
func NewSnapshot(cfg config.Config, ttl time.Duration) *Snapshot {
	if ttl <= 0 {
		ttl = DefaultSnapshotTTL
	}
	return &Snapshot{
		cfg:          cfg,
		ttl:          ttl,
		now:          time.Now,
		readGPU:      gpuprobe.Read,
		readRAM:      gpuprobe.HostFreeRAMGiB,
		readPresence: ProbePresence,
		readSeat: func(ctx context.Context, endpoint, seat string) (seatload.Reading, error) {
			return seatload.Inflight(ctx, seatClient, endpoint, seat)
		},
		seats: map[string]seatMemo{},
	}
}

// sharedSnapshots is the process-wide registry SharedSnapshot serves from:
// one Snapshot per placement identity, however many pipelines the process
// builds over the same box.
var (
	sharedMu        sync.Mutex
	sharedSnapshots = map[string]*Snapshot{}
)

// SharedSnapshot returns the process-wide Snapshot for cfg's placement
// identity — its endpoint, presence settings and layer seats — building it
// on first use with DefaultSnapshotTTL.
//
// WHY process-wide and not per pipeline: the in-loop offload builds a fresh
// Pipeline per contract (NewInLoopPipeline per agent_run on the MCP door,
// NewRecordlessOffload per node-side contract), so a memo owned by the
// pipeline gives every in-flight contract its own readers — 32 contracts on
// the pair seat would exec nvidia-smi and GET /running 32 times per window,
// the exact multiplicity DefaultSnapshotTTL exists to prevent. Keying on the
// placement identity rather than the endpoint alone keeps two configs that
// disagree on layers or presence (a test process, a hot-reloaded config)
// from reading through each other's memo.
func SharedSnapshot(cfg config.Config) *Snapshot {
	key := snapshotKey(cfg)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s, ok := sharedSnapshots[key]; ok {
		return s
	}
	s := NewSnapshot(cfg, DefaultSnapshotTTL)
	sharedSnapshots[key] = s
	return s
}

// snapshotKey is the placement identity SharedSnapshot memoises on: every
// config field a Snapshot reads (Endpoint for the seat probe, presence mode
// and idle threshold for the presence probe, the layers for LayerSeat).
func snapshotKey(cfg config.Config) string {
	b, err := json.Marshal(struct {
		Endpoint string
		Presence string
		Idle     time.Duration
		Layers   []config.LayerSpec
	}{cfg.Endpoint, cfg.PresenceMode(), cfg.OperatorIdle(), cfg.Layers})
	if err != nil {
		// Layers are plain strings, ints and floats; this cannot fail. Fall
		// back to the endpoint so a shared memo still exists rather than none.
		return cfg.Endpoint
	}
	return string(b)
}

// LiveFromReadings builds a Live from readings a caller ALREADY holds, for
// the surface that must not read anything itself: a fleet node's health
// handler answers from its background VRAM snapshot and host sample, never
// from an exec or a seat probe inside the request. A nil reading stays a nil
// reader, so the guard that needed it refuses (fail closed) instead of being
// admitted on a number nobody read.
//
// Seat is deliberately absent: a cached health read knows which seats the
// roster ANSWERS for (published per seat as SeatRow.Served), not which are
// loaded, and claiming an unread seat is cold is how a delegator would tell
// itself there was nothing to evict.
func LiveFromReadings(devs []gpuprobe.Device, hostFreeGiB *float64, pres *Presence) Live {
	l := Live{}
	if len(devs) > 0 {
		l.DeviceFree = func(device string) (float64, bool) { return gpuprobe.FreeGiB(devs, device) }
		l.DeviceIndex = func(device string) (string, bool) { return gpuprobe.IndexOf(devs, device) }
	}
	if hostFreeGiB != nil {
		free := *hostFreeGiB
		l.HostFree = func() (float64, bool) { return free, true }
	}
	if pres != nil {
		p := *pres
		l.Presence = func() Presence { return p }
	}
	return l
}

// LiveFromConfig is the one-liner every local caller uses: a fresh 2 s
// snapshot's readers. Callers that make many decisions per tick (the spread)
// keep one Snapshot and call Live() once instead.
func LiveFromConfig(cfg config.Config) Live {
	return NewSnapshot(cfg, DefaultSnapshotTTL).Live()
}

// Live exposes the snapshot as the table's reader contract. Verdict stays nil:
// a local box has no remote verdict to carry.
func (s *Snapshot) Live() Live {
	return Live{
		Seat:        s.Seat,
		DeviceFree:  s.DeviceFree,
		DeviceIndex: s.DeviceIndex,
		HostFree:    s.HostFree,
		Presence:    s.Presence,
	}
}

func (s *Snapshot) fresh(at time.Time) bool {
	return !at.IsZero() && s.now().Sub(at) <= s.ttl
}

// devices returns the memoised nvidia-smi reading; !ok on a failed probe.
func (s *Snapshot) devices() ([]gpuprobe.Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fresh(s.gpuAt) {
		return s.gpu, s.gpuOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
	defer cancel()
	devs, err := s.readGPU(ctx)
	s.gpuAt = s.now()
	s.gpu, s.gpuOK = devs, err == nil && len(devs) > 0
	return s.gpu, s.gpuOK
}

// DeviceFree reads one card's free VRAM by index or UUID prefix from the
// memoised probe.
func (s *Snapshot) DeviceFree(device string) (float64, bool) {
	devs, ok := s.devices()
	if !ok {
		return 0, false
	}
	return gpuprobe.FreeGiB(devs, device)
}

// DeviceIndex resolves a UUID prefix (or echoes an index the probe knows) to
// the CUDA index nvidia-smi reports, from the same memoised probe. Ambiguous
// or absent = !ok, so the display-floor guard refuses rather than guessing
// which card the operator's UUID meant.
func (s *Snapshot) DeviceIndex(device string) (string, bool) {
	devs, ok := s.devices()
	if !ok {
		return "", false
	}
	return gpuprobe.IndexOf(devs, device)
}

// HostFree reads free host RAM from the memoised reader.
func (s *Snapshot) HostFree() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fresh(s.ramAt) {
		return s.ram, s.ramOK
	}
	s.ram, s.ramOK = s.readRAM()
	s.ramAt = s.now()
	return s.ram, s.ramOK
}

// Presence reads the operator-presence state for the config's mode and idle
// threshold, memoised.
func (s *Snapshot) Presence() Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fresh(s.presAt) {
		return s.pres
	}
	s.pres = s.readPresence(s.cfg.PresenceMode(), s.cfg.OperatorIdle())
	s.presAt = s.now()
	return s.pres
}

// Seat reads a (layer, role) seat's occupancy through llama-swap, memoised
// per seat. Known=false for an undeclared seat, the router role (its rungs
// are the cascade's, not one seat), a read error, or an ambiguous reading
// (the roster unreadable and /running holding entries this name cannot be
// matched against) — "could not tell" is never "idle".
func (s *Snapshot) Seat(layer, role string) SeatState {
	seat, ok := s.cfg.LayerSeat(layer, role)
	if !ok || seat.Model == "" {
		return SeatState{}
	}
	key := layer + "/" + role
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.seats[key]; ok && s.fresh(m.at) {
		return m.state
	}
	ctx, cancel := context.WithTimeout(context.Background(), seatProbeTimeout)
	defer cancel()
	rd, err := s.readSeat(ctx, s.cfg.Endpoint, seat.Model)
	st := SeatState{}
	if err == nil && !rd.Ambiguous {
		st = SeatState{Known: true, Loaded: rd.Loaded, Inflight: rd.Inflight}
		if !rd.Loaded {
			st.Inflight = 0
		}
	}
	s.seats[key] = seatMemo{at: s.now(), state: st}
	return st
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func hasPrefixFold(s, lowerPrefix string) bool {
	return len(s) >= len(lowerPrefix) && lower(s[:len(lowerPrefix)]) == lowerPrefix
}
