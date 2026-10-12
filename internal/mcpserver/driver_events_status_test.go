package mcpserver

// driver_events_status_test.go: offload_status's gpu_lease.activity carries the driver's recent fault events for the
// leased cards (C-83-B). The reader is a seam (statusDriverEvents) so no test starts PowerShell or reads a log.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// leasedStatus takes a card-scoped lease on card 2 of a three-card fixture, with the GPU sample on and the driver
// reader fake, and returns the server.
func leasedStatus(t *testing.T, events []gpuactivity.DriverEvent, rerr error) *Server {
	t.Helper()
	s, cfg, _ := statusFixture(t)
	statusSamplesGPU = true
	statusGPUSampler = func(context.Context) ([]gpuactivity.GPU, error) {
		return []gpuactivity.GPU{
			{Index: 0, UUID: "GPU-aaaa0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true},
			{Index: 1, UUID: "GPU-bbbb0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true, DisplayActive: true},
			{Index: 2, UUID: "GPU-cccc0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true},
		}, nil
	}
	statusGPUProcSampler = func(context.Context) ([]gpuactivity.GPUProcess, error) { return nil, nil }
	t.Cleanup(func() { statusGPUSampler, statusGPUProcSampler = nil, nil })
	statusDriverEvents = func(context.Context) ([]gpuactivity.DriverEvent, error) { return events, rerr }

	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "batch", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return s
}

func leaseActivity(t *testing.T, s *Server, section string) map[string]any {
	t.Helper()
	got := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"`+section+`"}`)))
	block, _ := got["gpu_lease"].(map[string]any)
	act, _ := block["activity"].(map[string]any)
	if act == nil {
		t.Fatalf("section %s: no gpu_lease.activity in %v", section, got)
	}
	return act
}

func TestOffloadStatusGPULeaseActivityCarriesDriverEvents(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 5, 49, 0, time.UTC)
	s := leasedStatus(t, []gpuactivity.DriverEvent{
		{At: at, Source: "nvlddmkm", ID: 153, Bus: "b5:00", Card: 2, UUID: "GPU-cccc0000-x", Text: "Error occurred on GPUID: b500"},
		{At: at, Source: "nvlddmkm", ID: 153, Bus: "17:00", Card: 0, UUID: "GPU-aaaa0000-x", Text: "on a card nobody leased"},
	}, nil)
	for _, section := range []string{"gpu_lease", "all"} {
		act := leaseActivity(t, s, section)
		evs, _ := act["driver_events"].([]any)
		if len(evs) != 1 {
			t.Fatalf("section %s: only the leased card's event is shown, got %v", section, act["driver_events"])
		}
		e, _ := evs[0].(map[string]any)
		if e["source"] != "nvlddmkm" || e["id"] != float64(153) || e["card"] != float64(2) || e["bus"] != "b5:00" || e["uuid"] != "GPU-cccc0000-x" {
			t.Errorf("section %s: event = %v", section, e)
		}
	}
}

// Facts, not a verdict: the brief's one-line verdict is the one it was without the events.
func TestDriverEventsDoNotMoveTheBriefVerdict(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 5, 49, 0, time.UTC)
	s := leasedStatus(t, []gpuactivity.DriverEvent{{At: at, Source: "nvlddmkm", ID: 153, Card: 2, UUID: "GPU-cccc0000-x"}}, nil)
	line := func() string {
		got := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))
		l, _ := got["gpu_lease_verdict"].(string)
		return l
	}
	with := line()
	reader := statusDriverEvents
	statusDriverEvents = nil
	without := line()
	statusDriverEvents = reader
	if with != without {
		t.Fatalf("the events moved the brief verdict line:\nwith:    %s\nwithout: %s", with, without)
	}
}

func TestAnOffloadStatusReaderFailureIsANoteInTheActivity(t *testing.T) {
	s := leasedStatus(t, nil, errors.New("Get-WinEvent: access denied"))
	act := leaseActivity(t, s, "gpu_lease")
	if note, _ := act["driver_events_note"].(string); !strings.Contains(note, "access denied") {
		t.Fatalf("the failure rides as a note, got %v", act)
	}
	if _, bad := act["driver_events"]; bad {
		t.Errorf("a failed read lists no events: %v", act["driver_events"])
	}
}

// The fixture leaves the reader off, so no existing status test starts PowerShell.
func TestStatusFixtureLeavesTheDriverEventReaderOff(t *testing.T) {
	statusFixture(t)
	if statusDriverEvents != nil {
		t.Fatal("statusFixture must set the driver-event reader nil")
	}
}

// A section computes only what it returns. The brief returns the verdict line and the per-card table, neither of which
// reads a driver event, so a held lease must not make it start the PowerShell read (about 0.6 s, bounded at 8 s) only to
// throw the answer away; the gpu_lease section and the full answer, which do return the events, read once.
func TestBriefMakesNoDriverEventReadButTheLeaseSectionDoes(t *testing.T) {
	s := leasedStatus(t, nil, nil)
	reads := 0
	statusDriverEvents = func(context.Context) ([]gpuactivity.DriverEvent, error) { reads++; return nil, nil }
	for _, tc := range []struct {
		section string
		want    int
	}{{"brief", 0}, {"gpu_lease", 1}, {"all", 1}, {"fleet", 0}} {
		reads = 0
		_ = statusCall(t, s, json.RawMessage(`{"section":"`+tc.section+`"}`))
		if reads != tc.want {
			t.Errorf("section %s read the driver events %d time(s), want %d", tc.section, reads, tc.want)
		}
	}
}
