package main

// gpu_status_driverevents_test.go: `gpu status` (text and --json) shows the driver's recent fault events for the leased
// card (C-83-B). The reader is wired by activityOptions; the tests swap in a canned one, so nothing starts PowerShell.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// useDriverEvents runs the real activity snapshot (its lease read, its scope rule, its event filter) over a fake card
// sample and a canned driver-event reader, and fails the test when gpu status wired no reader of its own.
func useDriverEvents(t *testing.T, events []gpuactivity.DriverEvent) {
	t.Helper()
	statusActivityFn = func(ctx context.Context, o gpuactivity.Options) gpuactivity.View {
		if o.DriverEvents == nil {
			t.Error("gpu status must wire a driver-event reader into the activity snapshot (activityOptions)")
		}
		o.DriverEvents = func(context.Context) ([]gpuactivity.DriverEvent, error) { return events, nil }
		o.Endpoint, o.Seat = "", ""
		o.Sampler = func(context.Context) ([]gpuactivity.GPU, error) {
			return []gpuactivity.GPU{
				{Index: 0, UUID: "GPU-aaaa0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true},
				{Index: 1, UUID: "GPU-bbbb0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true},
				{Index: 2, UUID: "GPU-cccc0000-x", Name: "Test Card", MemTotalMiB: 16384, UtilKnown: true},
			}, nil
		}
		o.ProcSampler = func(context.Context) ([]gpuactivity.GPUProcess, error) { return nil, nil }
		return gpuactivity.Snapshot(ctx, o)
	}
}

func cardTwoFault() gpuactivity.DriverEvent {
	return gpuactivity.DriverEvent{At: time.Date(2026, 10, 11, 9, 5, 49, 0, time.UTC), Source: "nvlddmkm", ID: 153, Bus: "b5:00", Card: 2, UUID: "GPU-cccc0000-x", Text: "Error occurred on GPUID: b500"}
}

func TestGPUStatusJSONCarriesDriverEvents(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	_, finish := holdCardTwoWithAWaiter(t, m)
	defer finish()
	other := cardTwoFault()
	other.Card, other.UUID, other.Bus = 0, "GPU-aaaa0000-x", "17:00"
	useDriverEvents(t, []gpuactivity.DriverEvent{cardTwoFault(), other})

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc struct {
		Activity struct {
			DriverEvents []struct {
				Source string `json:"source"`
				ID     int    `json:"id"`
				Card   int    `json:"card"`
				Bus    string `json:"bus"`
			} `json:"driver_events"`
		} `json:"activity"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json must be one JSON document: %v\n%s", err, out)
	}
	evs := doc.Activity.DriverEvents
	if len(evs) != 1 || evs[0].Source != "nvlddmkm" || evs[0].ID != 153 || evs[0].Card != 2 || evs[0].Bus != "b5:00" {
		t.Fatalf("activity.driver_events must carry card 2's event only, got %+v\n%s", evs, out)
	}
}

func TestGPUStatusTextShowsDriverEvents(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	_, finish := holdCardTwoWithAWaiter(t, m)
	defer finish()
	useDriverEvents(t, []gpuactivity.DriverEvent{cardTwoFault()})

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"driver events", "card 2", "nvlddmkm 153", "Error occurred on GPUID: b500"} {
		if !strings.Contains(out, want) {
			t.Errorf("`gpu status` must print %q:\n%s", want, out)
		}
	}
}

// Guard: no event on the leased card prints nothing new.
func TestGPUStatusWithNoDriverEventsPrintsNoDriverLine(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	_, finish := holdCardTwoWithAWaiter(t, m)
	defer finish()
	useDriverEvents(t, nil)

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(strings.ToLower(out), "driver event") {
		t.Fatalf("a box with no events prints nothing about them:\n%s", out)
	}
}

// activityOptions is where gpu status wires the reader; the other gpu verbs' reads are unchanged.
func TestActivityOptionsWireTheDriverEventReader(t *testing.T) {
	if activityOptions(config.Default()).DriverEvents == nil {
		t.Fatal("activityOptions must wire gpuactivity.ReadDriverEvents")
	}
}
