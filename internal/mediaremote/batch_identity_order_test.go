package mediaremote

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// The batch twin of the single call's order (F59): a FREE LANE NEVER WAITS ON A FINGERPRINT. The batch asks the lane before it
// builds this machine's identity, because the identity waits on the fingerprints of the weight files (up to 20 s for each on a
// cold cache) and a free lane runs the whole batch warm on this machine without ever needing one. Only the single call's door
// was pinned: moving the identity above the lane question in chooseBatchMode passed every test of the package.
func TestBatchOnAFreeLaneNeverWaitsOnAFingerprintRead(t *testing.T) {
	e := newQEnv(t, 1, false)
	e.lr.set(core.LaneVerdict{Free: true})

	gate := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	var reads atomic.Int32
	swapLocalReaders(t, func(ctx context.Context, path string, size int64) string {
		reads.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return mediacap.FingerprintPending
	}, func(path string, size int64) string {
		reads.Add(1)
		return mediacap.FingerprintPending
	})

	dir := t.TempDir()
	jobs := []BatchJob{{Prompt: "a red door", Out: filepath.Join(dir, "a.png"), Seed: 11}, {Prompt: "a blue door", Out: filepath.Join(dir, "b.png"), Seed: 22}}
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true, Ms: 7}
		}
		return rows, false, nil
	}
	var b *Batch
	var err error
	inTime(t, 3*time.Second, "a batch on a free lane, every fingerprint read held", func() struct{} {
		b, err = RunBatch(bctx(t), e.cfg, e.lr, jobs, warm, batchOpts())
		return struct{}{}
	})
	if err != nil || b.Mode != ModeWarm || b.OK() != 2 {
		t.Fatalf("mode=%q ok=%d err=%v, want the whole batch warm on this machine", b.Mode, b.OK(), err)
	}
	if got := reads.Load(); got != 0 {
		t.Errorf("a free lane read %d weight file(s): the identity is not needed until the lane has read busy", got)
	}
	untouched(t, "the idle node", e.nodes[0])
}

// ...AND A BUSY LANE STILL BUILDS IT, AFTER THE LANE WAS ASKED: the control of the test above (the reads are on the batch path at
// all, so zero reads above means the order and not a seam that nothing reaches), in the order the lane first and the files after.
func TestBatchOnABusyLaneReadsTheFingerprintsOnlyAfterTheLaneWasAsked(t *testing.T) {
	e := newQEnv(t, 1, false, sizedPNGNodes(t))

	var mu sync.Mutex
	var order []string
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		if len(order) == 0 || order[len(order)-1] != what {
			order = append(order, what)
		}
	}
	prev := localFingerprints
	swapLocalReaders(t, func(ctx context.Context, path string, size int64) string {
		note("read")
		return prev(ctx, path, size)
	}, localPeek)
	lr := &orderedKeeper{keeperRunner: e.lr, note: note}

	b, err := RunBatch(bctx(t), e.cfg, lr, manyJobs(t, 1), neverWarm(t), batchOpts())
	if err != nil || b.Mode != ModeTickets || b.OK() != 1 {
		t.Fatalf("mode=%q ok=%d err=%v, want one ticket placed on the node", b.Mode, b.OK(), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "lane" || order[1] != "read" {
		t.Errorf("the lane is asked first and the weight files are read after it said busy: %v", order)
	}
}

// orderedKeeper is a keeperRunner that says when it is asked the lane question.
type orderedKeeper struct {
	*keeperRunner
	note func(string)
}

func (o *orderedKeeper) MediaLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	o.note("lane")
	return o.keeperRunner.MediaLaneFree(ctx, req)
}
