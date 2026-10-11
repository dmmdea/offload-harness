package mediaremote

// The image batch across the fleet (P0 plan S3; ADR 0083 decision 13). Everything below runs against real fleet-node
// servers (their doors, job store and recipe advertisement) whose leases the test toggles, a delegator whose pipeline is
// the S2 runner that answers the lane question, and the real ticket files in a temp state dir. The warm call, the one
// thing that needs a ComfyUI, is a function the test hands in and counts.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	pngenc "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/imagegen"
)

// batchPNG encodes a real PNG of the given size.
func batchPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	b, err := encodePNG(w, h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func encodePNG(w, h int) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.NRGBA{R: uint8(x), G: 200, B: 30, A: 255})
	}
	var buf bytes.Buffer
	if err := pngenc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeBatchPNG(t testing.TB, path string, w, h int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, batchPNG(t, w, h), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sizedPNGNodes makes every node of a qEnv render a real PNG of the size the job asked for.
func sizedPNGNodes(t *testing.T) func(i int, o *nodeOpts) {
	return func(_ int, o *nodeOpts) {
		o.imageBody = func(req core.Request) []byte {
			w, _ := posInt(req.Params, "width")
			h, _ := posInt(req.Params, "height")
			if w == 0 || h == 0 {
				w, h = 8, 8
			}
			return batchPNG(t, w, h)
		}
	}
}

const batchFamily = "qwen-image-2.1-fast"

// neverWarm is the warm call of a test that must not reach it.
func neverWarm(t *testing.T) WarmFunc {
	return func(context.Context, []BatchJob, time.Duration) ([]BatchRow, bool, error) {
		t.Error("the warm batch ran, but the jobs should have gone through tickets")
		return nil, false, nil
	}
}

// bctx is the context of a test batch, bounded so a regression that waits for ever ends the test in seconds.
func bctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func batchOpts(extra ...func(*BatchOpts)) BatchOpts {
	o := BatchOpts{Family: batchFamily}
	for _, f := range extra {
		f(&o)
	}
	return o
}

// ---- where the batch runs ------------------------------------------------------------------------------------------------

// A lane that is free at the start renders the WHOLE batch through the warm call, once, and asks the fleet nothing: no
// roster read, no ticket, no job on a node.
func TestBatchLocalWhenLaneFree(t *testing.T) {
	e := newQEnv(t, 1, false)
	e.lr.set(core.LaneVerdict{Free: true})
	dir := t.TempDir()
	jobs := []BatchJob{
		{Prompt: "a red door", Out: filepath.Join(dir, "a.png"), Seed: 11},
		{Prompt: "a blue door", Out: filepath.Join(dir, "b.png"), Seed: 22},
		{Prompt: "a green door", Out: filepath.Join(dir, "c.png"), Seed: 33},
	}
	var calls int
	var got []BatchJob
	var gotWait time.Duration
	warm := func(_ context.Context, js []BatchJob, wait time.Duration) ([]BatchRow, bool, error) {
		calls++
		got = js
		gotWait = wait
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true, Ms: 7}
		}
		return rows, false, nil
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, warm, batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != ModeWarm || calls != 1 || len(got) != 3 {
		t.Fatalf("mode=%q warm calls=%d jobs=%d, want one warm call with all three jobs", b.Mode, calls, len(got))
	}
	for i, r := range b.Rows {
		if !r.OK || r.Verdict != VerdictRendered || r.I != i || r.Out != jobs[i].Out || r.Seed != jobs[i].Seed || r.SHA256 == "" || r.Node == "" || r.Family != "qwen-image-2.1-fast" {
			t.Errorf("row %d = %+v, want a rendered row with the job's out and seed, a hash, the node that rendered it and the family", i, r)
		}
	}
	untouched(t, "the idle node", e.nodes[0])
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("%d ticket file(s) for a batch that ran warm", n)
	}
	if probes, runs := e.lr.counts(); probes != 1 || runs != 0 {
		t.Errorf("lane probes=%d single runs=%d, want one probe and no single render", probes, runs)
	}
	if gotWait != 0 {
		t.Errorf("a free lane asked the warm call to wait %v, want its own window (0)", gotWait)
	}
}

// route local never asks the fleet, whatever the lane says; a box with no fleet is the batch it always was.
func TestBatchLocalRouteAndNoFleetRunWarmWithoutAskingTheLane(t *testing.T) {
	e := newQEnv(t, 1, false) // the lane is HELD and a node is idle: the two would make tickets under auto
	dir := t.TempDir()
	jobs := []BatchJob{{Prompt: "a red door", Out: filepath.Join(dir, "a.png"), Seed: 5}}
	var waits []time.Duration
	warm := func(_ context.Context, js []BatchJob, wait time.Duration) ([]BatchRow, bool, error) {
		waits = append(waits, wait)
		writeBatchPNG(t, js[0].Out, 8, 8)
		return []BatchRow{{Out: js[0].Out, Seed: js[0].Seed, OK: true}}, false, nil
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, warm, batchOpts(func(o *BatchOpts) { o.Route = "local" }))
	if err != nil || b.Mode != ModeWarm {
		t.Fatalf("route local: mode=%q err=%v, want the warm batch", b.Mode, err)
	}
	untouched(t, "the idle node", e.nodes[0])
	if probes, _ := e.lr.counts(); probes != 0 {
		t.Errorf("route local asked the lane %d time(s); it has nothing to decide", probes)
	}

	noFleet := e.cfg
	noFleet.DelegateRemotes = nil
	var lines []string
	b, err = RunBatch(bctx(t), noFleet, e.lr, jobs, warm, batchOpts(func(o *BatchOpts) { o.Notify = func(l string) { lines = append(lines, l) } }))
	if err != nil || b.Mode != ModeWarm || len(lines) != 0 {
		t.Fatalf("no fleet: mode=%q err=%v lines=%q, want the warm batch without a word", b.Mode, err, lines)
	}
	// What the caller asked for by route, or what has no fleet, waits the pipeline's own window as it always did: only a
	// busy lane that no node can relieve makes the batch hold its place (see the next section).
	for i, w := range waits {
		if w != 0 {
			t.Errorf("warm call %d was asked to wait %v, want the pipeline's own window (0)", i, w)
		}
	}
}

// Each job of a batch whose lane is held becomes a ticket, and the node gets it exactly as the job was written: the
// seed, the negative, the size, refine=false, the family under the node's own name. The files land at the given outs, each
// row names the node and carries the hash of what is on disk.
func TestBatchJobsPlacedThroughTickets(t *testing.T) {
	e := newQEnv(t, 2, false, sizedPNGNodes(t))
	dir := t.TempDir()
	off, on := false, true
	jobs := []BatchJob{
		{Prompt: "a red door", Out: filepath.Join(dir, "one.png"), Seed: 101, Width: 1664, Height: 928, Negative: "people, text", Refine: &off, Steps: 20},
		{Prompt: "a blue door", Out: filepath.Join(dir, "sub", "two.png"), Seed: 102, Width: 1024, Height: 1024},
		{Prompt: "a green door", Out: filepath.Join(dir, "three.png"), Seed: 103, Width: 64, Height: 32, Refine: &on},
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != ModeTickets || b.InFlight != 3 {
		t.Fatalf("mode=%q in flight=%d, want tickets with 1 + 2 matching nodes = 3 in flight", b.Mode, b.InFlight)
	}
	if b.ExitCode() != 0 || b.Failed() != 0 {
		t.Fatalf("rows %+v", b.Rows)
	}

	var posts []seen
	for _, n := range e.nodes {
		posts = append(posts, n.posts()...)
	}
	if len(posts) != 3 {
		t.Fatalf("%d job(s) reached the nodes, want 3", len(posts))
	}
	bySeed := map[int]map[string]any{}
	for _, p := range posts {
		var wire struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(p.body, &wire); err != nil {
			t.Fatal(err)
		}
		bySeed[int(wire.Payload["seed"].(float64))] = wire.Payload
		if wire.Payload["family"] != "qwen-image-2.1" || wire.Payload["recipe_digest"] == "" {
			t.Errorf("the job must be sent under the node's own family name with the recipe digest: %v", wire.Payload)
		}
	}
	want := map[int]struct {
		w, h     float64
		negative string
		refine   bool // refine=false is sent, and only when the job said so
		steps    float64
	}{101: {1664, 928, "people, text", true, 20}, 102: {1024, 1024, "", false, 0}, 103: {64, 32, "", false, 0}}
	for seed, w := range want {
		p := bySeed[seed]
		if p == nil {
			t.Errorf("no job with seed %d reached a node: %v", seed, bySeed)
			continue
		}
		if got, _ := p["steps"].(float64); got != w.steps {
			t.Errorf("seed %d: steps = %v, want %v exactly (absent when the job has none)", seed, p["steps"], w.steps)
		}
		if p["width"] != w.w || p["height"] != w.h {
			t.Errorf("seed %d: size %vx%v, want %vx%v exactly", seed, p["width"], p["height"], w.w, w.h)
		}
		if w.negative == "" && p["negative"] != nil || w.negative != "" && p["negative"] != w.negative {
			t.Errorf("seed %d: negative = %v, want %q (absent when the job has none)", seed, p["negative"], w.negative)
		}
		if r, has := p["refine"]; w.refine && (!has || r != false) || !w.refine && has {
			t.Errorf("seed %d: refine = %v (present %v), want refine=false only on the job that set it", seed, r, has)
		}
	}
	for i, r := range b.Rows {
		data, err := os.ReadFile(jobs[i].Out)
		if err != nil {
			t.Fatalf("job %d: nothing at its out: %v", i, err)
		}
		cfgImg, err := pngenc.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfgImg.Width != jobs[i].Width || cfgImg.Height != jobs[i].Height {
			t.Errorf("job %d: the file at out is %v (%v), want a %dx%d PNG", i, cfgImg, err, jobs[i].Width, jobs[i].Height)
		}
		if r.Verdict != VerdictRendered || !r.OK || r.SHA256 != sha(data) || (r.Node != "node-a" && r.Node != "node-b") || r.Out != jobs[i].Out {
			t.Errorf("row %d = %+v, want rendered on a roster node, with the hash of the file at out", i, r)
		}
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("%d ticket file(s) left by a finished batch", n)
	}
}

// The results file holds one row per job in job order with every key, a row is on disk as soon as its job is final, and
// the keys the render script's results already had are the ones parseBatchResults reads.
func TestBatchWritesExactOutPathsAndResultsFile(t *testing.T) {
	e := newQEnv(t, 2, false, sizedPNGNodes(t))
	dir := t.TempDir()
	results := filepath.Join(dir, "deep", "r.jsonl")
	rf, err := OpenResults(results)
	if err != nil {
		t.Fatal(err)
	}
	jobs := []BatchJob{
		{Prompt: "p1", Out: filepath.Join(dir, "exact", "x1.png"), Seed: 7, Width: 64, Height: 32},
		{Prompt: "p2", Out: filepath.Join(dir, "exact", "x2.png"), Seed: 8, Width: 64, Height: 32},
	}
	var progressive []int
	var mu sync.Mutex
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts(func(o *BatchOpts) {
		o.OnRow = func(r BatchRow) {
			rf.Append(r)
			mu.Lock()
			progressive = append(progressive, r.I)
			mu.Unlock()
			// A row is on disk (in the partial file beside the target) by the time its callback returns.
			if raw, _ := os.ReadFile(results + ".partial"); !strings.Contains(string(raw), `"out":`) {
				t.Errorf("the results file has no row yet when row %d is final: %q", r.I, raw)
			}
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(b.Rows); err != nil {
		t.Fatal(err)
	}
	if len(progressive) != 2 {
		t.Errorf("OnRow ran %d time(s), want once per job", len(progressive))
	}
	raw, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("results file has %d line(s): %q", len(lines), raw)
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		for _, k := range []string{"i", "out", "seed", "ok", "ms", "node", "family", "sha256", "verdict"} {
			if _, has := row[k]; !has {
				t.Errorf("line %d lacks %q: %s", i, k, line)
			}
		}
		// The keys parseBatchResults reads, with the types it reads them as.
		var legacy struct {
			I     int    `json:"i"`
			Out   string `json:"out"`
			Seed  int    `json:"seed"`
			OK    bool   `json:"ok"`
			Ms    int64  `json:"ms"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &legacy); err != nil || legacy.I != i || legacy.Out != jobs[i].Out || legacy.Seed != jobs[i].Seed || !legacy.OK {
			t.Errorf("line %d does not read back as the legacy row: %+v (%v)", i, legacy, err)
		}
		if body, _ := os.ReadFile(jobs[i].Out); row["sha256"] != sha(body) {
			t.Errorf("line %d: sha256 %v is not the hash of the file at out (%s)", i, row["sha256"], sha(body))
		}
	}
}

// ---- the bound on jobs in flight --------------------------------------------------------------------------------------

// instantCall is a stand-in for the single-render call that renders (writes the out) after a short while and counts the
// calls in flight.
type instantCall struct {
	cur, peak atomic.Int32
	calls     atomic.Int32
}

func (c *instantCall) install(t *testing.T, hold time.Duration) {
	t.Helper()
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	batchCall = func(_ context.Context, _ config.Config, _ Runner, req core.Request, _ string, _ []string, _ Wait) core.Result {
		c.calls.Add(1)
		n := c.cur.Add(1)
		for {
			p := c.peak.Load()
			if n <= p || c.peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(hold)
		c.cur.Add(-1)
		out, _ := req.Params["out"].(string)
		writeBatchPNG(t, out, 8, 8)
		data, _ := json.Marshal(map[string]any{"image_path": out, "node": "node-a"})
		return core.Result{OK: true, Data: data}
	}
}

func manyJobs(t *testing.T, n int) []BatchJob {
	dir := t.TempDir()
	jobs := make([]BatchJob, n)
	for i := range jobs {
		jobs[i] = BatchJob{Prompt: "a door", Out: filepath.Join(dir, "j"+string(rune('a'+i))+".png"), Seed: i + 1}
	}
	return jobs
}

// At most N jobs are in flight, whatever the file lists: the default is this machine's lane plus one per node that
// matches, at most the cap; --jobs overrides it.
func TestBatchNeverHasMoreThanNInFlight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes int
		jobs  int // --jobs
		files int
		want  int // the bound, and the peak a 12-job file reaches
	}{
		{"default: this machine's lane and each matching node", 3, 0, 9, 4},
		{"--jobs lowers it", 3, 2, 9, 2},
		{"--jobs 1 is a queue of one", 3, 1, 5, 1},
		{"the default is capped", batchInFlightCap + 1, 0, batchInFlightCap + 4, batchInFlightCap},
		{"never more than there are jobs", 3, 0, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQEnv(t, tc.nodes, true) // every matching node busy: they match, and none takes a job
			c := &instantCall{}
			c.install(t, 40*time.Millisecond)
			b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, tc.files), neverWarm(t), batchOpts(func(o *BatchOpts) { o.Jobs = tc.jobs }))
			if err != nil {
				t.Fatal(err)
			}
			if b.Mode != ModeTickets || b.InFlight != tc.want {
				t.Fatalf("mode=%q in flight=%d, want tickets bounded at %d", b.Mode, b.InFlight, tc.want)
			}
			if got := int(c.peak.Load()); got != tc.want {
				t.Errorf("%d jobs were in flight at once, want exactly the bound %d", got, tc.want)
			}
			if int(c.calls.Load()) != tc.files || b.OK() != tc.files {
				t.Errorf("%d call(s) for %d jobs, %d ok", c.calls.Load(), tc.files, b.OK())
			}
		})
	}
}

// Where nothing claims a node for a job, the jobs run one at a time whatever --jobs asked, and the line says so: route
// remote, and a machine with no image lane (both go through the single-render call, whose pick of a node is not a claim).
func TestBatchOneAtATimeWhereNoClaimProtectsANode(t *testing.T) {
	e := newQEnv(t, 2, false)
	stubLocalRoutes(t, configured()) // this machine has no image lane (route remote does not look)
	for _, route := range []string{"remote", "auto"} {
		t.Run("route "+route, func(t *testing.T) {
			c := &instantCall{}
			c.install(t, 30*time.Millisecond)
			var lines []string
			b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 5), neverWarm(t), batchOpts(func(o *BatchOpts) {
				o.Route, o.Jobs = route, 4
				o.Notify = func(l string) { lines = append(lines, l) }
			}))
			if err != nil {
				t.Fatal(err)
			}
			if b.Mode != ModeSerial || b.InFlight != 1 || c.peak.Load() != 1 {
				t.Errorf("mode=%q in flight=%d peak=%d, want serial and one at a time", b.Mode, b.InFlight, c.peak.Load())
			}
			if !strings.Contains(strings.Join(lines, "\n"), "--jobs 4 does not apply") {
				t.Errorf("the caller asked for 4 and must be told it did not apply: %q", lines)
			}
		})
	}
}

// A batch never sends two jobs to one single-job node at once: with one idle node, the lane held and four jobs allowed in
// flight, the node holds one job until it ends, and the next goes only after.
func TestBatchNeverDispatchesTwoJobsToOneSingleJobNodeAtOnce(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold }, sizedPNGNodes(t))
	node := e.nodes[0]
	jobs := manyJobs(t, 3)
	type outcome struct {
		b   *Batch
		err error
	}
	done := make(chan outcome, 1)
	ctx := bctx(t)
	go func() {
		b, err := RunBatch(ctx, e.cfg, e.lr, jobs, neverWarm(t), batchOpts(func(o *BatchOpts) { o.Jobs = 4 }))
		done <- outcome{b, err}
	}()
	waitFor(t, 10*time.Second, "the first job to reach the node", func() bool { return len(node.posts()) >= 1 })
	// Many scans pass (40 ms each in a test) while the node holds its job; the others must not be sent.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := len(node.posts()); n != 1 {
			t.Fatalf("%d job(s) were sent to a node that holds one", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(hold)
	select {
	case r := <-done:
		if r.err != nil || r.b.OK() != 3 {
			t.Fatalf("err=%v rows=%+v", r.err, r.b.Rows)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the batch did not finish once the node was free")
	}
	if n := len(node.posts()); n != 3 {
		t.Errorf("%d job(s) reached the node in the end, want 3", n)
	}
}

// ---- krea2 and the 2512 build: no node renders them -----------------------------------------------------------------------

// A busy lane and no node that renders the recipe: the batch waits for the lane ONCE and runs warm on it, as before, and
// says which node does not match and why. Per-job tickets would be one cold start per picture on the same lane.
func TestBatchWithNoRemoteMatchRunsWarmOnThisMachineAndSaysWhy(t *testing.T) {
	e := newQEnv(t, 2, false) // the nodes bind the 2.1 family; the default binding of this machine is krea2, which they do not hold
	dir := t.TempDir()
	jobs := []BatchJob{{Prompt: "a red door", Out: filepath.Join(dir, "a.png"), Seed: 1}, {Prompt: "a blue door", Out: filepath.Join(dir, "b.png"), Seed: 2}}
	var calls int
	var lines []string
	var gotWait time.Duration
	warm := func(_ context.Context, js []BatchJob, wait time.Duration) ([]BatchRow, bool, error) {
		calls++
		gotWait = wait
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, warm, BatchOpts{Notify: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != ModeWarm || calls != 1 || b.OK() != 2 {
		t.Fatalf("mode=%q warm calls=%d ok=%d, want one warm call", b.Mode, calls, b.OK())
	}
	for _, n := range e.nodes {
		noPosts(t, "a node that cannot render krea2", n)
	}
	if len(b.Cluster) != 2 {
		t.Fatalf("cluster = %+v, want a row per roster node", b.Cluster)
	}
	for _, r := range b.Cluster {
		if r.State != "not-capable" || r.Why == "" {
			t.Errorf("row %+v, want not-capable with the reason", r)
		}
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "no node of the roster renders this recipe") || !strings.Contains(joined, "node-a") {
		t.Errorf("the batch must say why it stayed on this machine, naming the nodes: %q", lines)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("%d ticket(s) for a batch no node can take", n)
	}
	// The lane is held and nobody can relieve it: the batch KEEPS ITS PLACE for the lane (the queue's own ceiling), it does
	// not give up after the pipeline's 90 s window and end `queued` with exit 0 (the resend loop the queue exists to end).
	if gotWait != BatchHoldWindow() || gotWait < time.Hour {
		t.Errorf("the warm call was asked to wait %v, want the queue's ceiling %v", gotWait, BatchHoldWindow())
	}
	if !strings.Contains(strings.Join(lines, "\n"), "place in line") {
		t.Errorf("the line must say the batch holds its place: %q", lines)
	}
}

// ---- resume -------------------------------------------------------------------------------------------------------------------

// --resume skips a job only when the file at its out proves itself: it exists, is not empty, decodes whole as a PNG and has
// the size the job names. A zero-byte file (the ENOSPC night), a cut-short file, a file of another size, a file that is not
// a picture, a missing one: all rendered again. A job with no out cannot be proven and is never skipped. Without the flag
// nothing is skipped.
func TestBatchResumeSkipsOnlyExistingAndVerifying(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.png")
	writeBatchPNG(t, good, 64, 32)
	noSize := filepath.Join(dir, "nosize.png")
	writeBatchPNG(t, noSize, 17, 9)
	zero := filepath.Join(dir, "zero.png")
	if err := os.WriteFile(zero, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(dir, "wrong.png")
	writeBatchPNG(t, wrong, 10, 10)
	cut := filepath.Join(dir, "cut.png")
	full := batchPNG(t, 64, 32)
	if err := os.WriteFile(cut, full[:len(full)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.png")
	if err := os.WriteFile(garbage, []byte("IMG"), 0o644); err != nil {
		t.Fatal(err)
	}
	onlyWidth := filepath.Join(dir, "onlywidth.png")
	writeBatchPNG(t, onlyWidth, 64, 99)
	widthDiff := filepath.Join(dir, "widthdiff.png")
	writeBatchPNG(t, widthDiff, 10, 32)
	jobs := []BatchJob{
		{Prompt: "good", Out: good, Seed: 1, Width: 64, Height: 32},             // 0 skipped
		{Prompt: "nosize", Out: noSize, Seed: 2},                                // 1 skipped: the job names no size, any picture will do
		{Prompt: "zero", Out: zero, Seed: 3, Width: 64, Height: 32},             // 2 rendered
		{Prompt: "wrong", Out: wrong, Seed: 4, Width: 64, Height: 32},           // 3 rendered
		{Prompt: "cut", Out: cut, Seed: 5, Width: 64, Height: 32},               // 4 rendered
		{Prompt: "garbage", Out: garbage, Seed: 6, Width: 64, Height: 32},       // 5 rendered
		{Prompt: "missing", Out: filepath.Join(dir, "m.png"), Seed: 7},          // 6 rendered
		{Prompt: "onlywidth", Out: onlyWidth, Seed: 8, Width: 64, Height: 32},   // 7 rendered: 64x99 is not 64x32
		{Prompt: "no out", Seed: 9},                                             // 8 rendered, never skipped
		{Prompt: "onlywidth ok", Out: onlyWidth + ".b.png", Seed: 10, Width: 5}, // 9 skipped: 5x77 and the job names a width only
		{Prompt: "widthdiff", Out: widthDiff, Seed: 11, Width: 64, Height: 32},  // 10 rendered: 10x32 is not 64x32
	}
	writeBatchPNG(t, jobs[9].Out, 5, 77) // width matches, height not named: skipped
	jobs = append([]BatchJob(nil), jobs...)

	var renderedIdx []int
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for k, j := range js {
			out := j.Out
			if out == "" {
				out = filepath.Join(dir, "default.png")
			}
			writeBatchPNG(t, out, 64, 32)
			rows[k] = BatchRow{Out: out, Seed: j.Seed, OK: true, Ms: 5}
			renderedIdx = append(renderedIdx, int(j.Seed))
		}
		return rows, false, nil
	}

	b, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, jobs, warm, BatchOpts{Resume: true, Route: "local"})
	if err != nil {
		t.Fatal(err)
	}
	wantSkipped := map[int]bool{0: true, 1: true, 9: true}
	for i, r := range b.Rows {
		if r.I != i {
			t.Errorf("row %d carries i=%d: the rows must keep the job's own index after the skipped ones are taken out", i, r.I)
		}
		if wantSkipped[i] {
			if r.Verdict != VerdictSkipped || !r.OK || r.Ms != 0 || r.SHA256 == "" || r.Node != "" {
				t.Errorf("row %d = %+v, want skipped-existing with the hash of the file, no time, no node", i, r)
			}
			continue
		}
		if r.Verdict != VerdictRendered || !r.OK {
			t.Errorf("row %d = %+v, want rendered", i, r)
		}
	}
	if b.Skipped() != 3 || len(renderedIdx) != 8 {
		t.Errorf("skipped %d and rendered %d, want 3 and 8 (jobs %v were handed to the render)", b.Skipped(), len(renderedIdx), renderedIdx)
	}

	// Without the flag every job renders, even the ones that would have proven themselves.
	renderedIdx = nil
	for _, p := range []string{good, noSize} {
		writeBatchPNG(t, p, 64, 32)
	}
	b, err = RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, jobs, warm, BatchOpts{Route: "local"})
	if err != nil || b.Skipped() != 0 || len(renderedIdx) != len(jobs) {
		t.Errorf("without --resume: skipped %d, rendered %d of %d (err %v)", b.Skipped(), len(renderedIdx), len(jobs), err)
	}
}

// The A7 shape on fakes: a batch over the fleet, then the same batch with --resume skips the files it left, and after one is
// lost and one is cut short only those two are placed again.
func TestBatchResumeAfterTicketsPlacesOnlyWhatIsMissing(t *testing.T) {
	e := newQEnv(t, 2, false, sizedPNGNodes(t))
	dir := t.TempDir()
	jobs := []BatchJob{
		{Prompt: "one", Out: filepath.Join(dir, "1.png"), Seed: 1, Width: 64, Height: 32},
		{Prompt: "two", Out: filepath.Join(dir, "2.png"), Seed: 2, Width: 64, Height: 32},
		{Prompt: "three", Out: filepath.Join(dir, "3.png"), Seed: 3, Width: 64, Height: 32},
	}
	run := func(resume bool) *Batch {
		b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts(func(o *BatchOpts) { o.Resume = resume }))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	posts := func() int {
		n := 0
		for _, nd := range e.nodes {
			n += len(nd.posts())
		}
		return n
	}
	if b := run(false); b.OK() != 3 || posts() != 3 {
		t.Fatalf("first run: ok=%d posts=%d", b.OK(), posts())
	}
	b := run(true)
	if b.Skipped() != 3 || posts() != 3 || b.Mode != ModeNone {
		t.Fatalf("resume of a finished batch: skipped=%d posts=%d mode=%q, want all skipped and nothing sent", b.Skipped(), posts(), b.Mode)
	}
	if err := os.Remove(jobs[0].Out); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(jobs[2].Out)
	if err := os.WriteFile(jobs[2].Out, full[:len(full)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	b = run(true)
	if b.Skipped() != 1 || posts() != 5 || b.Rows[1].Verdict != VerdictSkipped || b.Rows[0].Verdict != VerdictRendered || b.Rows[2].Verdict != VerdictRendered {
		t.Fatalf("resume after losing one file and cutting another: skipped=%d posts=%d rows=%+v", b.Skipped(), posts(), b.Rows)
	}
}

// ---- how it ends --------------------------------------------------------------------------------------------------------------

// Exit 4 when any job failed (imagegen.BatchExitJobsFailed), 1 when the batch stopped or was interrupted, 0 otherwise, and
// the failed job's reason is in its row.
func TestBatchExit4OnAnyFailure(t *testing.T) {
	dir := t.TempDir()
	jobs := []BatchJob{
		{Prompt: "ok", Out: filepath.Join(dir, "ok.png"), Seed: 1},
		{Prompt: "fails", Out: filepath.Join(dir, "fails.png"), Seed: 2},
		{Prompt: "ok too", Out: filepath.Join(dir, "ok2.png"), Seed: 3},
	}
	warm := func(failIdx int) WarmFunc {
		return func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
			rows := make([]BatchRow, len(js))
			for i, j := range js {
				if i == failIdx {
					rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, Error: "comfy-render exited 1: ComfyUI exec error"}
					continue
				}
				writeBatchPNG(t, j.Out, 8, 8)
				rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
			}
			return rows, false, nil
		}
	}
	cfg := config.Config{StateDir: t.TempDir()}
	o := BatchOpts{Route: "local"}

	b, err := RunBatch(bctx(t), cfg, nil, jobs, warm(-1), o)
	if err != nil || b.ExitCode() != 0 {
		t.Fatalf("a clean batch: exit %d err %v", b.ExitCode(), err)
	}
	b, err = RunBatch(bctx(t), cfg, nil, jobs, warm(1), o)
	if err != nil || b.ExitCode() != imagegen.BatchExitJobsFailed || imagegen.BatchExitJobsFailed != 4 {
		t.Fatalf("a batch with a failed job: exit %d err %v, want 4", b.ExitCode(), err)
	}
	if r := b.Rows[1]; r.OK || r.Verdict != VerdictFailed || !strings.Contains(r.Error, "ComfyUI exec error") || r.SHA256 != "" {
		t.Errorf("the failed row = %+v, want failed with the render's own reason and no hash", r)
	}
	if b.Rows[0].Verdict != VerdictRendered || b.Rows[2].Verdict != VerdictRendered {
		t.Errorf("one failure must not take the other jobs with it: %+v", b.Rows)
	}

	// A warm call that stopped (the batch aborted) is exit 1, not 4: a wrapper tells "some failed" from "stopped".
	stop := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		return []BatchRow{{Out: js[0].Out, Seed: js[0].Seed, Error: "ENOSPC"}}, false, context.DeadlineExceeded
	}
	b, _ = RunBatch(bctx(t), cfg, nil, jobs, stop, o)
	if b.ExitCode() != 1 || b.Rows[1].Verdict != VerdictFailed {
		t.Errorf("a stopped batch: exit %d rows %+v, want 1 with the jobs it never reached failed", b.ExitCode(), b.Rows)
	}

	// A busy lane that kept the whole batch waiting is a defer: nothing ran, the jobs are queued, exit 0 as it always was.
	busy := func(context.Context, []BatchJob, time.Duration) ([]BatchRow, bool, error) {
		return nil, true, context.Canceled
	}
	b, _ = RunBatch(bctx(t), cfg, nil, jobs, busy, o)
	if b.ExitCode() != 0 || !b.Deferred || b.Queued() != 3 {
		t.Errorf("a deferred batch: exit %d deferred=%v queued=%d, want exit 0 with three queued jobs", b.ExitCode(), b.Deferred, b.Queued())
	}

	// ... but a job that failed BEFORE any lane was asked (its folder cannot be made) is a failure the defer must not hide: a
	// wrapper reading exit 0 would take a batch with a failed row for a batch that merely waits.
	afile := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(afile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mixed := []BatchJob{
		{Prompt: "unplaceable", Out: filepath.Join(afile, "sub", "no.png"), Seed: 7},
		{Prompt: "fine", Out: filepath.Join(dir, "fine.png"), Seed: 8},
	}
	b, _ = RunBatch(bctx(t), cfg, nil, mixed, busy, o)
	if b.ExitCode() != imagegen.BatchExitJobsFailed || !b.Deferred || b.Failed() != 1 || b.Queued() != 1 {
		t.Errorf("a deferred batch with a failed row: exit %d deferred=%v failed=%d queued=%d, want exit 4", b.ExitCode(), b.Deferred, b.Failed(), b.Queued())
	}
}

// The same on the ticket path: a node that fails one job fails that row only, and the exit says so.
func TestBatchTicketJobThatFailsOnItsNodeIsOneFailedRow(t *testing.T) {
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.deferAs, o.deferClass = "ComfyUI exec error", "other" })
	jobs := manyJobs(t, 2)
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.ExitCode() != imagegen.BatchExitJobsFailed || b.Failed() != 2 {
		t.Fatalf("exit %d failed %d rows %+v", b.ExitCode(), b.Failed(), b.Rows)
	}
	for _, r := range b.Rows {
		if r.OK || r.Verdict != VerdictFailed || !strings.Contains(r.Error, "ComfyUI exec error") {
			t.Errorf("row %+v, want failed with the node's reason", r)
		}
	}
}

// An interrupt ends the batch: the jobs that had not ended are queued (a ticket that sent nothing is canceled, a job on a
// node cannot be recalled), none is sent to a node, and the exit is 1.
func TestBatchInterruptLeavesQueuedRowsAndExit1(t *testing.T) {
	e := newQEnv(t, 2, true) // every node busy: nothing can start
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan *Batch, 1)
	go func() {
		b, _ := RunBatch(ctx, e.cfg, e.lr, manyJobs(t, 5), neverWarm(t), batchOpts(func(o *BatchOpts) { o.Jobs = 2 }))
		done <- b
	}()
	waitFor(t, 15*time.Second, "two tickets", func() bool { return len(ticketFiles(t, e.cfg)) == 2 })
	cancel()
	var b *Batch
	select {
	case b = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the batch did not end after the interrupt")
	}
	if b.Queued() != 5 || !b.Interrupted || b.ExitCode() != 1 {
		t.Fatalf("queued=%d interrupted=%v exit=%d rows=%+v, want all five queued and exit 1", b.Queued(), b.Interrupted, b.ExitCode(), b.Rows)
	}
	for _, n := range e.nodes {
		noPosts(t, "a busy node", n)
	}
	waitFor(t, 10*time.Second, "the canceled tickets to be gone", func() bool { return len(ticketFiles(t, e.cfg)) == 0 })
}

// ---- refusals and single-job failures --------------------------------------------------------------------------------------

func TestBatchRefusalsBeforeAnythingRuns(t *testing.T) {
	e := newQEnv(t, 1, false)
	dir := t.TempDir()
	ok := BatchJob{Prompt: "a door", Out: filepath.Join(dir, "a.png"), Seed: 1}
	for name, tc := range map[string]struct {
		jobs []BatchJob
		opts BatchOpts
		want string
	}{
		"empty":              {nil, batchOpts(), "empty batch"},
		"an empty prompt":    {[]BatchJob{ok, {Prompt: "  ", Out: filepath.Join(dir, "b.png")}}, batchOpts(), "job 1: empty prompt"},
		"two different pics": {[]BatchJob{ok, {Prompt: "another", Out: ok.Out, Seed: 2}}, batchOpts(), "jobs 0 and 1 are different pictures with the same out"},
		"two seedless jobs":  {[]BatchJob{{Prompt: "a door", Out: ok.Out}, {Prompt: "a door", Out: ok.Out}}, batchOpts(), "jobs 0 and 1 are different pictures with the same out"},
		"an unknown family":  {[]BatchJob{ok}, BatchOpts{Family: "nope"}, `unknown image family "nope"`},
		"an unknown route":   {[]BatchJob{ok}, batchOpts(func(o *BatchOpts) { o.Route = "everywhere" }), "unrecognized route"},
		"a negative --jobs":  {[]BatchJob{ok}, batchOpts(func(o *BatchOpts) { o.Jobs = -1 }), "--jobs -1"},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := RunBatch(bctx(t), e.cfg, e.lr, tc.jobs, neverWarm(t), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) || b != nil {
				t.Fatalf("want a refusal containing %q, got b=%v err=%v", tc.want, b, err)
			}
		})
	}
	untouched(t, "the node", e.nodes[0])

	// The same picture twice is not a clash.
	e.lr.set(core.LaneVerdict{Free: true})
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	if _, err := RunBatch(bctx(t), e.cfg, e.lr, []BatchJob{ok, ok}, warm, batchOpts()); err != nil {
		t.Errorf("two identical jobs write one file twice, which is not a clash: %v", err)
	}
}

// A job whose out cannot be made fails alone, before any lane is asked for it: it costs nothing and spoils nothing.
func TestBatchAnOutThatCannotBeMadeFailsOnlyThatJob(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := []BatchJob{
		{Prompt: "fine", Out: filepath.Join(dir, "a.png"), Seed: 1},
		{Prompt: "unplaceable", Out: filepath.Join(blocker, "b.png"), Seed: 2},
		{Prompt: "fine too", Out: filepath.Join(dir, "c.png"), Seed: 3},
	}
	var handed []string
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			handed = append(handed, j.Prompt)
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	b, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, jobs, warm, BatchOpts{Route: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(handed, ",") != "fine,fine too" {
		t.Errorf("the render was handed %v, want only the jobs that can be placed", handed)
	}
	if r := b.Rows[1]; r.Verdict != VerdictFailed || !strings.Contains(r.Error, "cannot make the folder for out") || r.I != 1 {
		t.Errorf("row 1 = %+v, want failed naming the folder", r)
	}
	if b.ExitCode() != imagegen.BatchExitJobsFailed || b.Rows[0].Verdict != VerdictRendered || b.Rows[2].Verdict != VerdictRendered {
		t.Errorf("exit %d rows %+v, want exit 4 with the other two rendered", b.ExitCode(), b.Rows)
	}
}

// The time in a row is the lane's own when it reports one (a render's wall time), and the call's wall time when it does not.
func TestBatchMsIsTheLanesOwnWhenItReportsOne(t *testing.T) {
	e := newQEnv(t, 1, true)
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	batchCall = func(_ context.Context, _ config.Config, _ Runner, req core.Request, _ string, _ []string, _ Wait) core.Result {
		out, _ := req.Params["out"].(string)
		writeBatchPNG(t, out, 8, 8)
		data, _ := json.Marshal(map[string]any{"image_path": out})
		res := core.Result{OK: true, Data: data}
		if req.Input == "reports" {
			res.Meta.LatencyMs = 4321
		} else {
			time.Sleep(30 * time.Millisecond)
		}
		return res
	}
	dir := t.TempDir()
	jobs := []BatchJob{{Prompt: "reports", Out: filepath.Join(dir, "a.png"), Seed: 1}, {Prompt: "silent", Out: filepath.Join(dir, "b.png"), Seed: 2}}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.Rows[0].Ms != 4321 {
		t.Errorf("ms = %d, want the lane's own 4321", b.Rows[0].Ms)
	}
	if b.Rows[1].Ms < 30 || b.Rows[1].Ms > 20000 {
		t.Errorf("ms = %d, want the call's wall time (at least the 30 ms it took) when the lane reports none", b.Rows[1].Ms)
	}
}

// Outs that differ only in case are one file on Windows: two different pictures for them are a clash there.
func TestBatchOutsThatDifferOnlyInCaseClashOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("file names are case-sensitive here")
	}
	dir := t.TempDir()
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	_, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, []BatchJob{
		{Prompt: "a", Out: filepath.Join(dir, "Pic.png")}, {Prompt: "b", Out: filepath.Join(dir, "pic.PNG")},
	}, warm, BatchOpts{Route: "local"})
	if err == nil || !strings.Contains(err.Error(), "same out") {
		t.Fatalf("want a clash, got %v", err)
	}
}

// A lane that said it rendered what is not there, or nothing, or something nobody could check, did not render it.
func TestBatchAJobIsRenderedOnlyWhenItsFileIsThere(t *testing.T) {
	e := newQEnv(t, 1, true)
	dir := t.TempDir()
	jobs := []BatchJob{
		{Prompt: "missing", Out: filepath.Join(dir, "missing.png"), Seed: 1},
		{Prompt: "empty", Out: filepath.Join(dir, "empty.png"), Seed: 2},
		{Prompt: "unverified", Out: filepath.Join(dir, "unverified.png"), Seed: 3},
		{Prompt: "fine", Out: filepath.Join(dir, "fine.png"), Seed: 4},
	}
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	batchCall = func(_ context.Context, _ config.Config, _ Runner, req core.Request, _ string, _ []string, _ Wait) core.Result {
		out, _ := req.Params["out"].(string)
		extra := map[string]any{"image_path": out, "node": "node-a"}
		switch req.Input {
		case "empty":
			_ = os.WriteFile(out, nil, 0o644)
		case "unverified":
			writeBatchPNG(t, out, 8, 8)
			extra["unverified"] = true
		case "fine":
			writeBatchPNG(t, out, 8, 8)
		}
		data, _ := json.Marshal(extra)
		return core.Result{OK: true, Data: data}
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"cannot be read", "is empty", "published no sha256", ""} {
		r := b.Rows[i]
		if want == "" {
			if r.Verdict != VerdictRendered || r.SHA256 == "" {
				t.Errorf("row %d = %+v, want rendered", i, r)
			}
			continue
		}
		if r.Verdict != VerdictFailed || r.OK || !strings.Contains(r.Error, want) || r.SHA256 != "" {
			t.Errorf("row %d = %+v, want failed (%q), no hash", i, r, want)
		}
	}
	// A failed job leaves no file of its own at its path: an empty file is removed, and a file the batch rejected must not be promoted by
	// the next --resume. The picture the node sent is set aside beside it, not destroyed, and the row says where.
	if _, err := os.Stat(jobs[1].Out); err == nil {
		t.Errorf("the empty file the lane left is still at %s", jobs[1].Out)
	}
	unverified := jobs[2].Out
	if _, err := os.Stat(unverified); err == nil {
		t.Errorf("the rejected file is still at %s", unverified)
	}
	if !strings.Contains(b.Rows[2].Error, unverified+".unverified") {
		t.Errorf("the row must name where the file was set aside: %q", b.Rows[2].Error)
	}
	if !pngVerifies(unverified+".unverified", 8, 8) {
		t.Errorf("the picture the node sent was not kept beside the out")
	}
	b2, err := RunBatch(bctx(t), e.cfg, e.lr, jobs[2:3], neverWarm(t), batchOpts(func(o *BatchOpts) { o.Resume = true }))
	if err != nil {
		t.Fatal(err)
	}
	if b2.Skipped() != 0 || b2.Rows[0].Verdict != VerdictFailed || b2.ExitCode() != imagegen.BatchExitJobsFailed {
		t.Errorf("--resume over a rejected file: row %+v exit %d, want it rendered again (and rejected again), never skipped-existing", b2.Rows[0], b2.ExitCode())
	}
}

// A job whose single render comes back "a card is held" (another job of the batch took the lane a moment after it read
// free) is sent again, a bounded number of times, and then becomes a ticket; any other failure is final at once. A card that
// stays held is a place not had, so the job ends `queued` (never `failed`: a busy card is a place in line), and the queue's
// own answers (its ceiling, a ticket it could not adopt) are not a lost race and are not sent again.
func TestBatchRetriesALostLaneRaceAndOnlyThat(t *testing.T) {
	e := newQEnv(t, 1, true)
	var calls atomic.Int32
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	mk := func(class string, okAfter int32) {
		calls.Store(0)
		batchCall = func(_ context.Context, _ config.Config, _ Runner, req core.Request, _ string, _ []string, _ Wait) core.Result {
			n := calls.Add(1)
			out, _ := req.Params["out"].(string)
			if n > okAfter {
				writeBatchPNG(t, out, 8, 8)
				data, _ := json.Marshal(map[string]any{"image_path": out})
				return core.Result{OK: true, Data: data}
			}
			res := core.Deferf("gpu busy: card c2 is held", "", core.Meta{ErrClass: class})
			res.DeferClass = core.DeferClassCapacity
			return res
		}
	}
	one := func() *Batch {
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mk(core.ErrClassGPUBusy, 1)
	if b := one(); b.OK() != 1 || calls.Load() != 2 {
		t.Errorf("one lost race: ok=%d calls=%d, want the second call to render it", b.OK(), calls.Load())
	}
	mk(core.ErrClassGPUBusy, 99)
	if b := one(); b.Failed() != 0 || b.Queued() != 1 || calls.Load() != 1+batchBusyRetries {
		t.Errorf("a lane that stays held: failed=%d queued=%d calls=%d, want %d calls and then a queued row (a place not had is never a failed row)", b.Failed(), b.Queued(), calls.Load(), 1+batchBusyRetries)
	} else if r := b.Rows[0]; !strings.Contains(r.Error, "gpu busy: card c2 is held") || !strings.Contains(r.Error, "--resume") || b.Ceiling {
		t.Errorf("the queued row = %+v ceiling=%v, want the lane's own reason and the way to render the job, and no ceiling (the queue was not waited for)", r, b.Ceiling)
	}
	mk("oom", 99)
	if b := one(); b.Failed() != 1 || calls.Load() != 1 {
		t.Errorf("another failure: failed=%d calls=%d, want no retry", b.Failed(), calls.Load())
	}

	// The queue's own answers say a ticket ended without a place (its ceiling, a ticket it could not adopt). They carry the
	// held class like a lost race does, but nothing was lost a moment after it read free: sending the request again would
	// start another ticket and another wait. One call, a queued row with the queue's reason, and not the race's words.
	for name, tc := range map[string]struct {
		class  string
		reason string
	}{
		"the ceiling":        {core.DeferClassBudget, "ticket mq-abc123 waited 8h0m0s without a lane anywhere and ended; send the request again"},
		"a ticket not taken": {core.DeferClassInfrastructure, "ticket mq-abc123 could not be adopted: its owner ended and the job could not be taken over; send the request again"},
	} {
		calls.Store(0)
		batchCall = func(context.Context, config.Config, Runner, core.Request, string, []string, Wait) core.Result {
			calls.Add(1)
			res := core.Deferf(tc.reason, "", core.Meta{ErrClass: core.ErrClassGPUQueued})
			res.DeferClass = tc.class
			return res
		}
		var told []string
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts(func(o *BatchOpts) { o.Notify = func(l string) { told = append(told, l) } }))
		if err != nil {
			t.Fatal(err)
		}
		r := b.Rows[0]
		if b.Ceiling != (tc.class == core.DeferClassBudget) {
			t.Errorf("%s: ceiling = %v, want it set for the queue's ceiling and for nothing else", name, b.Ceiling)
		}
		if calls.Load() != 1 || b.Failed() != 0 || b.Queued() != 1 || !strings.Contains(r.Error, tc.reason) || strings.Contains(r.Error, "lane was taken") || strings.Contains(strings.Join(told, "\n"), "lane was taken") {
			t.Errorf("%s: calls=%d failed=%d queued=%d row=%+v lines=%q, want one call and a queued row with the queue's own reason", name, calls.Load(), b.Failed(), b.Queued(), r, told)
		}
	}
}

// What a human at the terminal is told is prefixed with the job it is about.
func TestBatchLinesNameTheirJob(t *testing.T) {
	e := newQEnv(t, 1, false, sizedPNGNodes(t))
	var mu sync.Mutex
	var lines []string
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 2), neverWarm(t), batchOpts(func(o *BatchOpts) {
		o.Notify = func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	}))
	if err != nil || b.OK() != 2 {
		t.Fatalf("err=%v ok=%d", err, b.OK())
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "tickets") {
		t.Fatalf("the batch must say where it runs first: %q", lines)
	}
	seenJob := false
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "job 0: ") && !strings.HasPrefix(l, "job 1: ") {
			t.Errorf("a ticket line without its job: %q", l)
		}
		seenJob = true
	}
	if !seenJob {
		t.Errorf("no ticket line was told: %q", lines)
	}
}

// A hand-forced remote batch on a machine with no fleet is refused up front, once, not as one failed row per job.
func TestBatchRouteRemoteWithoutAFleetIsRefusedUpFront(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	_, err := RunBatch(bctx(t), cfg, nil, manyJobs(t, 3), nil, BatchOpts{Route: "remote"})
	if err == nil || !strings.Contains(err.Error(), "delegate_remotes") {
		t.Fatalf("want the missing fleet named, got %v", err)
	}
}

// ---- the results file, on its own -----------------------------------------------------------------------------------------

// A row is on disk the moment it is appended (a batch that dies leaves the jobs that finished), and Close leaves the file
// whole and in job order whatever order the jobs ended in.
func TestResultsFileKeepsRowsAsJobsEndAndEndsInJobOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "r.jsonl")
	rf, err := OpenResults(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := []BatchRow{
		{I: 0, Out: "a", Seed: 1, OK: true, Verdict: VerdictRendered},
		{I: 1, Out: "b", Seed: 2, OK: true, Verdict: VerdictSkipped},
		{I: 2, Out: "c", Seed: 3, Verdict: VerdictFailed, Error: "boom"},
	}
	rf.Append(rows[2]) // the jobs end in the order 2, 0, 1
	rf.Append(rows[0])
	rf.Append(rows[1])
	// The rows are written beside the target, so a record an earlier run left at the path is whole until this run ends.
	raw, _ := os.ReadFile(path + ".partial")
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 3 || !strings.Contains(lines[0], `"i":2`) {
		t.Fatalf("before Close the partial file holds the rows as they ended, got %q", raw)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the target exists before the batch ended")
	}
	if err := rf.Close(rows); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("after Close: %q", raw)
	}
	for i, l := range lines {
		var r BatchRow
		if err := json.Unmarshal([]byte(l), &r); err != nil || r.I != i {
			t.Errorf("line %d = %q (%v), want the row of job %d", i, l, err, i)
		}
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf("the temp file of the rewrite was left behind")
	}
	if _, err := os.Stat(path + ".partial"); err == nil {
		t.Errorf("the partial file was left behind after the batch ended")
	}
	// A path that cannot be written stops the command before it starts; a refused batch leaves nothing.
	if _, err := OpenResults(filepath.Join(path, "x.jsonl")); err == nil {
		t.Errorf("a results path under a file must be refused")
	}
	d := filepath.Join(t.TempDir(), "d.jsonl")
	rf2, err := OpenResults(d)
	if err != nil {
		t.Fatal(err)
	}
	rf2.Discard()
	if _, err := os.Stat(d); err == nil {
		t.Errorf("a discarded results file must be gone")
	}
	if _, err := os.Stat(d + ".partial"); err == nil {
		t.Errorf("a discarded results file must leave no partial behind")
	}
}

// A batch the command refuses, or an earlier run's record when a later one is still going, must not cost the earlier run's
// results: the node, the milliseconds and the hash of every picture are only there.
func TestResultsFileNeverCostsAnEarlierRecordUntilTheBatchEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	const earlier = "{\"i\":0,\"out\":\"a\",\"node\":\"node-a\",\"sha256\":\"abc\",\"verdict\":\"rendered\"}\n"
	if err := os.WriteFile(path, []byte(earlier), 0o644); err != nil {
		t.Fatal(err)
	}
	same := func(when string) {
		t.Helper()
		if raw, err := os.ReadFile(path); err != nil || string(raw) != earlier {
			t.Fatalf("%s: the earlier record is %q (%v), want it untouched", when, raw, err)
		}
	}
	// refused before it began
	rf, err := OpenResults(path)
	if err != nil {
		t.Fatal(err)
	}
	same("after the file was opened")
	rf.Discard()
	same("after a refused batch discarded its file")
	// a batch that is still running, then one that ends
	rf, err = OpenResults(path)
	if err != nil {
		t.Fatal(err)
	}
	rf.Append(BatchRow{I: 0, Out: "a", Verdict: VerdictRendered, OK: true})
	same("while the new batch runs")
	if err := rf.Close([]BatchRow{{I: 0, Out: "a", Verdict: VerdictRendered, OK: true}, {I: 1, Out: "b", Verdict: VerdictFailed}}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Count(string(raw), "\n") != 2 || strings.Contains(string(raw), "node-a") {
		t.Errorf("after the batch ended the file must hold this run's rows, got %q", raw)
	}
}

// ---- the cases that keep a batch on this machine -------------------------------------------------------------------------

// A process that runs under a lease it inherited (`gpu reserve -- local-offload ...`) has its card chosen for it: the batch
// renders on that lease, warm, whatever the lane and the fleet look like. A machine with no queue, whose lease refuses the state
// root (a cloud-synced folder), has nowhere to keep a ticket and does the same. A config that sets no state_dir is NOT such a
// machine: the queue lives under the state root the lease resolves (F65), so that batch is tickets.
func TestBatchStaysWarmUnderAnInheritedLeaseOrWithoutAQueue(t *testing.T) {
	warm := func(t *testing.T, calls *int) WarmFunc {
		return func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
			*calls++
			rows := make([]BatchRow, len(js))
			for i, j := range js {
				writeBatchPNG(t, j.Out, 8, 8)
				rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
			}
			return rows, false, nil
		}
	}
	t.Run("an inherited lease", func(t *testing.T) {
		e := newQEnv(t, 1, false) // the lane is held and a node is idle: tickets, but for the lease
		t.Setenv("GPU_LEASE_DIR", t.TempDir())
		var calls int
		b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 2), warm(t, &calls), batchOpts())
		if err != nil || b.Mode != ModeWarm || calls != 1 {
			t.Fatalf("mode=%q calls=%d err=%v", b.Mode, calls, err)
		}
		untouched(t, "the idle node", e.nodes[0])
	})
	t.Run("a state root the lease refuses (no queue)", func(t *testing.T) {
		e := newQEnv(t, 1, false)
		cfg := e.cfg
		cfg.StateDir = filepath.Join(t.TempDir(), "Google Drive", "state") // refused by the lease: no queue, nowhere to keep a ticket
		if _, ok := storeFor(cfg); ok {
			t.Fatal("control: a cloud-synced root keeps no queue")
		}
		var calls int
		b, err := RunBatch(bctx(t), cfg, e.lr, manyJobs(t, 2), warm(t, &calls), batchOpts())
		if err != nil || b.Mode != ModeWarm || calls != 1 {
			t.Fatalf("mode=%q calls=%d err=%v", b.Mode, calls, err)
		}
		untouched(t, "the idle node", e.nodes[0])
	})
	t.Run("no state_dir at all has a queue, so the batch is tickets (F65)", func(t *testing.T) {
		e := newQEnv(t, 1, false, sizedPNGNodes(t))
		cfg := e.cfg
		cfg.StateDir = "" // the operator's real config sets none
		defaultRoot(t)
		if _, ok := storeFor(cfg); !ok {
			t.Fatal("control: a config with no state_dir has the default state root's queue")
		}
		var calls int
		b, err := RunBatch(bctx(t), cfg, e.lr, manyJobs(t, 2), warm(t, &calls), batchOpts())
		if err != nil || b.Mode != ModeTickets || calls != 0 {
			t.Fatalf("mode=%q calls=%d err=%v", b.Mode, calls, err)
		}
	})
}

// ---- where a ticket's picture comes from -------------------------------------------------------------------------------------

// A ticket that starts on THIS machine's lane (it freed first) is attributed to this machine, not to a node.
type localRender struct {
	*keeperRunner
}

func (l localRender) Run(_ context.Context, req core.Request) core.Result {
	l.laneRunner.mu.Lock()
	l.laneRunner.runs++
	l.laneRunner.ran = append(l.laneRunner.ran, req)
	l.laneRunner.mu.Unlock()
	out, _ := req.Params["out"].(string)
	body, err := encodePNG(8, 8)
	if err == nil {
		err = os.WriteFile(out, body, 0o644)
	}
	if err != nil {
		return core.Deferf(err.Error(), "", core.Meta{})
	}
	data, _ := json.Marshal(map[string]any{"image_path": out})
	return core.Result{OK: true, Data: data}
}

func TestBatchTicketThatRunsHereIsAttributedToThisMachine(t *testing.T) {
	e := newQEnv(t, 1, true) // the node is busy: the lane that frees first is this machine's
	lr := localRender{e.lr}
	jobs := manyJobs(t, 1)
	done := make(chan *Batch, 1)
	ctx := bctx(t)
	go func() {
		b, _ := RunBatch(ctx, e.cfg, lr, jobs, neverWarm(t), batchOpts())
		done <- b
	}()
	waitFor(t, 10*time.Second, "the ticket", func() bool { return len(ticketFiles(t, e.cfg)) == 1 })
	e.lr.set(core.LaneVerdict{Free: true})
	select {
	case b := <-done:
		r := b.Rows[0]
		if r.Verdict != VerdictRendered || r.Node != "delegator-host" || r.SHA256 == "" {
			t.Fatalf("row = %+v, want rendered on this machine's own id", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the ticket never ran on the lane that freed")
	}
}

// A job that names no out takes the name the lane gives it, and the row says where it landed.
func TestBatchAJobWithNoOutTakesTheNameTheLaneGivesIt(t *testing.T) {
	e := newQEnv(t, 1, false, sizedPNGNodes(t))
	b, err := RunBatch(bctx(t), e.cfg, e.lr, []BatchJob{{Prompt: "no out given", Seed: 77, Width: 64, Height: 32}}, neverWarm(t), batchOpts())
	if err != nil {
		t.Fatal(err)
	}
	r := b.Rows[0]
	if r.Verdict != VerdictRendered || r.Out == "" || filepath.Dir(r.Out) != filepath.Clean(e.cfg.MediaDir) {
		t.Fatalf("row = %+v, want the picture in the media dir %s", r, e.cfg.MediaDir)
	}
	if body, err := os.ReadFile(r.Out); err != nil || r.SHA256 != sha(body) {
		t.Errorf("the row's out holds %v (%v) but the row says %s", len(body), err, r.SHA256)
	}
}

// ---- the edges of the decision ------------------------------------------------------------------------------------------------

// A lane the pipeline could not read is not a free lane (the card table is slowest to read exactly when a heavy render
// holds the card): with a node that renders the recipe, the batch is tickets, not a warm session on a lane that may be held.
func TestBatchLaneThatCannotBeReadIsNotAFreeLane(t *testing.T) {
	e := newQEnv(t, 1, false, sizedPNGNodes(t))
	e.lr.set(core.LaneVerdict{Free: true, Unjudged: true, Why: "the card table did not read in time"})
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts())
	if err != nil || b.Mode != ModeTickets || b.OK() != 1 {
		t.Fatalf("mode=%q ok=%d err=%v, want one ticket placed on the node", b.Mode, b.OK(), err)
	}
}

// A --remote that is not one of delegate_remotes cannot be used to reach past the operator's list: the batch is what it
// would be without a fleet, and says why. A machine with no fleet says nothing.
func TestBatchABadRemoteKeepsTheBatchOnThisMachine(t *testing.T) {
	e := newQEnv(t, 1, false)
	var lines []string
	var calls int
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		calls++
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), warm, batchOpts(func(o *BatchOpts) {
		o.Remotes = []string{"http://127.0.0.1:1"}
		o.Notify = func(l string) { lines = append(lines, l) }
	}))
	if err != nil || b.Mode != ModeWarm || calls != 1 {
		t.Fatalf("mode=%q calls=%d err=%v", b.Mode, calls, err)
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "not in delegate_remotes") {
		t.Errorf("the batch must say why the fleet was not used: %q", lines)
	}
	untouched(t, "the roster node", e.nodes[0])

	noFleet := e.cfg
	noFleet.DelegateRemotes = nil
	lines = nil
	if _, err := RunBatch(bctx(t), noFleet, e.lr, manyJobs(t, 1), warm, batchOpts(func(o *BatchOpts) { o.Notify = func(l string) { lines = append(lines, l) } })); err != nil || len(lines) != 0 {
		t.Errorf("a machine with no fleet runs its batch as it always did, without a word: %q (%v)", lines, err)
	}
}

// --jobs bounds tickets, and a batch that renders on one warm session has none: the caller is told it did not apply.
func TestBatchJobsFlagOnAWarmBatchIsSaidNotApplied(t *testing.T) {
	e := newQEnv(t, 1, false)
	e.lr.set(core.LaneVerdict{Free: true})
	var lines []string
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		rows := make([]BatchRow, len(js))
		for i, j := range js {
			writeBatchPNG(t, j.Out, 8, 8)
			rows[i] = BatchRow{Out: j.Out, Seed: j.Seed, OK: true}
		}
		return rows, false, nil
	}
	_, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 2), warm, batchOpts(func(o *BatchOpts) {
		o.Jobs = 3
		o.Notify = func(l string) { lines = append(lines, l) }
	}))
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "--jobs 3 applies to the ticket path only") {
		t.Errorf("err=%v lines=%q", err, lines)
	}
}

// A job reaches its node as the caller wrote it: a seed it did not give is minted once and is the one in the row, steps
// go exact, refine=true is the machine's own setting (nothing is sent) and only an explicit false is sent.
func TestBatchMintsASeedAndCarriesStepsAndOnlyAnExplicitRefineFalse(t *testing.T) {
	e := newQEnv(t, 1, false, sizedPNGNodes(t))
	dir := t.TempDir()
	on := true
	jobs := []BatchJob{{Prompt: "a door", Out: filepath.Join(dir, "a.png"), Steps: 20, Refine: &on}}
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts())
	if err != nil || b.OK() != 1 {
		t.Fatalf("err=%v rows=%+v", err, b.Rows)
	}
	_, payload := posted(t, e.nodes[0])
	if seed, _ := payload["seed"].(float64); seed <= 0 || int(seed) != b.Rows[0].Seed {
		t.Errorf("payload seed %v, row seed %d: a seed the job did not give is minted once and is the row's", payload["seed"], b.Rows[0].Seed)
	}
	if payload["steps"] != float64(20) {
		t.Errorf("steps = %v, want 20 exactly", payload["steps"])
	}
	if _, has := payload["refine"]; has {
		t.Errorf("refine=true must send nothing (the node's own setting), got %v", payload["refine"])
	}
}

// A warm session that said a job rendered when the file is not there did not render it.
func TestBatchWarmJobThatReportedOKWithoutAFileIsFailed(t *testing.T) {
	dir := t.TempDir()
	jobs := []BatchJob{{Prompt: "a", Out: filepath.Join(dir, "a.png"), Seed: 1}, {Prompt: "b", Out: filepath.Join(dir, "b.png"), Seed: 2}}
	warm := func(_ context.Context, js []BatchJob, _ time.Duration) ([]BatchRow, bool, error) {
		writeBatchPNG(t, js[0].Out, 8, 8) // the second is reported rendered and never written
		return []BatchRow{{Out: js[0].Out, Seed: 1, OK: true}, {Out: js[1].Out, Seed: 2, OK: true}}, false, nil
	}
	b, err := RunBatch(bctx(t), config.Config{StateDir: t.TempDir()}, nil, jobs, warm, BatchOpts{Route: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Rows[0].Verdict != VerdictRendered || b.Rows[1].Verdict != VerdictFailed || !strings.Contains(b.Rows[1].Error, "cannot be read") || b.ExitCode() != imagegen.BatchExitJobsFailed {
		t.Errorf("rows = %+v exit %d", b.Rows, b.ExitCode())
	}
}
