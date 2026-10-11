package pipeline

// F56: what the calibration does with the store's host peaks. The store now answers with a high percentile of a window of
// recent runs (internal/fleetnode: one inflated run is not the answer); here the declaration reads it, is CLAMPED, and says so
// once. The clamp: a measurement raises a declaration above what the model files add up to, at most to hostPeakMaxRaise times
// that sum. A render whose process tree reads three times its files has an estimate that is wrong or a sampler that is
// charging card memory to the process, and either is a finding for the estimate, not a number to declare: an unclamped
// figure above what the box can ever admit makes the lane read "can never admit" for every later call, with
// `gpu reserve --ram` no remedy for a pipeline call. And the log: the note said "declaring host RAM: ..." keyed on a text that
// carried the run count, so every render added a map entry and a log line for the life of a fleet-serve.

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

func oneSixteenGiBCard() []gpuprobe.Card {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{{Index: 0, UUID: admitUUIDA, Name: "A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true}}, admitOrder)
	return cards
}

// One inflated run among twelve is not what the lane declares (the store's percentile), and neither is the clamp.
func TestOneInflatedRunDoesNotRaiseTheDeclaration(t *testing.T) {
	p, cfg := calibrationFixture(t, 0, 0, 0)
	store := p.FootprintStore()
	store.RecordHost("krea2", "", "image-gen", 200, 150) // the inflated run
	for i := 0; i < 11; i++ {
		store.RecordHost("krea2", "", "image-gen", 60, 40)
	}
	got := declaredFor(t, p, cfg, 16)
	if math.Abs(got.GiB-40) > 1e-9 || got.Source != hostneed.SourceMeasured {
		t.Fatalf("eleven runs at 40 GiB resident and one at 150: declared %+v, want 40 GiB measured", got)
	}
}

// The raise is clamped at hostPeakMaxRaise times the files' sum, and the declaration says it was.
func TestAMeasurementIsClampedAtAMultipleOfTheFilesSum(t *testing.T) {
	p, cfg := calibrationFixture(t, 12, 130, 100) // consistently 100 GiB resident against 32.8 GiB of files
	got := declaredFor(t, p, cfg, 16)
	want := 32.8 * hostPeakMaxRaise
	if math.Abs(got.GiB-want) > 0.01 || got.Source != hostneed.SourceMeasured {
		t.Fatalf("declared %+v, want the clamp %.1f GiB (%.0fx the files' 32.8 GiB)", got, want, hostPeakMaxRaise)
	}
	for _, w := range []string{"clamped", "100.0 GiB", "90th percentile", "last 12 runs"} {
		if !strings.Contains(got.Detail, w) {
			t.Errorf("the detail must say what happened (%q): %s", w, got.Detail)
		}
	}
	// A measurement under the clamp is declared as it is, and says nothing of one.
	p2, cfg2 := calibrationFixture(t, 12, 60, 40)
	if got := declaredFor(t, p2, cfg2, 16); math.Abs(got.GiB-40) > 1e-9 || strings.Contains(got.Detail, "clamped") {
		t.Errorf("40 GiB is under the clamp: %+v", got)
	}
}

// The note is made once per key and declared figure, however many renders (and so recorded runs) lie between two
// declarations; a figure that moves is said again.
func TestTheCalibrationNoteIsMadeOncePerKeyAndFigureNotPerRender(t *testing.T) {
	p, cfg := calibrationFixture(t, 0, 0, 0)
	store := p.FootprintStore()
	cards := oneSixteenGiBCard()
	need := imageNeed(cfg, "")
	buf := captureLogLines(t)
	declare := func() { p.declaredRAM(context.Background(), need, cards, nil) }
	count := func() int { return len(linesWith(buf, "declaring host RAM: ")) }

	for i := 0; i < 6; i++ { // six renders: each records a run, each declares
		store.RecordHost("krea2", "", "image-gen", 57.7, 44.4)
		if i >= hostPeakMinRuns-1 {
			declare()
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("four declarations of one figure over four different run counts must leave one line, got %d:\n%s", n, buf.String())
	}
	for i := 0; i < 20; i++ { // the window moves to a new figure
		store.RecordHost("krea2", "", "image-gen", 60, 47.7)
	}
	declare()
	declare()
	if n := count(); n != 2 {
		t.Fatalf("a figure that moved is said once more, got %d lines in all:\n%s", n, buf.String())
	}
}
