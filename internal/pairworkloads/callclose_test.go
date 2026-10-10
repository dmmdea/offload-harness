package pairworkloads

// How a long call's PAIR card closes when its door answers (2026-10-09).
//
// A session's runner opened a fresh stdio door per attempt, called offload_generate_image, got a clean
// "gpu queued" deferral, and closed stdin and killed the door right after the reply. The door's card
// was still open in the register (its terminal frame was on a background goroutine), so the orphan
// sweep closed it "Failed: harness process exited before the job finished": two red cards for two
// calls that had answered. These tests pin the three things that went wrong: the close is on the wire
// before the call returns, a call held back by another job's hold on the card is not a failure, and
// the sweep has nothing to find afterwards.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// heldReason is the reason a real queued answer carries (pipeline.errGPUQueued.Error).
const heldReason = "gpu queued: card(s) 0000 held by media (epoch 7); your place in line is #1 (token tk-9f2c), at most 90s until the lease(s) in the way end; call again with waiter_token=tk-9f2c to keep it"

func heldResult() core.Result {
	res := core.Deferf(heldReason, "", core.Meta{ErrClass: core.ErrClassGPUQueued, Model: "comfyui:x"})
	res.DeferClass = core.DeferClassCapacity
	return res
}

// closeRig is a PAIR ingress that is slow (a PAIR on a loaded box answers late, never early) beside a
// register, so a close that is left to a background goroutine cannot have landed when the call returns.
type closeRig struct {
	*orphanRig
	delay time.Duration
}

func newCloseRig(t *testing.T, delay time.Duration) *closeRig {
	t.Helper()
	r := &orphanRig{pair: &capture{}, appDir: writePairAppDir(t), dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(delay)
		r.pair.handler(w, req)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return &closeRig{orphanRig: r, delay: delay}
}

// ledgerFor opens a ledger the emitter observes, as the CLI and the MCP server do.
func ledgerFor(t *testing.T, e *Emitter) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e.AttachLedger(l)
	return l
}

// closedCard is what is on the wire for one card and in the register, read at the moment a call's door
// would answer: nothing here may wait for a background goroutine (no e.Wait).
type closedCard struct {
	open   map[string]any // the queued frame
	closed map[string]any // the terminal frame
	method string         // its lifecycle method
}

func (r *closeRig) readCard(t *testing.T, id string) closedCard {
	t.Helper()
	var c closedCard
	for i := 0; i < r.pair.count(); i++ {
		info := r.pair.info(i)
		if info["id"] != id {
			t.Fatalf("a second card appeared: %v", info)
		}
		switch r.pair.method(i) {
		case "workload:submitted":
			c.open = info
		case "workload:completed", "workload:errored":
			if c.closed != nil {
				t.Fatalf("card %s closed twice", id)
			}
			c.closed, c.method = info, r.pair.method(i).(string)
		}
	}
	return c
}

// nothingLeftForTheSweep is the incident's acceptance: with the door answered (and its process about to die),
// the register is empty and a sweep of a dead process's cards closes nothing, so no card is ever
// closed "harness process exited before the job finished" over a call that answered.
func (r *closeRig) nothingLeftForTheSweep(t *testing.T) {
	t.Helper()
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("the register still holds %v when the door answers: the sweep would close it as an orphan", files)
	}
	sw := r.emitter(dead, func(int) (int64, bool) { return 0, false })
	if n := sw.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("the sweep found %d card(s) of a call that had answered", n)
	}
	for _, f := range r.failedFrames() {
		if f["error"] == OrphanError {
			t.Fatalf("a card was closed as an orphan: %v", f)
		}
	}
}

// closePaths runs one call to its end both ways a card closes: by the call's end alone (the lane wrote
// no row: a cache hit, or the ledger is held by another process), and by the call's own ledger row,
// whose frame the observer's goroutine posts (row builds the row the lane would have written once it
// knows the call id). started says the lane marked the card running first. check holds for both.
func closePaths(t *testing.T, row func(callID string) *ledger.Entry, res core.Result, started bool, check func(t *testing.T, r *closeRig, c closedCard)) {
	t.Helper()
	for _, withRow := range []bool{false, true} {
		name := "end alone"
		if withRow {
			name = "ledger row then end"
		}
		t.Run(name, func(t *testing.T) {
			r := newCloseRig(t, 120*time.Millisecond)
			e := r.emitter(nil, nil)
			var l *ledger.Ledger
			if withRow {
				l = ledgerFor(t, e)
			}
			id, working, end := e.Begin("generate_image", "offload_generate_image")
			e.Wait() // the queued frame landed while the call waited for its card
			if started {
				working()
				e.Wait()
			}
			if withRow {
				if err := l.Record(*row(id)); err != nil {
					t.Fatal(err)
				}
			}
			end(res)
			// THE DOOR ANSWERS HERE. Its client kills the process next; nothing below waits.
			check(t, r, r.readCard(t, id))
			r.nothingLeftForTheSweep(t)
		})
	}
}

func heldRow(callID string) *ledger.Entry {
	return &ledger.Entry{Task: "generate_image", ModelTier: "comfyui:x", Deferred: true, ErrClass: core.ErrClassGPUQueued,
		Reason: heldReason, LatencyMs: 61000, CallID: callID}
}

// A call held back by another job's hold on the card (the real queued answer) is closed quiet: PAIR's
// terminal state `completed`, never `workload:errored`, with no start and the reason in `error`.
func TestAHeldCallClosesQuietBeforeItsDoorAnswers(t *testing.T) {
	closePaths(t, heldRow, heldResult(), false, func(t *testing.T, r *closeRig, c closedCard) {
		if c.open == nil || c.closed == nil {
			t.Fatalf("the card must be opened and closed on the wire when the door answers: %d frame(s)", r.pair.count())
		}
		if c.method != "workload:completed" || c.closed["state"] != "completed" {
			t.Fatalf("a held call closed %s/%v: a place in line is not a failure", c.method, c.closed["state"])
		}
		if c.closed["startedAt"] != nil {
			t.Fatalf("a call that never held the card has no start: %v", c.closed["startedAt"])
		}
		if got, _ := c.closed["error"].(string); !strings.HasPrefix(got, "gpu queued: card(s) 0000 held by media") {
			t.Fatalf("the close must carry the deferral reason: %q", got)
		}
		if c.closed["engine"] != "comfyui" || c.closed["id"] != c.open["id"] || c.closed["createdAt"] != c.open["createdAt"] {
			t.Fatalf("the close must name the card the call opened: open %v closed %v", c.open, c.closed)
		}
		if c.closed["completedAt"] == nil {
			t.Fatalf("a closed card has a completion time: %v", c.closed)
		}
		if n := len(r.failedFrames()); n != 0 {
			t.Fatalf("%d workload:errored frame(s) for a call that only waited its turn", n)
		}
	})
}

// gpu_busy (the window ran out and no place was left) is the same doctrine as gpu_queued.
func TestABusyCardCallClosesQuietToo(t *testing.T) {
	res := core.Deferf("gpu busy: another generation job holds the card", "", core.Meta{ErrClass: core.ErrClassGPUBusy})
	closePaths(t, func(id string) *ledger.Entry {
		return &ledger.Entry{Task: "generate_image", Deferred: true, ErrClass: core.ErrClassGPUBusy, Reason: res.Reason, CallID: id}
	}, res, false, func(t *testing.T, r *closeRig, c closedCard) {
		if c.method != "workload:completed" || c.closed["startedAt"] != nil || c.closed["error"] != "gpu busy: another generation job holds the card" {
			t.Fatalf("a busy-card call: %s %v", c.method, c.closed)
		}
	})
}

func TestASuccessfulCallClosesCompletedWithItsStart(t *testing.T) {
	ok := core.Result{OK: true, Meta: core.Meta{Model: "comfyui:x"}}
	closePaths(t, func(id string) *ledger.Entry {
		return &ledger.Entry{Task: "generate_image", ModelTier: "comfyui:x", LatencyMs: 4000, CallID: id}
	}, ok, true, func(t *testing.T, r *closeRig, c closedCard) {
		if c.closed == nil || c.method != "workload:completed" || c.closed["error"] != nil || c.closed["startedAt"] == nil {
			t.Fatalf("a success closes completed, started, with no error: %s %v", c.method, c.closed)
		}
	})
}

// Anything that did not succeed and was not held back is a failure with the real error: a lane that
// ran and broke, a configuration fault (an unusable lease location is gpu_lease_unavailable, not a
// busy card), an unclassified deferral.
func TestAFailedCallClosesFailedWithTheError(t *testing.T) {
	for _, tc := range []struct{ name, class, reason string }{
		{"a render that broke", "timeout", "image generation failed: ComfyUI did not answer in 600s"},
		{"an unusable lease location", "gpu_lease_unavailable", "gpu lease unavailable: state dir is read-only"},
		{"an unclassified deferral", "", "no image-gen route configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := core.Deferf(tc.reason, "", core.Meta{ErrClass: tc.class})
			closePaths(t, func(id string) *ledger.Entry {
				return &ledger.Entry{Task: "generate_image", Deferred: true, ErrClass: tc.class, Reason: tc.reason, CallID: id}
			}, res, false, func(t *testing.T, r *closeRig, c closedCard) {
				if c.method != "workload:errored" || c.closed["state"] != "failed" || c.closed["error"] != tc.reason {
					t.Fatalf("want a failed close carrying %q, got %s %v", tc.reason, c.method, c.closed)
				}
			})
		})
	}
}

// A held class says the lane never got the card. A lane that DID hold it (the card ran) and then
// deferred is a failure whatever the class says.
func TestAHeldClassOnACardThatRanStillFails(t *testing.T) {
	closePaths(t, heldRow, heldResult(), true, func(t *testing.T, r *closeRig, c closedCard) {
		if c.method != "workload:errored" || c.closed["startedAt"] == nil {
			t.Fatalf("a card that ran and deferred is a failure with its start: %s %v", c.method, c.closed)
		}
	})
}

// The same rule on a ledger row that no call opened a card for (a text or vision call, which only ever
// has the one terminal card).
func TestAHeldLedgerRowIsAQuietTerminalCard(t *testing.T) {
	e := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1", AppDir: writePairAppDir(t), OpenDir: t.TempDir()})
	row := ledger.Entry{TS: 1_700_000_000, Task: "vqa", ModelTier: "qwen3-vl-8b", Deferred: true, ErrClass: core.ErrClassGPUBusy,
		Reason: "gpu busy: generation job holds the lock", LatencyMs: 2000}
	ev := e.FromLedger(row)
	if ev.State != "completed" || ev.StartedAt != 0 || ev.Error != "gpu busy: generation job holds the lock" || ev.CompletedAt != 1_700_000_000_000 {
		t.Fatalf("a held row: %+v", ev)
	}
	row.ErrClass = "timeout"
	ev = e.FromLedger(row)
	if ev.State != "failed" || ev.StartedAt == 0 || ev.Error == "" {
		t.Fatalf("any other deferred row is still a failure, started when it began: %+v", ev)
	}
	row.Deferred, row.ErrClass, row.Reason = false, "", ""
	if ev = e.FromLedger(row); ev.State != "completed" || ev.Error != "" || ev.StartedAt == 0 {
		t.Fatalf("a successful row: %+v", ev)
	}
}

func TestCardOutcomeTable(t *testing.T) {
	cases := []struct {
		name      string
		res       core.Result
		started   bool
		wantState string
		wantErr   string
	}{
		{"success", core.Result{OK: true}, true, "completed", ""},
		{"success that never marked working (a cache hit)", core.Result{OK: true}, false, "completed", ""},
		{"queued answer", heldResult(), false, "completed", heldReason},
		{"busy card", core.Deferf("gpu busy: x", "", core.Meta{ErrClass: "gpu_busy"}), false, "completed", "gpu busy: x"},
		{"queued answer after the lane started", heldResult(), true, "failed", heldReason},
		{"render failure", core.Deferf("image generation failed: boom", "", core.Meta{ErrClass: "timeout"}), true, "failed", "image generation failed: boom"},
		{"capacity class alone is not a held card", func() core.Result {
			r := core.Deferf("no eligible node", "", core.Meta{})
			r.DeferClass = core.DeferClassCapacity
			return r
		}(), false, "failed", "no eligible node"},
		{"deferred with no reason", core.Deferf("", "", core.Meta{}), false, "failed", "deferred"},
		{"not ok and not deferred", core.Result{OK: false, Reason: "agent task failed"}, false, "failed", "agent task failed"},
		{"a held deferral with no reason", core.Deferf("", "", core.Meta{ErrClass: "gpu_queued"}), false, "completed", "deferred"},
	}
	for _, tc := range cases {
		state, errText := CardOutcome(tc.res, tc.started)
		if state != tc.wantState || errText != tc.wantErr {
			t.Errorf("%s: got (%s, %q), want (%s, %q)", tc.name, state, errText, tc.wantState, tc.wantErr)
		}
	}
}

// The terminal verdict is on disk BEFORE it is posted. A producer killed while its terminal post is in
// flight (the register's in-flight marker is all that is left of an unparked card) would have the sweep
// say "harness process exited before the job finished" over a job whose outcome it knew; parked, the
// sweep sends the outcome itself.
func TestTheTerminalVerdictIsOnDiskBeforeItIsPosted(t *testing.T) {
	r := &orphanRig{pair: &capture{}, appDir: writePairAppDir(t), dir: t.TempDir()}
	release := make(chan struct{})
	var once sync.Once
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(peekBody(req), `"workload:completed"`) {
			held := false
			once.Do(func() { held = true })
			if held {
				close(arrived)
				<-release // the producer's own terminal post hangs here: it is killed before it lands
			}
		}
		r.pair.handler(w, req)
	}))
	defer srv.Close()
	r.url = srv.URL
	p := r.emitter(nil, nil)
	defer p.Wait()       // runs after the release below: the hung post lands, then the server closes
	defer close(release) // runs first
	p.Emit(Event{JobID: "call-1", Model: "generate_image", Engine: "comfyui", State: "queued", CreatedAt: 1_000})
	p.Wait()
	p.Emit(Event{JobID: "call-1", Model: "comfyui:x", Engine: "comfyui", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 9_000})
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal post never reached the ingress")
	}
	files := r.files(t)
	if len(files) != 1 {
		t.Fatalf("one marker for the one open card, got %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var m openMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !m.Pending || string(m.Info["state"]) != `"completed"` || string(m.Info["startedAt"]) != "2000" || string(m.Info["completedAt"]) != "9000" {
		t.Fatalf("while the terminal post is in flight the marker must already hold the verdict as a pending frame: pending=%v info=%v", m.Pending, m.Info)
	}
	// The process dies here. The next one's sweep sends the verdict, not an orphan failure.
	sw := r.emitter(dead, nil)
	if n := sw.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the sweep must deliver the parked verdict: %d", n)
	}
	if len(r.failedFrames()) != 0 {
		t.Fatalf("a job that completed was closed failed: %v", r.failedFrames())
	}
	var verdict map[string]any
	for i := 0; i < r.pair.count(); i++ {
		if r.pair.method(i) == "workload:completed" {
			verdict = r.pair.info(i)
		}
	}
	if verdict == nil || verdict["startedAt"].(float64) != 2000 || verdict["completedAt"].(float64) != 9000 {
		t.Fatalf("the swept frame must be the producer's own verdict: %v", verdict)
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a delivered verdict leaves no marker: %v", files)
	}
}

// peekBody reads a request's body and puts it back for the handler behind.
func peekBody(req *http.Request) string {
	b, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(b))
	return string(b)
}

// The call's door waits for its card to be closed, but only so long: a PAIR that hangs costs the call
// the post's bound, never its answer, and the verdict is parked for the sweep.
func TestAHungPairDelaysTheAnswerByItsBoundNotForever(t *testing.T) {
	r := &orphanRig{pair: &capture{}, appDir: writePairAppDir(t), dir: t.TempDir()}
	var hang sync.WaitGroup
	hang.Add(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(peekBody(req), `"workload:completed"`) {
			hang.Wait()
			return
		}
		r.pair.handler(w, req)
	}))
	defer srv.Close()
	defer hang.Done()
	r.url = srv.URL
	e := r.emitter(nil, nil)
	_, _, end := e.Begin("generate_image", "offload_generate_image")
	e.Wait()
	start := time.Now()
	end(heldResult())
	// The post is bounded at sendTimeout; the slack is for a box that is busy, not for a wait that has no bound.
	if took := time.Since(start); took > sendTimeout+3*time.Second {
		t.Fatalf("end held the call %s behind a hung PAIR; the post is bounded at %s", took, sendTimeout)
	}
	files := r.files(t)
	if len(files) != 1 {
		t.Fatalf("a verdict PAIR could not take stays in the register for the sweep: %v", files)
	}
	raw, _ := os.ReadFile(filepath.Join(r.dir, files[0]))
	var m openMarker
	if err := json.Unmarshal(raw, &m); err != nil || !m.Pending || string(m.Info["state"]) != `"completed"` {
		t.Fatalf("the parked frame is the call's verdict: %s", raw)
	}
}

// A door that answers and is killed, for real: the child opens a card, the call is held back, it
// closes its card and "answers" (prints), and the parent kills it with the PAIR it posted to still
// slow. The production liveness rule then sweeps the register: it must find nothing.
func TestACallThatAnsweredAndWasKilledLeavesNothingForTheSweep(t *testing.T) {
	if os.Getenv("PAIRWORKLOADS_CLOSE_CHILD") == "1" {
		e := New(Config{Enabled: true, Endpoint: os.Getenv("PAIRWORKLOADS_URL"), AppDir: os.Getenv("PAIRWORKLOADS_APPDIR"), OpenDir: os.Getenv("PAIRWORKLOADS_DIR")})
		l, err := ledger.Open(os.Getenv("PAIRWORKLOADS_LEDGER"))
		if err != nil {
			os.Stdout.WriteString("ledger: " + err.Error() + "\n")
			return
		}
		e.AttachLedger(l)
		id, _, end := e.Begin("generate_image", "offload_generate_image")
		_ = l.Record(*heldRow(id))
		end(heldResult())
		os.Stdout.WriteString("answered\n")
		time.Sleep(time.Minute) // killed by the parent long before this ends
		return
	}
	r := newCloseRig(t, 200*time.Millisecond)
	cmd := exec.Command(os.Args[0], "-test.run=^TestACallThatAnsweredAndWasKilledLeavesNothingForTheSweep$")
	cmd.Env = append(os.Environ(), "PAIRWORKLOADS_CLOSE_CHILD=1", "PAIRWORKLOADS_URL="+r.url, "PAIRWORKLOADS_APPDIR="+r.appDir,
		"PAIRWORKLOADS_DIR="+r.dir, "PAIRWORKLOADS_LEDGER="+filepath.Join(t.TempDir(), "ledger.jsonl"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = cmd.Process.Kill() // the runner kills the door the moment it has the reply
	_ = cmd.Wait()
	if !strings.Contains(line, "answered") {
		t.Fatalf("the child did not answer: %q", line)
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("the killed door left %v in the register", files)
	}
	s := New(Config{Enabled: true, Endpoint: r.url, AppDir: r.appDir, OpenDir: r.dir}) // production liveness: gpulease.PIDAlive
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("the sweep closed %d card(s) of a call that had answered", n)
	}
	var sawClose bool
	for i := 0; i < r.pair.count(); i++ {
		switch r.pair.method(i) {
		case "workload:errored":
			t.Fatalf("a call that answered a place in line was closed failed: %v", r.pair.info(i))
		case "workload:completed":
			sawClose = true
		}
	}
	if !sawClose {
		t.Fatalf("the card was never closed before the door answered (%d frames)", r.pair.count())
	}
}

// The remote lanes' card closes the same way (RemoteCall.Finish): inline, so the door that returns the
// node's answer may be killed right after; a node that answered "the card is held" ran nothing, so its
// card closes quiet with no start, though a node DID answer; a node that ran the job and failed is a
// failure that started.
func TestARemoteCallClosesBeforeItsDoorAnswers(t *testing.T) {
	cases := []struct {
		name        string
		running     bool // the node's job state turned running before it answered
		res         core.Result
		wantMethod  string
		wantStarted bool
		wantErr     string
	}{
		{"the node's card was held", false, core.Deferf("gpu busy: another generation job holds the card", "", core.Meta{Node: "node-b-fleet16", ErrClass: "gpu_busy"}),
			"workload:completed", false, "gpu busy: another generation job holds the card"},
		// A media job is claimed to running when the node admits it, before its lane waits for the card:
		// the polls see "running" and the answer is still "the card is held". Quiet all the same; the
		// start the card showed stays, none is invented.
		{"the node admitted it, then its card was held", true, core.Deferf("gpu busy: another generation job holds the card", "", core.Meta{Node: "node-b-fleet16", ErrClass: "gpu_busy"}),
			"workload:completed", true, "gpu busy: another generation job holds the card"},
		{"the node ran it and it broke", true, core.Deferf("image generation failed: boom", "", core.Meta{Node: "node-b-fleet16", ErrClass: "timeout"}),
			"workload:errored", true, "image generation failed: boom"},
		{"the node broke before the polls saw it run", false, core.Deferf("image generation failed: boom", "", core.Meta{Node: "node-b-fleet16", ErrClass: "timeout"}),
			"workload:errored", true, "image generation failed: boom"},
		{"the node rendered it", false, core.Result{OK: true, Meta: core.Meta{Node: "node-b-fleet16", Model: "comfyui:x"}},
			"workload:completed", true, ""},
		{"no node answered, a placement defer", false, core.Deferf("dispatch http://node-b:18811: status 503: queue full", "", core.Meta{}),
			"workload:errored", false, "dispatch http://node-b:18811: status 503: queue full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCloseRig(t, 120*time.Millisecond)
			e := r.emitter(nil, nil)
			h := NewRemoteCall(e, nil, core.Request{Task: core.TaskGenerateImage, Door: "offload_generate_image"}, "remote")
			h.Dispatched("http://node-b:18811", "node-b-fleet16", "media-abc123")
			if tc.running {
				h.Running()
			}
			e.Wait()
			h.Finish(tc.res)
			// The door answers here.
			c := r.readCard(t, "media-abc123")
			if c.open == nil || c.closed == nil {
				t.Fatalf("the card must be opened and closed on the wire when the door answers: %d frame(s)", r.pair.count())
			}
			if c.method != tc.wantMethod {
				t.Fatalf("closed %s, want %s: %v", c.method, tc.wantMethod, c.closed)
			}
			if (c.closed["startedAt"] != nil) != tc.wantStarted {
				t.Fatalf("startedAt = %v, want started=%v", c.closed["startedAt"], tc.wantStarted)
			}
			if got, _ := c.closed["error"].(string); got != tc.wantErr {
				t.Fatalf("error = %q, want %q", got, tc.wantErr)
			}
			r.nothingLeftForTheSweep(t)
		})
	}
}

// The relay carries the quiet close like any other frame: the strict decode a member applies takes a
// completed frame that carries an error and no start, and keeps the reason, so a box with no PAIR of its
// own (a thin client reporting through a member) closes a held call's card the same way.
func TestARelayedHeldCardCloseIsTakenByTheMembersDecode(t *testing.T) {
	r := newFakeRelay(t)
	e, _ := relayEmitter(t, r)
	e.EmitSync(Event{JobID: "call-9-1", Model: "generate_image", Engine: "comfyui", State: "completed", Error: heldReason,
		Requester: "offload-harness/sess-9", CreatedAt: 5000, CompletedAt: 6000})
	if r.count() != 1 {
		t.Fatalf("relay hits = %d, want the one close, posted before EmitSync returned", r.count())
	}
	raw, err := json.Marshal(r.hit(0).body)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := ParseRelay(raw, "node-q")
	if err != nil {
		t.Fatalf("the member's decode refused the quiet close: %v", err)
	}
	if ev.State != "completed" || ev.StartedAt != 0 || !strings.HasPrefix(ev.Error, "gpu queued: card(s) 0000 held by media") {
		t.Fatalf("the member must read a completed, unstarted card carrying the reason: %+v", ev)
	}
}
