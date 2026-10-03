package gpuactivity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The lease verdict attributes load: "that load is the operator's game, not the holder's
// work". That is a question about a display IN USE, so it reads display_active alone
// (gpuprobe.DisplayCardUUIDs). display_attached is read and published (the card table marks
// the monitor's card with it, so the allocator never auto-picks it), but it is true for the
// whole life of the box: counting it as the operator's load would hide a holder's own work
// on that card and change every verdict on a host with card-scoped leases OFF.

func TestParseGPUsReadsDisplayAttached(t *testing.T) {
	gpus := ParseGPUs("0, GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 5060 Ti, 0, 100, 16311, Disabled, No\r\n" +
		"1, GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff, NVIDIA GeForce RTX 5070 Ti, 33, 900, 16303, Disabled, Yes\r\n" +
		"2, GPU-cccc3333-dddd-eeee-ffff-000000000000, NVIDIA GeForce RTX 5060 Ti, 0, 100, 16311, Disabled, [Not Supported]\r\n")
	if len(gpus) != 3 {
		t.Fatalf("parsed %d cards, want 3", len(gpus))
	}
	for i, want := range []bool{false, true, false} {
		if gpus[i].DisplayAttached != want {
			t.Errorf("card %d DisplayAttached = %v, want %v", i, gpus[i].DisplayAttached, want)
		}
		if gpus[i].DisplayActive {
			t.Errorf("card %d DisplayActive = true, the driver said Disabled", i)
		}
	}
}

// An older driver refuses the whole query when it names display_attached: the sample must
// still be taken, from the query this surface always ran.
func TestSampleGPUsFallsBackWhenDisplayAttachedIsRefused(t *testing.T) {
	gpuprobe.ResetDisplayAwareState()
	t.Cleanup(gpuprobe.ResetDisplayAwareState)
	oldRun := smiRun
	t.Cleanup(func() { smiRun = oldRun })
	var queries []string
	smiRun = func(_ context.Context, args ...string) (string, error) {
		q := strings.Join(args, " ")
		queries = append(queries, q)
		if strings.Contains(q, "display_attached") {
			return "", errors.New(`exit status 2: Field "display_attached" is not a valid field to query.`)
		}
		return "0, GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 5060 Ti, 0, 100, 16311, Disabled\r\n", nil
	}
	gpus, err := SampleGPUs(context.Background())
	if err != nil || len(gpus) != 1 {
		t.Fatalf("SampleGPUs: %v (%d cards), want the fallback's one card", err, len(gpus))
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "display_attached") || strings.Contains(queries[1], "display_attached") {
		t.Fatalf("queries = %q, want the full query then the one without display_attached", queries)
	}
}

func TestSampleGPUsAsksForDisplayAttachedFirst(t *testing.T) {
	gpuprobe.ResetDisplayAwareState()
	t.Cleanup(gpuprobe.ResetDisplayAwareState)
	oldRun := smiRun
	t.Cleanup(func() { smiRun = oldRun })
	var got string
	smiRun = func(_ context.Context, args ...string) (string, error) {
		got = strings.Join(args, " ")
		return "1, GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff, NVIDIA GeForce RTX 5070 Ti, 0, 900, 16303, Disabled, Yes\r\n", nil
	}
	gpus, err := SampleGPUs(context.Background())
	if err != nil || len(gpus) != 1 || !gpus[0].DisplayAttached {
		t.Fatalf("SampleGPUs: %v %+v, want the attached card read", err, gpus)
	}
	if !strings.Contains(got, ",display_active,display_attached") {
		t.Fatalf("query = %q, want display_active,display_attached", got)
	}
}

// The screen is asleep: display_active is Disabled everywhere and display_attached marks
// card 1. Nothing is being displayed, so the 33% on card 1 is whatever is computing there, and
// the holder is the only one who says it is: the verdict reads it as the holder's work.
func TestAttachedCardLoadIsStillTheHoldersWork(t *testing.T) {
	const (
		card0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
		card1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
		card2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
	)
	view := View{
		At: time.Now(), Held: true,
		Holder: &Holder{PID: 26464, Alive: true, Class: "text", AgeSec: 8456, Reason: "seat-gate"},
		Seat:   SeatState{Name: "agent-pool"},
		GPUs: []GPU{
			{Index: 0, UUID: card0, Name: "RTX 5060 Ti", UtilPct: 0, UtilKnown: true, MemTotalMiB: 16311},
			{Index: 1, UUID: card1, Name: "RTX 5070 Ti", UtilPct: 33, UtilKnown: true, MemTotalMiB: 16303, DisplayAttached: true},
			{Index: 2, UUID: card2, Name: "RTX 5060 Ti", UtilPct: 0, UtilKnown: true, MemTotalMiB: 16311},
		},
	}
	verdict, note := Assess(view)
	if verdict == VerdictHeldIdle {
		t.Fatalf("verdict = %q: an attached monitor alone must not hide the load on its card\nnote: %s", verdict, note)
	}
	if strings.Contains(note, "display card") {
		t.Errorf("nothing is displaying on card 1, the note must not call it the display card: %s", note)
	}
	// With the display INITIALISED on that card the load is the operator's, as it always was.
	view.GPUs[1].DisplayAttached = false
	view.GPUs[1].DisplayActive = true
	if v, n := Assess(view); v != VerdictHeldIdle || !strings.Contains(n, "33% on card 1") {
		t.Errorf("an initialised display on card 1 must still be excluded: %q\n%s", v, n)
	}
}
