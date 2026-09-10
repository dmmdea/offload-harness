package placement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

func TestSnapshotReadsEachSourceOncePerTTL(t *testing.T) {
	cfg := config.CompositeFixture()
	cfg.OperatorPresence = "auto"
	now := time.Unix(1_700_000_000, 0)
	gpuReads, ramReads, presReads, seatReads := 0, 0, 0, 0
	s := NewSnapshot(cfg, 2*time.Second)
	s.now = func() time.Time { return now }
	s.readGPU = func(context.Context) ([]gpuprobe.Device, error) {
		gpuReads++
		return []gpuprobe.Device{{Index: 0, UUID: "GPU-aaaa", FreeGiB: 2}, {Index: 1, UUID: "GPU-2a44210f-6739", FreeGiB: 15}, {Index: 2, UUID: "GPU-cccc", FreeGiB: 3}}, nil
	}
	s.readRAM = func() (float64, bool) { ramReads++; return 80, true }
	s.readPresence = func(mode string, idle time.Duration) Presence {
		presReads++
		if mode != "auto" || idle != 15*time.Minute {
			t.Errorf("presence probe gets the config's mode and idle threshold, got %q %v", mode, idle)
		}
		return Presence{Mode: mode, Known: true, Away: true, Locked: true}
	}
	s.readSeat = func(ctx context.Context, endpoint, seat string) (seatload.Reading, error) {
		seatReads++
		if endpoint != cfg.Endpoint {
			t.Errorf("seats are read through the box's own llama-swap, got %q", endpoint)
		}
		if seat == "agent-pool" {
			return seatload.Reading{Loaded: true, Inflight: 4}, nil
		}
		return seatload.Reading{}, nil
	}
	live := s.Live()
	// A 32-subtask spread asks many times inside one tick: each source is read once.
	for i := 0; i < 32; i++ {
		if free, ok := live.DeviceFree("1"); !ok || free != 15 {
			t.Fatalf("device 1 free: %v %v", free, ok)
		}
		if free, ok := live.DeviceFree("GPU-2a44210f"); !ok || free != 15 {
			t.Fatalf("UUID prefix resolves through the same probe: %v %v", free, ok)
		}
		if idx, ok := live.DeviceIndex("GPU-2a44210f"); !ok || idx != "1" {
			t.Fatalf("UUID → index: %q %v", idx, ok)
		}
		if _, ok := live.DeviceFree("7"); ok {
			t.Fatal("an absent card is not ok")
		}
		if ram, ok := live.HostFree(); !ok || ram != 80 {
			t.Fatalf("host RAM: %v %v", ram, ok)
		}
		if p := live.Presence(); !p.Known || !p.Away {
			t.Fatalf("presence: %+v", p)
		}
		if st := live.Seat("pair", "agent"); !st.Known || !st.Loaded || st.Inflight != 4 {
			t.Fatalf("pair/agent: %+v", st)
		}
		if st := live.Seat("single", "ocr"); !st.Known || st.Loaded {
			t.Fatalf("single/ocr: %+v", st)
		}
		if st := live.Seat("single", "router"); st.Known {
			t.Fatalf("the router role is never read as a seat: %+v", st)
		}
		if st := live.Seat("nope", "agent"); st.Known {
			t.Fatalf("an undeclared seat is unknown: %+v", st)
		}
	}
	if gpuReads != 1 || ramReads != 1 || presReads != 1 || seatReads != 2 {
		t.Fatalf("one read per source per ttl: gpu=%d ram=%d presence=%d seats=%d (want 1/1/1/2)", gpuReads, ramReads, presReads, seatReads)
	}
	// Past the ttl every source is read again — a stale reading is not a reading.
	now = now.Add(2*time.Second + time.Millisecond)
	live.DeviceFree("1")
	live.HostFree()
	live.Presence()
	live.Seat("pair", "agent")
	if gpuReads != 2 || ramReads != 2 || presReads != 2 || seatReads != 3 {
		t.Fatalf("after the ttl each source is re-read: gpu=%d ram=%d presence=%d seats=%d", gpuReads, ramReads, presReads, seatReads)
	}
}

func TestSnapshotFailsClosedOnReaderErrorsAndUnknownOnAmbiguousSeats(t *testing.T) {
	cfg := config.CompositeFixture()
	s := NewSnapshot(cfg, time.Second)
	s.readGPU = func(context.Context) ([]gpuprobe.Device, error) { return nil, errors.New("nvidia-smi: hung") }
	s.readRAM = func() (float64, bool) { return 0, false }
	s.readSeat = func(ctx context.Context, endpoint, seat string) (seatload.Reading, error) {
		switch seat {
		case "agent-pool":
			return seatload.Reading{}, errors.New("metrics unreadable")
		case "qwen3.8-27b-262k":
			return seatload.Reading{Ambiguous: true, RunningOthers: 1}, nil
		}
		return seatload.Reading{Loaded: true, Inflight: 1}, nil
	}
	live := s.Live()
	if _, ok := live.DeviceFree("1"); ok {
		t.Fatal("a failed probe is not ok")
	}
	if _, ok := live.DeviceIndex("GPU-2a44210f"); ok {
		t.Fatal("a failed probe resolves nothing")
	}
	if _, ok := live.HostFree(); ok {
		t.Fatal("a failed RAM read is not ok")
	}
	if st := live.Seat("pair", "agent"); st.Known {
		t.Fatalf("a read error is unknown, never idle: %+v", st)
	}
	if st := live.Seat("pair", "long"); st.Known {
		t.Fatalf("an ambiguous read is unknown: %+v", st)
	}
	if st := live.Seat("single", "ocr"); !st.Known || !st.Loaded || st.Inflight != 1 {
		t.Fatalf("a clean read is known: %+v", st)
	}
	// The presence probe with the default mode (present) never touches the OS.
	p := live.Presence()
	if p.Mode != "present" || !p.Known || p.Away {
		t.Fatalf("default presence is present: %+v", p)
	}
	// LiveFromConfig is the 2 s snapshot's Live — every reader set.
	l := LiveFromConfig(cfg)
	if l.Seat == nil || l.DeviceFree == nil || l.DeviceIndex == nil || l.HostFree == nil || l.Presence == nil {
		t.Fatalf("LiveFromConfig wires every reader: %+v", l)
	}
	if l.Verdict != nil {
		t.Fatal("a local box has no remote verdict")
	}
}
