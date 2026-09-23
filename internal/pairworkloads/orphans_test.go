package pairworkloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// orphanRig is one PAIR ingress, one PAIR app dir and one register directory
// shared by a "producer" emitter (the process that dies) and any number of
// "sweeper" emitters (the processes that come after it).
type orphanRig struct {
	pair   *capture
	url    string
	appDir string
	dir    string
}

func newOrphanRig(t *testing.T) *orphanRig {
	t.Helper()
	r := &orphanRig{pair: &capture{}, appDir: writePairAppDir(t), dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(r.pair.handler))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// emitter builds an enabled emitter on the rig. alive and start are the
// liveness and process-start seams it applies to OTHER processes' markers
// (nil = production).
func (r *orphanRig) emitter(alive func(int) bool, start func(int) (int64, bool)) *Emitter {
	e := New(Config{Enabled: true, Endpoint: r.url, AppDir: r.appDir, OpenDir: r.dir})
	if alive != nil {
		e.alive = alive
	}
	if start != nil {
		e.procStart = start
	}
	return e
}

func (r *orphanRig) files(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(r.dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// failedFrames returns the workloadInfo of every workload:errored frame.
func (r *orphanRig) failedFrames() []map[string]any {
	var out []map[string]any
	for i := 0; i < r.pair.count(); i++ {
		if r.pair.method(i) == "workload:errored" {
			out = append(out, r.pair.info(i))
		}
	}
	return out
}

func dead(int) bool  { return false }
func alive(int) bool { return true }

// runningJob sends the queued and running frames of one delegation-shaped job
// and never its terminal frame: the process that sent them is about to die.
func runningJob(e *Emitter, id string) {
	e.Emit(Event{JobID: id, Model: "agent-pool", Engine: "vllm", State: "queued", Requester: "offload-harness/s1", CreatedAt: 1_000})
	e.Emit(Event{JobID: id, Model: "agent-pool", Engine: "vllm", State: "running", Requester: "offload-harness/s1", CreatedAt: 1_000, StartedAt: 2_000})
	e.Wait()
}

func TestOpenMarkerFollowsTheCard(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	runningJob(e, "agd-1")
	files := r.files(t)
	if len(files) != 1 || !strings.HasSuffix(files[0], "-agd-1.json") {
		t.Fatalf("an in-flight card must leave exactly one marker, got %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var m openMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.PID != os.Getpid() || string(m.Info["id"]) != `"agd-1"` || string(m.Info["state"]) != `"running"` || string(m.Info["startedAt"]) != "2000" {
		t.Fatalf("marker must hold this pid and the LAST in-flight frame: pid=%d info=%v", m.PID, m.Info)
	}
	e.Emit(Event{JobID: "agd-1", Model: "agent-pool", Engine: "vllm", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 3_000})
	e.Wait()
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("the terminal frame must remove the marker, left %v", files)
	}
}

// TestSweepClosesADeadProducersCard is the 2026-09-22 defect: a process sent
// a running frame and was killed. The next process to sweep must send the
// terminal frame the dead one never sent, for the SAME card, exactly once.
func TestSweepClosesADeadProducersCard(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "agd-91c2")
	var running map[string]any // background posts land in either order
	for i := 0; i < r.pair.count(); i++ {
		if r.pair.method(i) == "workload:started" {
			running = r.pair.info(i)
		}
	}

	s := r.emitter(dead, nil)
	at := time.UnixMilli(9_000_000)
	s.now = func() time.Time { return at }
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("sweep closed %d cards, want 1", n)
	}
	failed := r.failedFrames()
	if len(failed) != 1 {
		t.Fatalf("want exactly one failed frame, got %d", len(failed))
	}
	f := failed[0]
	for _, k := range []string{"id", "runId", "model", "engine", "originatedFrom", "scheduledOn", "createdAt", "startedAt", "requesterId"} {
		if f[k] != running[k] {
			t.Errorf("%s: orphan frame %v != running frame %v (a different identity is a different card)", k, f[k], running[k])
		}
	}
	if f["id"] != "agd-91c2" || f["createdAt"].(float64) != 1000 || f["state"] != "failed" || f["error"] != OrphanError || f["completedAt"].(float64) != 9_000_000 {
		t.Fatalf("orphan frame wrong: %v", f)
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a closed orphan's marker must be gone, left %v", files)
	}
	if n := s.SweepOrphans(context.Background()); n != 0 || len(r.failedFrames()) != 1 {
		t.Fatalf("a second sweep must send nothing: n=%d failed=%d", n, len(r.failedFrames()))
	}
}

func TestSweepLeavesALiveProducerAlone(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, func(int) (int64, bool) { return 111, true }), "agd-live")
	s := r.emitter(alive, func(int) (int64, bool) { return 111, true })
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("a live producer's card was closed (%d)", n)
	}
	if len(r.failedFrames()) != 0 || len(r.files(t)) != 1 {
		t.Fatalf("live card touched: failed=%d files=%v", len(r.failedFrames()), r.files(t))
	}
}

func TestSweepClosesARecycledPid(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, func(int) (int64, bool) { return 111, true }), "agd-recycled")
	s := r.emitter(alive, func(int) (int64, bool) { return 222, true })
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("a pid now owned by another process must read as gone: closed %d", n)
	}
}

func TestSweepLeakCap(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "agd-leak")
	s := r.emitter(alive, func(int) (int64, bool) { return 0, false })
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("a fresh marker of a live pid was closed")
	}
	s.now = func() time.Time { return time.Now().Add(OpenMaxAge + time.Minute) }
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("a marker past OpenMaxAge must be closed whatever its pid says: closed %d", n)
	}
}

func TestDisabledEmitterWritesAndSweepsNothing(t *testing.T) {
	r := newOrphanRig(t)
	off := New(Config{Enabled: false, Endpoint: r.url, AppDir: r.appDir, OpenDir: r.dir})
	off.Emit(Event{JobID: "agd-off", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1})
	off.Wait()
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a disabled emitter wrote %v", files)
	}
	runningJob(r.emitter(nil, nil), "agd-orphan")
	off.alive = dead
	if n := off.SweepOrphans(context.Background()); n != 0 || len(r.failedFrames()) != 0 || len(r.files(t)) != 1 {
		t.Fatalf("a disabled emitter swept: n=%d failed=%d files=%v", n, len(r.failedFrames()), r.files(t))
	}
}

// TestSweepKeepsTheClaimWhenPairIsDown: an orphan whose frame cannot be
// delivered stays in the register under its own name for the next sweep.
func TestSweepKeepsTheClaimWhenPairIsDown(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "agd-down")
	before := r.files(t)
	down := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/v1/workloads/events", AppDir: r.appDir, OpenDir: r.dir})
	down.alive = dead
	if n := down.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("an undeliverable orphan counted as closed")
	}
	if after := r.files(t); len(after) != 1 || after[0] != before[0] {
		t.Fatalf("the claim must be put back under its own name: before %v after %v", before, after)
	}
	s := r.emitter(dead, nil)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the next sweep with PAIR up must close it: %d", n)
	}
}

// TestRacingSweepersCloseEachCardOnce: two processes sweeping the same
// register at once send one terminal frame per orphan, never two.
func TestRacingSweepersCloseEachCardOnce(t *testing.T) {
	r := newOrphanRig(t)
	p := r.emitter(nil, nil)
	const jobs = 25
	for i := 0; i < jobs; i++ {
		p.Emit(Event{JobID: "agd-race-" + string(rune('a'+i)), Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 1})
	}
	p.Wait()
	a, b := r.emitter(dead, nil), r.emitter(dead, nil)
	var wg sync.WaitGroup
	for _, s := range []*Emitter{a, b} {
		wg.Add(1)
		go func(s *Emitter) { defer wg.Done(); s.SweepOrphans(context.Background()) }(s)
	}
	wg.Wait()
	// A marker busy under the other sweeper's read is left for the next pass.
	a.SweepOrphans(context.Background())
	seen := map[string]int{}
	for _, f := range r.failedFrames() {
		seen[f["id"].(string)]++
	}
	if len(seen) != jobs {
		t.Fatalf("closed %d distinct cards, want %d", len(seen), jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s closed %d times", id, n)
		}
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("register not empty after the sweeps: %v", files)
	}
}

// TestFirstEmitSweepsOnce: a harness process that reports anything closes
// what a dead process left open, once, without being asked.
func TestFirstEmitSweepsOnce(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "agd-left")
	e := r.emitter(dead, nil)
	e.Emit(Event{JobID: "led-1", Model: "gemma-4-e4b", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 2})
	e.Emit(Event{JobID: "led-2", Model: "gemma-4-e4b", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 2})
	e.Wait()
	failed := r.failedFrames()
	if len(failed) != 1 || failed[0]["id"] != "agd-left" {
		t.Fatalf("first Emit must sweep the dead process's card once: %v", failed)
	}
}

// TestSeatWatchCardOfAKilledNodeIsClosed: fleet-serve killed with a
// seat-activity card open (no closeAll) leaves a marker the next sweep closes.
func TestSeatWatchCardOfAKilledNodeIsClosed(t *testing.T) {
	rig := newSeatRig(t)
	dir := t.TempDir()
	rig.w.em.cfg.OpenDir = dir
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	if rig.pair.count() != 1 || rig.pair.method(0) != "workload:started" {
		t.Fatalf("seat card did not open: %v", rig.states())
	}
	id := rig.pair.info(0)["id"]
	// The node dies here: no closeAll, no terminal frame.
	s := New(Config{Enabled: true, Endpoint: rig.w.em.cfg.Endpoint, AppDir: rig.w.em.appDir, OpenDir: dir})
	s.alive = dead
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the killed node's seat card was not closed: %d", n)
	}
	last := rig.pair.count() - 1
	if rig.pair.method(last) != "workload:errored" || rig.pair.info(last)["id"] != id || rig.pair.info(last)["requesterId"] != SeatRequester {
		t.Fatalf("wrong closing frame: %v %v", rig.pair.method(last), rig.pair.info(last))
	}
}

// TestKilledProcessCardIsClosed runs the defect for real: a child process
// sends a running frame and is killed before its terminal frame; a sweep with
// the production liveness rule (gpulease.PIDAlive, ProcessStart) closes it.
func TestKilledProcessCardIsClosed(t *testing.T) {
	if os.Getenv("PAIRWORKLOADS_ORPHAN_CHILD") == "1" {
		e := New(Config{Enabled: true, Endpoint: os.Getenv("PAIRWORKLOADS_URL"), AppDir: os.Getenv("PAIRWORKLOADS_APPDIR"), OpenDir: os.Getenv("PAIRWORKLOADS_DIR")})
		e.Emit(Event{JobID: "agd-killed", Model: "agent-pool", Engine: "vllm", State: "running", CreatedAt: 1_000, StartedAt: 1_000})
		e.Wait()
		os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute) // killed by the parent long before this ends
		return
	}
	r := newOrphanRig(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledProcessCardIsClosed$")
	cmd.Env = append(os.Environ(), "PAIRWORKLOADS_ORPHAN_CHILD=1", "PAIRWORKLOADS_URL="+r.url,
		"PAIRWORKLOADS_APPDIR="+r.appDir, "PAIRWORKLOADS_DIR="+r.dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, _ := out.Read(buf); !strings.Contains(string(buf[:n]), "ready") {
		_ = cmd.Process.Kill()
		t.Fatalf("child did not report ready: %q", buf[:n])
	}
	s := New(Config{Enabled: true, Endpoint: r.url, AppDir: r.appDir, OpenDir: r.dir})
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("the child is alive; its card was closed (%d)", n)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the killed child's card was not closed: %d (files %v)", n, r.files(t))
	}
	failed := r.failedFrames()
	if len(failed) != 1 || failed[0]["id"] != "agd-killed" {
		t.Fatalf("closing frame: %v", failed)
	}
}

// TestUndeliveredTerminalFrameIsResent: a LIVE producer whose terminal post
// fails (PAIR restarting, a slow answer) must not drop the marker — that was
// the only record of a card PAIR still shows running — and must not leave the
// in-flight marker either, which would later close a finished job as failed.
// The marker becomes the pending terminal frame, and the next sweep delivers
// it with its real verdict without waiting for the producer to exit.
func TestUndeliveredTerminalFrameIsResent(t *testing.T) {
	r := newOrphanRig(t)
	var down sync.Mutex
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		down.Lock()
		no := refuse
		down.Unlock()
		if no {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		r.pair.handler(w, req)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	p := r.emitter(nil, nil)
	runningJob(p, "agd-late")
	down.Lock()
	refuse = true
	down.Unlock()
	p.Emit(Event{JobID: "agd-late", Model: "agent-pool", Engine: "vllm", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 3_000})
	p.Wait()
	files := r.files(t)
	if len(files) != 1 {
		t.Fatalf("an undelivered terminal frame must stay in the register, got %v", files)
	}
	down.Lock()
	refuse = false
	down.Unlock()
	s := r.emitter(alive, nil) // the producer is still alive
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the pending terminal frame was not delivered: %d", n)
	}
	last := r.pair.count() - 1
	if r.pair.method(last) != "workload:completed" || r.pair.info(last)["state"] != "completed" || r.pair.info(last)["completedAt"].(float64) != 3000 {
		t.Fatalf("resent frame must be the producer's own verdict: %v %v", r.pair.method(last), r.pair.info(last))
	}
	if len(r.failedFrames()) != 0 || len(r.files(t)) != 0 {
		t.Fatalf("failed=%d files=%v", len(r.failedFrames()), r.files(t))
	}
}
