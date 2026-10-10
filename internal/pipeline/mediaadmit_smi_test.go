package pipeline

// F24, the incident on the real read path. Every other test of the card-table read in this package injects a
// reader (flakyTable, tableScript), so the production reader that a Pipeline falls back to (gpualloc.ReadCards:
// the PATH lookup, exec.CommandContext, the kill at the deadline, the parse) never ran under a media call, and
// the claim that matters, that a slow nvidia-smi under load queues a call instead of deferring it, rested on
// stand-ins for the very thing that failed. Here the process behind it is a stand-in nvidia-smi
// (internal/gpuprobe/smitest: this test binary, first on PATH) that hangs or answers as the script says. The
// error text below is the incident's own, produced by the real code. No card, no GPU, nothing rendered.

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// incidentWords is what offload_generate_image answered on 2026-10-09: the allocation's re-read of the table, killed
// at its deadline, as gpuprobe words it.
const incidentWords = "the card table: nvidia-smi: nvidia-smi: context deadline exceeded"

// notListing fails the test unless every nvidia-smi call the media path made was the one per-device query.
func notListing(t *testing.T, calls [][]string) {
	t.Helper()
	for _, c := range calls {
		if len(c) == 0 || !strings.HasPrefix(c[0], "--query-gpu=") {
			t.Errorf("the media path ran nvidia-smi %q, want only the per-device query: it never lists processes", c)
		}
	}
}

// The incident: two cards held by other sessions' renders, the call's admission read answers, the allocation's
// first read hangs and is killed at its deadline, and the read once more answers. The call queues with a token
// like the calls around it, and nothing says it was degraded, because it was not.
func TestAnAllocationReadThatHangsOnceIsReadAgainAndTheCallQueuesNormally(t *testing.T) {
	logs := captureAdmissionLog(t)
	f := newAdmitFixture(t, nil)
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	f.p.alloc.Cards = nil // the production reader
	f.shortReads(4*time.Second, 9*time.Second)
	st := smitest.Install(t,
		smitest.Step{},                        // the admission read
		smitest.Step{Delay: 30 * time.Second}, // the allocation's first attempt: never answers, killed at its deadline
		smitest.Step{},                        // its retry
	)

	start := time.Now()
	res := f.await(f.image(nil))
	took := time.Since(start)
	p := wantQueued(t, res)
	if len(p.Devices) != 1 || (p.Devices[0] != leaseIDOf(admitUUIDA) && p.Devices[0] != leaseIDOf(admitUUIDC)) {
		t.Errorf("it waits for one of the two held cards, got %v", p.Devices)
	}
	if strings.Contains(res.Reason, "could not be re-read") || strings.Contains(res.Reason, incidentWords) {
		t.Errorf("the retry answered, so nothing was degraded and the incident's error is not in the answer: %s", res.Reason)
	}
	if !strings.Contains(logs.String(), "reading it once more under 9s") {
		t.Errorf("the retry is logged:\n%s", logs.String())
	}
	calls := st.Calls()
	if len(calls) != 3 {
		t.Errorf("%d nvidia-smi calls %q, want 3: admission, the allocation's attempt that was killed, its retry", len(calls), calls)
	}
	notListing(t, calls)
	if took < 3900*time.Millisecond {
		t.Errorf("the call took %v: the hung read was not waited out to its deadline", took.Round(time.Millisecond))
	}
}

// The same incident when the read never answers: it is read twice and the call is placed from the table it holds
// and queued, with the incident's own words in the reason where they used to be a hard defer.
func TestAnAllocationReadThatNeverAnswersQueuesTheCallAndCarriesTheIncidentsWords(t *testing.T) {
	f := newAdmitFixture(t, nil)
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	f.p.alloc.Cards = nil
	f.shortReads(3*time.Second, 5*time.Second)
	st := smitest.Install(t,
		smitest.Step{},                        // the admission read
		smitest.Step{Delay: 30 * time.Second}, // every read after it never answers
	)

	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 1 {
		t.Errorf("devices = %v", p.Devices)
	}
	for _, want := range []string{
		"could not be re-read",
		strings.TrimPrefix(incidentWords, "the card table: "),
		"read twice: no answer within 3s, then none within 5s",
	} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the answer must carry %q: %s", want, res.Reason)
		}
	}
	calls := st.Calls()
	if len(calls) != 3 {
		t.Errorf("%d nvidia-smi calls %q, want 3: admission, then the allocation's attempt and its retry, both killed", len(calls), calls)
	}
	notListing(t, calls)
}
