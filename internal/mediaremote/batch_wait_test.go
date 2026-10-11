package mediaremote

// The fix round of the fleet batch (P0 plan S3; ADR 0083 decision 12). A busy place is WAITED for, never turned into a
// failed row: where jobs go one at a time through the single-render call (route remote, a machine with no image lane) a
// held card or a fleet with no free node is retried with a backoff until the job is placed or the queue's own ceiling ends,
// and an interrupted wait leaves the job queued. A serial row carries what the node that rendered it said about the recipe,
// not this machine's binding of a name nobody compared. Two spellings of one out are one file.

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fastWaits shortens the pauses between two asks of a serial job to milliseconds.
func fastWaits(t *testing.T) {
	t.Helper()
	prev := batchWaitSteps
	t.Cleanup(func() { batchWaitSteps = prev })
	batchWaitSteps = []time.Duration{2 * time.Millisecond, 5 * time.Millisecond}
}

// scriptedCall installs a single-render stand-in that answers its n-th call (1-based) with script(n, req).
func scriptedCall(t *testing.T, script func(n int, req core.Request) core.Result) *atomic.Int32 {
	t.Helper()
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	var n atomic.Int32
	batchCall = func(_ context.Context, _ config.Config, _ Runner, req core.Request, _ string, _ []string, _ Wait) core.Result {
		return script(int(n.Add(1)), req)
	}
	return &n
}

// renderedAt is a node's success: the picture is written at the job's out and the result names it, with what else the
// node said.
func renderedAt(t *testing.T, req core.Request, extra map[string]any) core.Result {
	t.Helper()
	out, _ := req.Params["out"].(string)
	writeBatchPNG(t, out, 8, 8)
	m := map[string]any{"image_path": out, "node": "node-a"}
	for k, v := range extra {
		m[k] = v
	}
	data, _ := json.Marshal(m)
	return core.Result{OK: true, Data: data}
}

// busyNode is a node's answer when its card is held: the job was accepted and deferred gpu_busy.
func busyNode() core.Result {
	res := core.Deferf("gpu busy: card c2 is held", "", core.Meta{ErrClass: core.ErrClassGPUBusy})
	res.DeferClass = core.DeferClassCapacity
	return res
}

// noNodeAdmits is the delegator's own answer when no node can take the job now (unreachable, text-leased): a capacity
// defer that carries no error class, as pickNodeFor's placementError does.
func noNodeAdmits() core.Result {
	res := core.Deferf("no fleet node serves generate_image: probed node-a: a text lease holds its card", "", core.Meta{Placement: "remote: forced"})
	res.DeferClass = core.DeferClassCapacity
	return res
}

func serialOpts(extra ...func(*BatchOpts)) BatchOpts {
	return batchOpts(append([]func(*BatchOpts){func(o *BatchOpts) { o.Route = "remote" }}, extra...)...)
}

// A held card and a fleet with no free node are a place in line, not a failure: the job is asked again after a pause until
// it is placed. The lines say the job waits, and the batch ends 0 with every picture there.
func TestBatchSerialWaitsForAPlaceAndNeverFailsTheJob(t *testing.T) {
	e := newQEnv(t, 1, false)
	fastWaits(t)
	calls := scriptedCall(t, func(n int, req core.Request) core.Result {
		switch {
		case n <= 2:
			return busyNode()
		case n == 3:
			return noNodeAdmits()
		}
		return renderedAt(t, req, nil)
	})
	var lines []string
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 2), neverWarm(t), serialOpts(func(o *BatchOpts) {
		o.Notify = func(l string) { lines = append(lines, l) }
	}))
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != ModeSerial || b.OK() != 2 || b.Failed() != 0 || b.ExitCode() != 0 {
		t.Fatalf("mode=%q ok=%d failed=%d exit=%d rows=%+v, want both pictures and exit 0", b.Mode, b.OK(), b.Failed(), b.ExitCode(), b.Rows)
	}
	// 3 asks that found no place, then job 0 placed (4th), then job 1 placed at once (5th).
	if calls.Load() != 5 {
		t.Errorf("%d call(s), want 5 (three waits, then each job placed once)", calls.Load())
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "job 0: no place for it yet") || strings.Contains(joined, "lane was taken by another job") {
		t.Errorf("the lines must say job 0 waits for a place, in the serial mode's words: %q", lines)
	}
	// The pause grows with the asks that found nothing (2 ms, then 5 ms, the last step holding), it is not a fixed beat.
	var pauses []string
	for _, l := range lines {
		if i := strings.Index(l, "asking again in "); i >= 0 {
			pauses = append(pauses, l[i+len("asking again in "):])
		}
	}
	if len(pauses) != 3 || pauses[0] != "2ms" || pauses[1] != "5ms" || pauses[2] != "5ms" {
		t.Errorf("the pauses between asks = %q, want 2ms, 5ms, then the last step holding at 5ms", pauses)
	}
	if !strings.Contains(joined, "recipe is not compared") {
		t.Errorf("the serial mode must say the node's recipe is not compared: %q", lines)
	}
}

// A job that has waited the queue's own ceiling without a place ends `queued`, never `failed`: nothing was wrong with it,
// the fleet had no room, and --resume renders it. The exit says the batch is unfinished.
func TestBatchSerialEndsQueuedAtTheCeilingNeverFailed(t *testing.T) {
	e := newQEnv(t, 1, false)
	fastWaits(t)
	prev := ticketMaxWait
	t.Cleanup(func() { ticketMaxWait = prev })
	ticketMaxWait = 80 * time.Millisecond
	calls := scriptedCall(t, func(int, core.Request) core.Result { return busyNode() })
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 2), neverWarm(t), serialOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.Queued() != 2 || b.Failed() != 0 || b.ExitCode() != 1 {
		t.Fatalf("queued=%d failed=%d exit=%d rows=%+v, want both jobs queued, none failed, exit 1", b.Queued(), b.Failed(), b.ExitCode(), b.Rows)
	}
	for _, r := range b.Rows {
		if r.OK || !strings.Contains(r.Error, "waited") || !strings.Contains(r.Error, "gpu busy") {
			t.Errorf("row %+v, want the wait and the node's own reason in the error", r)
		}
	}
	if calls.Load() < 4 {
		t.Errorf("%d call(s): each job must have been asked more than once before it gave up", calls.Load())
	}
}

// Only a place that is not there is waited for. Anything else the call answers is final at once, as it was: a node that
// cannot be reached after the job was sent, a refusal of the contract, a render that failed.
func TestBatchSerialWaitsOnlyForAPlace(t *testing.T) {
	infra := core.Deferf("node node-a: dispatch: connection refused", "", core.Meta{})
	infra.DeferClass = core.DeferClassInfrastructure
	oom := core.Deferf("CUDA out of memory", "", core.Meta{ErrClass: "oom"})
	oom.DeferClass = core.DeferClassCapacity // a class of its own, not a held card, even under the capacity defer class
	for name, res := range map[string]core.Result{
		"infrastructure":                  infra,
		"a render that ran out of memory": oom,
		"a plain failure":                 {OK: false, Reason: "the node reported a broken checkpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQEnv(t, 1, false)
			fastWaits(t)
			calls := scriptedCall(t, func(int, core.Request) core.Result { return res })
			b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), serialOpts())
			if err != nil {
				t.Fatal(err)
			}
			if b.Failed() != 1 || b.Queued() != 0 || calls.Load() != 1 {
				t.Errorf("failed=%d queued=%d calls=%d, want one call and a failed row", b.Failed(), b.Queued(), calls.Load())
			}
		})
	}
}

// An interrupt ends a wait at once: the job is queued, the batch is interrupted, exit 1, and nobody sleeps out a pause.
func TestBatchSerialInterruptEndsTheWait(t *testing.T) {
	e := newQEnv(t, 1, false)
	prev := batchWaitSteps
	t.Cleanup(func() { batchWaitSteps = prev })
	batchWaitSteps = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(bctx(t))
	t.Cleanup(cancel)
	scriptedCall(t, func(n int, _ core.Request) core.Result {
		if n == 1 {
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		}
		return busyNode()
	})
	done := make(chan *Batch, 1)
	go func() {
		b, _ := RunBatch(ctx, e.cfg, e.lr, manyJobs(t, 3), neverWarm(t), serialOpts())
		done <- b
	}()
	var b *Batch
	select {
	case b = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the interrupt did not end a wait of an hour")
	}
	if b.Queued() != 3 || b.Failed() != 0 || !b.Interrupted || b.ExitCode() != 1 {
		t.Errorf("queued=%d failed=%d interrupted=%v exit=%d rows=%+v, want all three queued and exit 1", b.Queued(), b.Failed(), b.Interrupted, b.ExitCode(), b.Rows)
	}
}

// ---- what a serial row says about the recipe -------------------------------------------------------------------------------

// Where the recipe was never compared (route remote, no image lane), the node that rendered the picture is the only one
// that knows its family and licence, so the row carries ITS words. The local default binding is not the recipe that ran: a
// node whose own default is another family rendered a batch that named none.
func TestBatchSerialRowsCarryTheNodesOwnFamilyAndLicense(t *testing.T) {
	e := newQEnv(t, 1, false)
	noFamily := func(o *BatchOpts) { o.Family = "" }

	scriptedCall(t, func(_ int, req core.Request) core.Result {
		return renderedAt(t, req, map[string]any{"family": "hidream-o1", "license": "Node License", "commercial_use": false})
	})
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), serialOpts(noFamily))
	if err != nil {
		t.Fatal(err)
	}
	r := b.Rows[0]
	if r.Verdict != VerdictRendered || r.Family != "hidream-o1" || r.License != "Node License" || r.CommercialUse == nil || *r.CommercialUse {
		t.Errorf("row %+v, want the node's family, licence and commercial_use=false", r)
	}

	// A node that says nothing about it: the row says nothing, it does not claim this machine's binding.
	scriptedCall(t, func(_ int, req core.Request) core.Result { return renderedAt(t, req, nil) })
	b, err = RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), serialOpts(noFamily))
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Rows[0]; r.Verdict != VerdictRendered || r.Family != "" || r.License != "" || r.CommercialUse != nil {
		t.Errorf("row %+v, want no family and no licence claimed for a recipe nobody compared", r)
	}

	// The ticket path compared the recipe, so it keeps naming the recipe by this machine's name for it, whatever name and
	// licence the node's result carries.
	e2 := newQEnv(t, 1, true) // every node busy: tickets
	scriptedCall(t, func(_ int, req core.Request) core.Result {
		return renderedAt(t, req, map[string]any{"family": "node-family", "license": "Node License", "commercial_use": true})
	})
	b, err = RunBatch(bctx(t), e2.cfg, e2.lr, manyJobs(t, 1), neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Rows[0]; b.Mode != ModeTickets || r.Family != batchFamily || r.License != "" || r.CommercialUse != nil {
		t.Errorf("mode=%q row %+v, want the local family name and no node licence on a ticket row", b.Mode, r)
	}
}

// A family this machine cannot resolve is carried to a node by name, as a single render carries it: a thin client has no
// binding for a family only the nodes define. A machine that renders locally still refuses a name it does not know.
func TestBatchCarriesAFamilyThisMachineCannotResolveWhereItCannotRenderLocally(t *testing.T) {
	const nodeOnly = "node-only-family"
	e := newQEnv(t, 1, false)
	scriptedCall(t, func(_ int, req core.Request) core.Result {
		if got, _ := req.Params["family"].(string); got != nodeOnly {
			t.Errorf("the family reached the call as %q, want %q", got, nodeOnly)
		}
		return renderedAt(t, req, map[string]any{"family": nodeOnly, "license": "Node License"})
	})
	named := func(o *BatchOpts) { o.Family = nodeOnly }

	t.Run("route remote", func(t *testing.T) {
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), serialOpts(named))
		if err != nil {
			t.Fatalf("a hand-forced remote batch must carry the name: %v", err)
		}
		if r := b.Rows[0]; b.Mode != ModeSerial || r.Verdict != VerdictRendered || r.Family != nodeOnly || r.License != "Node License" {
			t.Errorf("mode=%q row %+v, want a serial row with the node's family", b.Mode, r)
		}
	})
	t.Run("a thin client under auto", func(t *testing.T) {
		stubLocalRoutes(t, configured()) // no image lane here
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts(named))
		if err != nil {
			t.Fatalf("a thin client must carry the name: %v", err)
		}
		if b.Mode != ModeSerial || b.OK() != 1 {
			t.Errorf("mode=%q ok=%d, want the serial mode", b.Mode, b.OK())
		}
	})
	t.Run("a thin client under an inherited lease still goes to a node", func(t *testing.T) {
		// A lease the process inherited keeps a batch warm where there is a lane to run it; a family only the nodes know has
		// no lane here, and the warm call would refuse it.
		stubLocalRoutes(t, configured())
		t.Setenv("GPU_LEASE_DIR", t.TempDir())
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts(named))
		if err != nil || b.Mode != ModeSerial || b.OK() != 1 {
			t.Errorf("err=%v mode=%q ok=%d, want the serial mode", err, b.Mode, b.OK())
		}
	})
	t.Run("a machine with a lane still refuses a name it does not know", func(t *testing.T) {
		stubLocalRoutes(t, configured("generate_image"))
		_, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts(named))
		if err == nil || !strings.Contains(err.Error(), "unknown image family") {
			t.Errorf("err = %v, want the unknown family refused", err)
		}
	})
}

// ---- outs ------------------------------------------------------------------------------------------------------------------

// Two spellings of one file are one out: a relative path and the absolute path it stands for would otherwise let the
// second picture silently replace the first.
func TestBatchOutsThatNameOneFileTwoWaysClash(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".", "outs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rel := filepath.Join(filepath.Base(dir), "pic.png")
	abs := filepath.Join(cwd, rel)
	warm := func(context.Context, []BatchJob, time.Duration) ([]BatchRow, bool, error) {
		t.Error("the warm call ran for a batch with two pictures for one file")
		return nil, false, nil
	}
	spellings := map[string][2]string{
		"relative and absolute": {rel, abs},
		"a dot segment":         {rel, filepath.Join(filepath.Base(dir), ".", "sub", "..", "pic.png")},
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		spellings["case and absolute"] = [2]string{strings.ToUpper(rel), abs}
	}
	for name, outs := range spellings {
		t.Run(name, func(t *testing.T) {
			_, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, []BatchJob{
				{Prompt: "a", Out: outs[0], Seed: 1}, {Prompt: "b", Out: outs[1], Seed: 2},
			}, warm, BatchOpts{Route: "local"})
			if err == nil || !strings.Contains(err.Error(), "same out") {
				t.Fatalf("want the clash named, got %v", err)
			}
		})
	}
	// Two identical jobs for one file are one picture twice, whichever way the file is spelled.
	dup := func(context.Context, []BatchJob, time.Duration) ([]BatchRow, bool, error) {
		return []BatchRow{{Out: abs, Seed: 1, Error: "stub"}, {Out: abs, Seed: 1, Error: "stub"}}, false, nil
	}
	if _, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, []BatchJob{
		{Prompt: "a", Out: rel, Seed: 1}, {Prompt: "a", Out: abs, Seed: 1},
	}, dup, BatchOpts{Route: "local"}); err != nil {
		t.Errorf("the same picture named two ways is not a clash: %v", err)
	}
}

// ---- the warm path ---------------------------------------------------------------------------------------------------------

// A warm batch interrupted while it waits for the lane has not failed: its jobs are queued, exit 1, and --resume renders
// them.
func TestBatchWarmInterruptedWhileWaitingForTheLaneQueuesTheRows(t *testing.T) {
	ctx, cancel := context.WithCancel(bctx(t))
	cancel()
	warm := func(ctx context.Context, _ []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		return nil, false, ctx.Err()
	}
	b, err := RunBatch(ctx, config.Config{StateDir: t.TempDir()}, nil, manyJobs(t, 3), warm, BatchOpts{Route: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Queued() != 3 || b.Failed() != 0 || !b.Interrupted || b.ExitCode() != 1 {
		t.Errorf("queued=%d failed=%d interrupted=%v exit=%d rows=%+v, want three queued rows and exit 1", b.Queued(), b.Failed(), b.Interrupted, b.ExitCode(), b.Rows)
	}
}

// ---- the resume proof ------------------------------------------------------------------------------------------------------

// writeHugePNG writes a valid 1-bit grey PNG of w x h whose rows are all zero, so it is a few KB on disk and decodes to a
// canvas of w*h bytes: what a stray oversized file at an out path looks like to a resume pre-pass.
func writeHugePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	chunk := func(buf *bytes.Buffer, typ string, data []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(data)))
		buf.Write(n[:])
		buf.WriteString(typ)
		buf.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], crc.Sum32())
		buf.Write(c[:])
	}
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 1, 0 // 1 bit per pixel, grey
	var idat bytes.Buffer
	zw, err := zlib.NewWriterLevel(&idat, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	row := make([]byte, 1+(w+7)/8)
	for y := 0; y < h; y++ {
		if _, err := zw.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	chunk(&out, "IHDR", ihdr[:])
	chunk(&out, "IDAT", idat.Bytes())
	chunk(&out, "IEND", nil)
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A file that cannot be the picture a job names is judged by its header and rendered again without being decoded: a decode
// allocates the whole canvas (400 MB here), and the operator's rule is no RAM overload. A canvas past the ceiling is never
// decoded, whether the job names a size or not.
func TestResumeDoesNotDecodeACanvasItCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.png")
	writeHugePNG(t, path, 20000, 20000)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	named := pngVerifies(path, 64, 32)
	unnamed := pngVerifies(path, 0, 0)
	runtime.ReadMemStats(&after)
	if named || unnamed {
		t.Errorf("a 20000x20000 file was taken for a finished picture (named size %v, no size %v)", named, unnamed)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 32<<20 {
		t.Errorf("the proof allocated %d MB for a file it should have judged by its header", got>>20)
	}
	// a file of the right, small size still proves itself
	ok := filepath.Join(t.TempDir(), "ok.png")
	writeBatchPNG(t, ok, 64, 32)
	if !pngVerifies(ok, 64, 32) || !pngVerifies(ok, 0, 0) {
		t.Errorf("a whole 64x32 PNG must still prove itself")
	}
}
