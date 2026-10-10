package mediaremote

// The fix round after the S2 review (P0 plan S2; ADR 0083): one test per defect the two reviewers found, written red against
// the code they read (the tree at the S2 tip) and green on the fix. Each test names the defect it guards.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// ---- the directory lock --------------------------------------------------------------------------------------------------

// Two contenders are never inside the lock at once, even when the lock a crashed holder left behind is as old as a lock can
// be. The first design broke a stale lock by removing the file, and a slow contender removed the fresh lock of the one that
// had just broken it (24 of 300 rounds had two inside, measured on the S2 tip).
func TestTheDirectoryLockNeverHasTwoHolders(t *testing.T) {
	st := &store{dir: filepath.Join(t.TempDir(), queueDirName)}
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(st.dir, lockName)
	const contenders, rounds = 6, 200
	var inside, twoInside atomic.Int32
	for round := 0; round < rounds; round++ {
		_ = os.WriteFile(lock, []byte("1\n"), 0o644)
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(lock, old, old)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for c := 0; c < contenders; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := st.withLock(func() error {
					if inside.Add(1) > 1 {
						twoInside.Add(1)
					}
					time.Sleep(3 * time.Millisecond)
					inside.Add(-1)
					return nil
				})
				if err != nil {
					t.Errorf("a contender could not take the lock: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
	if n := twoInside.Load(); n != 0 {
		t.Fatalf("two holders were inside the lock at once %d times", n)
	}
}

// A holder whose process ends, however it ends, frees the lock at once: nothing has to judge the lock stale, and a claim is
// not left waiting for a crashed process's lock to age.
func TestACrashedHoldersLockIsFreedWhenItsProcessEnds(t *testing.T) {
	if dir := os.Getenv("MR_LOCK_HOLDER_DIR"); dir != "" {
		// The child: take the lock, say so, and hold it until killed.
		st := &store{dir: dir}
		_ = st.withLock(func() error {
			fmt.Println("locked")
			_, _ = io.Copy(io.Discard, os.Stdin)
			return nil
		})
		os.Exit(0)
	}
	dir := filepath.Join(t.TempDir(), queueDirName)
	st := &store{dir: dir}
	cmd := exec.Command(os.Args[0], "-test.run=^TestACrashedHoldersLockIsFreedWhenItsProcessEnds$")
	cmd.Env = append(os.Environ(), "MR_LOCK_HOLDER_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	locked := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := stdout.Read(buf)
			if strings.Contains(string(buf[:n]), "locked") {
				close(locked)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-locked:
	case <-time.After(20 * time.Second):
		t.Fatal("the holder process never took the lock")
	}
	prev := lockWait
	lockWait = 150 * time.Millisecond
	t.Cleanup(func() { lockWait = prev })
	if err := st.withLock(func() error { return nil }); err != errLockBusy {
		t.Fatalf("a live holder in another process keeps the lock: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lockWait = 5 * time.Second
	began := time.Now()
	if err := st.withLock(func() error { return nil }); err != nil {
		t.Fatalf("the lock of a process that ended is free: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the lock of a process that ended is free at once, not after it ages: %s", took)
	}
}

// ---- cancel and the claim ------------------------------------------------------------------------------------------------

// A cancel that lands while the scan is claiming a node (the claim waits for the directory lock) ends the ticket BEFORE
// anything is sent: the claim is let go, the file the claim wrote is removed, no job goes to the node, and the ticket is
// finished once. On the S2 tip the job was sent after the cancel and finish closed the ticket's channel a second time.
func TestACancelDuringTheClaimSendsNothingAndFinishesOnce(t *testing.T) {
	e := newQEnv(t, 1, true)
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	forgetFirstReading(lt)
	e.free(0) // an idle matching node: the next scan claims it
	st, _ := storeFor(e.cfg)

	// Someone else holds the directory lock, so the claim in startRemote waits.
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = st.withLock(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	scanned := make(chan struct{})
	go func() {
		q.cycle(context.Background())
		close(scanned)
	}()
	waitFor(t, 5*time.Second, "the scan is claiming the node", func() bool {
		lt.mu.Lock()
		defer lt.mu.Unlock()
		return lt.running
	})
	if !lt.cancelUndispatched("canceled by the test") {
		close(release)
		t.Fatal("a ticket that has sent nothing can be canceled")
	}
	close(release)
	select {
	case <-scanned:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan never came back")
	}
	time.Sleep(300 * time.Millisecond) // a job wrongly sent would be on the node by now
	if n := len(e.nodes[0].posts()); n != 0 {
		t.Errorf("nothing is sent for a canceled ticket: %d POST(s)", n)
	}
	if files := ticketFiles(t, e.cfg); len(files) != 0 {
		t.Errorf("the canceled ticket's file is gone, the claim did not bring it back: %v", files)
	}
	if claims := queueFiles(t, e.cfg, "use-"); len(claims) != 0 {
		t.Errorf("the node is let go: %v", claims)
	}
	if !isDone(lt) {
		t.Error("the canceled ticket is finished")
	}
}

// ---- the job's id and adoption ------------------------------------------------------------------------------------------

// The ticket names its job BEFORE the job is sent: at the instant the node receives the POST, the ticket on disk says
// "placing" and carries the id the POST carries. Without it a process that died in that window left a ticket nobody could
// poll.
func TestTheTicketNamesItsJobBeforeThePost(t *testing.T) {
	e := newQEnv(t, 1, true)
	st, _ := storeFor(e.cfg)
	var mu sync.Mutex
	var atPost ticketFile
	var posted string
	e.nodes[0].beforePost = func(body []byte) {
		var b struct {
			JobID string `json:"job_id"`
		}
		_ = json.Unmarshal(body, &b)
		mu.Lock()
		defer mu.Unlock()
		posted = b.JobID
		for _, tf := range st.list() {
			atPost = tf
		}
	}
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	forgetFirstReading(lt)
	e.free(0)
	q.cycle(context.Background())
	waitFor(t, 10*time.Second, "the node renders it", func() bool { return isDone(lt) })
	mu.Lock()
	defer mu.Unlock()
	if posted == "" {
		t.Fatal("the node received no job")
	}
	if atPost.State != stPlacing || atPost.Job != posted {
		t.Errorf("at the POST the ticket on disk is placing with the job id %q, got state %q job %q", posted, atPost.State, atPost.Job)
	}
}

// An owner that died after it claimed a node and before the POST left nothing on the node: the adopter finds the node
// denies the job, and the ticket goes back in line with its arrival instead of failing the caller with an infrastructure error.
func TestADeadOwnersPlacingTicketThatWasNeverSentGoesBackInLine(t *testing.T) {
	e := newQEnv(t, 1, false)
	req := e.req("a red door", nil)
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadplacing01", ReqKey: reqKey(req), Created: nowMs() - 90_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlacing, Node: "node-a", Base: e.nodes[0].srv.URL,
		Job: "media-deadbeef00000009", LaneWhy: "card(s) c2 held by a media-class lease"}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}
	res := runDoor(e.cfg, e.lr, req, 5*time.Second)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the ticket the dead owner never sent goes back in line and runs on the idle node: %+v", res)
	}
	if got := len(e.nodes[0].posts()); got != 1 {
		t.Errorf("the request is sent once, by the adopter's scan: %d POSTs", got)
	}
	if strings.Contains(res.Meta.Placement, "taken over") {
		t.Errorf("nothing was taken over, the job was never on the node: %q", res.Meta.Placement)
	}
}

// A placing ticket that records no job id (a file an older build wrote) cannot be polled: the answer says the job may be on a
// node, and nothing is sent to /fleet/jobs/ with an empty id.
func TestAPlacingTicketWithNoJobIdIsNeverPolled(t *testing.T) {
	e := newQEnv(t, 1, false)
	req := e.req("a red door", nil)
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadplacing02", ReqKey: reqKey(req), Created: nowMs() - 90_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlacing, Node: "node-a", Base: e.nodes[0].srv.URL}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}
	res := runDoor(e.cfg, e.lr, req, callWindow)
	if res.OK || !strings.Contains(res.Reason, "no job id") {
		t.Fatalf("the answer says the ticket records no job id: %+v", res)
	}
	for _, r := range e.nodes[0].requests() {
		if strings.HasPrefix(r.path, "/fleet/jobs/") && strings.TrimPrefix(r.path, "/fleet/jobs/") == "" {
			t.Errorf("a poll with an empty job id: %s %s", r.method, r.path)
		}
	}
	if got := len(e.nodes[0].posts()); got != 0 {
		t.Errorf("nothing is sent when the ticket may already be on a node: %d POSTs", got)
	}
}

// A job a dead owner sent, that the node accepted and then passed back (another job held its card, or it could not take its
// lease: nothing ran), re-enters the line, as it does for a live owner: the caller is not handed a failure for a job that can
// still run, and the node that passed it back is left alone for a while instead of being sent the same job at once.
func TestAnAdoptedJobTheNodePassedBackGoesBackInLine(t *testing.T) {
	for _, class := range []string{core.ErrClassGPUBusy, core.ErrClassGPUQueued, core.ErrClassGPULeaseUnavailable} {
		t.Run(class, func(t *testing.T) {
			e := newQEnv(t, 1, false, func(i int, o *nodeOpts) {
				o.deferAs, o.deferClass = "the card is held by another job", class
			})
			req := e.req("a red door", nil)
			n := e.nodes[0]
			local, _, _ := localIdentity(e.cfg, "qwen-image-2.1-fast")
			jobID := "media-deadbeef00000003"
			body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": taskImage, "payload": map[string]any{
				"prompt": req.Input, "family": "qwen-image-2.1", "seed": 424242, "width": 2048, "height": 2048, "recipe_digest": local.recipe.Digest()}})
			if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, "the node ends the job", func() bool {
				j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
				return err == nil && j.State == "error"
			})
			st, _ := storeFor(e.cfg)
			dead := ticketFile{ID: "mq-deadbeef0002", ReqKey: reqKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
				Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
				LaneWhy: "card(s) c2 held by a media-class lease"}
			if err := st.write(dead); err != nil {
				t.Fatal(err)
			}
			res := runDoor(e.cfg, e.lr, req, 2*time.Second)
			a := answerOf(t, res) // a queued answer, not the node's failure
			if a.Ticket == "" {
				t.Fatalf("the ticket is still in line: %+v", res)
			}
			if got := len(n.posts()); got != 1 {
				t.Errorf("the node that passed the job back is left alone: the one POST is the dead owner's, got %d", got)
			}
		})
	}
}

// ---- the lost race on the local lane ------------------------------------------------------------------------------------

// A lane that reads free while the run is refused at once (gpu_wait_ms 0, or a younger place on the same card) does not turn
// into a loop of runs: the ticket tries the lane again only after a pause, and meanwhile looks at the fleet, so an idle node
// takes the job. On the S2 tip a lost race nudged the scanner and the run was repeated with nothing between (226 runs in 2 s
// measured, each writing the pipeline's own deferred row), and the fleet was never looked at.
func TestALostRaceOnTheLocalLaneIsNotARetryLoopAndTheFleetIsTriedMeanwhile(t *testing.T) {
	e := newQEnv(t, 1, true)
	lost := core.Deferf("gpu queued: card(s) c2 held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	lost.DeferClass = core.DeferClassCapacity
	e.lr.local = lost
	req := e.req("a red door", nil)
	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce)) // the lane is held: a ticket
	e.lr.set(core.LaneVerdict{Free: true})              // the reading says free from now on; every run is refused at once
	waitFor(t, 5*time.Second, "the first run is refused", func() bool { return len(e.lr.ranCalls()) >= 1 })
	time.Sleep(600 * time.Millisecond) // fifteen scans at the test's rhythm
	if runs := len(e.lr.ranCalls()); runs > 2 {
		t.Fatalf("a refused run is not repeated at once: %d runs in 0.6 s", runs)
	}
	e.free(0) // an idle matching node
	deadline := time.Now().Add(10 * time.Second)
	var res core.Result
	for time.Now().Before(deadline) {
		res = runDoor(e.cfg, e.lr, req, AtOnce)
		if res.OK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("while the lane keeps refusing, the idle node takes the ticket: %+v", res)
	}
	if runs := len(e.lr.ranCalls()); runs > 2 {
		t.Errorf("the lane was not asked again while it kept refusing: %d runs", runs)
	}
	_ = a
}

// ---- a lane reading the pipeline could not make ------------------------------------------------------------------------

func unjudgedLane() core.LaneVerdict {
	return core.LaneVerdict{Free: true, Unjudged: true, Why: "not judged: the card table could not be read (context deadline exceeded); the call reads it itself"}
}

// A lane the pipeline could not read is not a lane that is free. The call becomes a ticket, the fleet is looked at in the
// same scan, and an idle matching node takes the job; the run on this machine is the answer only when no node is. On the S2
// tip an unread lane ran locally at once and the fleet was never looked at.
func TestAnUnreadLaneStillLooksAtTheFleet(t *testing.T) {
	e := newQEnv(t, 1, false) // an idle matching node
	e.lr.set(unjudgedLane())
	e.lr.local = core.Deferf("gpu queued: card(s) c2 held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	res := runDoor(e.cfg, e.lr, e.req("a red door", nil), callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the idle node takes a call whose lane could not be read: %+v", res)
	}
	if !strings.Contains(res.Meta.Placement, "could not be judged") {
		t.Errorf("the placement says the lane could not be judged, not that it was busy: %q", res.Meta.Placement)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("no run on this machine's lane when a node took the job: %d", runs)
	}
}

// With no node to take it an unread lane runs the call here, in the scan that found no node, as it always did.
func TestAnUnreadLaneWithNoNodeRunsHere(t *testing.T) {
	e := newQEnv(t, 1, true) // the only node is busy
	e.lr.set(unjudgedLane())
	res := runDoor(e.cfg, e.lr, e.req("a red door", nil), callWindow)
	if !res.OK || string(res.Data) != `{"local":true}` {
		t.Fatalf("the call runs on this machine's lane: %+v", res)
	}
	if _, runs := e.lr.counts(); runs != 1 {
		t.Errorf("one run: %d", runs)
	}
}

// ---- places and results -------------------------------------------------------------------------------------------------

// slowProbe is a runner whose lane question blocks (after the first, which the call itself asks) until the test lets it go,
// the way a card-table read that is slow because the card is busy does.
type slowProbe struct {
	*keeperRunner
	gate  chan struct{}
	calls atomic.Int32
}

func (s *slowProbe) MediaLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	if s.calls.Add(1) > 1 {
		select {
		case <-s.gate:
		case <-ctx.Done():
		}
	}
	return s.keeperRunner.MediaLaneFree(ctx, req)
}

// A ticket's place in the local line is touched on its own rhythm, not only at the turn of its scan: a scan that spends its time
// in slow lane questions (one per waiting ticket) must not let the places of the tickets behind lapse. On the S2 tip a place
// was touched once per scan, so with enough tickets the gap passed the 30 s the line holds a place for.
func TestAPlaceIsTouchedWhileASlowScanIsRunning(t *testing.T) {
	e := newQEnv(t, 1, true)
	sp := &slowProbe{keeperRunner: e.lr, gate: make(chan struct{})}
	t.Cleanup(func() { close(sp.gate) })
	a := answerOf(t, runDoor(e.cfg, sp, e.req("a red door", nil), AtOnce))
	st, _ := storeFor(e.cfg)
	tf, _ := st.read(a.Ticket)
	if tf.Place == "" {
		t.Fatal("the ticket holds a place")
	}
	// Let the scan reach its (now blocked) lane question, then count the touches over a window in which no scan finishes.
	waitFor(t, 5*time.Second, "a scan is inside its lane question", func() bool { return sp.calls.Load() >= 2 })
	before := e.lr.keptCount(tf.Place)
	time.Sleep(400 * time.Millisecond)
	if got := e.lr.keptCount(tf.Place) - before; got < 3 {
		t.Fatalf("the place is touched on its own rhythm while the scan waits on a slow read: %d touches in 0.4 s", got)
	}
}

// A finished result nobody collected is kept for hours, as a job renders for minutes to hours and the caller was told to
// come back for it; a failure is kept only for a short while, as a repeat of the request should try again. On the S2 tip
// both went after ten minutes, and a caller who obeyed "do not call again" found its finished job gone and sent a second render.
func TestAFinishedResultNobodyCollectedIsKeptForHoursAFailureIsNot(t *testing.T) {
	e := newQEnv(t, 1, true)
	st, _ := storeFor(e.cfg)
	long := nowMs() - int64((2 * time.Hour).Milliseconds())
	okRes, _ := json.Marshal(core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)})
	failRes, _ := json.Marshal(core.Deferf("the node went away", "", core.Meta{}))
	for _, f := range []ticketFile{
		{ID: "mq-doneok0000001", ReqKey: "k1", Created: long, Owner: deadOwner, Beat: long, Updated: long, State: stDone, Result: okRes},
		{ID: "mq-donefail00001", ReqKey: "k2", Created: long, Owner: deadOwner, Beat: long, Updated: long, State: stFailed, Result: failRes},
	} {
		// write stamps Updated with now; the age of a finished ticket is its beat and update, so write the file by hand.
		b, _ := json.Marshal(f)
		if err := writeFileAtomic(st.path(f.ID), b); err != nil {
			t.Fatal(err)
		}
	}
	st.sweep(st.list())
	left := map[string]bool{}
	for _, f := range ticketFiles(t, e.cfg) {
		left[strings.TrimSuffix(f, ".json")] = true
	}
	if !left["mq-doneok0000001"] {
		t.Error("a finished, uncollected result stays for hours")
	}
	if left["mq-donefail00001"] {
		t.Error("an old uncollected failure goes: a repeat of the request should try again")
	}
}

// A result a call took stays on file, marked collected, for a short while: a second call that names the ticket (an MCP door
// and a CLI of another process waiting on the same job, each told the token) gets the result too, and no second render is
// started for it. A request that only repeats the content is a new job, as before. On the S2 tip the first call removed the
// file, and the second was told to send the request again, which rendered it twice.
func TestASecondCallNamingTheTicketStillGetsTheResultTheFirstTook(t *testing.T) {
	e := newQEnv(t, 1, true)
	q1, _ := manual(t)
	// Nobody scans the new ticket at the end (the scanner is off), so its first look must not be waited for.
	prev := firstScanBound
	firstScanBound = 20 * time.Millisecond
	t.Cleanup(func() { firstScanBound = prev })
	req := e.req("a red door", map[string]any{"seed": 11})
	lt := admitOn(t, q1, e.cfg, e.lr, req)
	forgetFirstReading(lt)
	e.free(0)
	q1.cycle(context.Background())
	waitFor(t, 10*time.Second, "the node renders it", func() bool { return isDone(lt) })
	first := q1.deliver(lt)
	if !first.OK {
		t.Fatalf("the owner's caller takes the result: %+v", first)
	}

	named := req
	named.Params = map[string]any{}
	for k, v := range req.Params {
		named.Params[k] = v
	}
	named.Params["waiter_token"] = lt.t.ID
	res := runDoor(e.cfg, e.lr, named, callWindow)
	if !res.OK || string(res.Data) != string(first.Data) {
		t.Fatalf("the second call naming the ticket gets the same result: %+v | first %s", res, first.Data)
	}
	if again := runDoor(e.cfg, e.lr, named, callWindow); !again.OK || string(again.Data) != string(first.Data) {
		t.Errorf("every call naming the ticket gets it while the file stays: %+v", again)
	}
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Errorf("the job was rendered once: %d POSTs", n)
	}

	// A request that only repeats the content: the result was taken, so it is a new job (here the lane is busy: a ticket).
	e.hold(0)
	if a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce)); a.Ticket == lt.t.ID {
		t.Errorf("a repeat of the content is not the ticket whose result was taken: %s", a.Ticket)
	}
}

// A taken result goes after collectedKeep, and is not listed as a ticket meanwhile.
func TestATakenResultGoesAfterCollectedKeepAndIsNotListed(t *testing.T) {
	e := newQEnv(t, 1, true)
	st, _ := storeFor(e.cfg)
	okRes, _ := json.Marshal(core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)})
	stamp := func(id string, ago time.Duration) {
		at := time.Now().Add(-ago).UnixMilli()
		b, _ := json.Marshal(ticketFile{ID: id, ReqKey: id, Created: at, Owner: selfOwner(), Beat: at, Updated: at, Task: "generate_image", State: stDone, Result: okRes, Collected: true})
		if err := writeFileAtomic(st.path(id), b); err != nil {
			t.Fatal(err)
		}
	}
	stamp("mq-takenfresh001", collectedKeep/4)
	stamp("mq-takenold00001", collectedKeep*4)
	if rows := Queue(e.cfg); len(rows) != 0 {
		t.Errorf("a ticket whose result was taken is not a ticket in the status listing: %+v", rows)
	}
	st.sweep(st.list())
	if _, ok := st.read("mq-takenfresh001"); !ok {
		t.Error("a taken result stays for the other callers waiting on it")
	}
	if _, ok := st.read("mq-takenold00001"); ok {
		t.Error("a taken result goes after collectedKeep")
	}
}

// ---- finish, cancel, pauses and places: the pieces the tests above lean on ------------------------------------------------

// A ticket ends once. A cancel ends a ticket without making its state terminal, so the guard against a second end is its own
// flag; a send that was already under way when the cancel landed must not close the ticket's channel a second time.
func TestAFinishedTicketIsNeverFinishedAgain(t *testing.T) {
	e := newQEnv(t, 1, true)
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	if !lt.cancelUndispatched("canceled by the test") {
		t.Fatal("a waiting ticket can be canceled")
	}
	canceled := lt.res
	lt.finish(core.Result{OK: true, Data: json.RawMessage(`{"late":true}`)}, false) // a panic here is the defect
	if lt.res.OK || lt.res.Reason != canceled.Reason {
		t.Errorf("the cancel's result stands: %+v", lt.res)
	}
	if lt.cancelUndispatched("again") {
		t.Error("a ticket that ended cannot be canceled again")
	}
}

// A cancel that comes AFTER the job was sent cancels nothing: a job on a node cannot be recalled (ADR 0064), the ticket goes on
// and its result is the caller's.
func TestACancelAfterTheJobWasSentDoesNotCancelIt(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, true, func(i int, o *nodeOpts) { o.hold = hold })
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	forgetFirstReading(lt)
	e.free(0)
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the node has the job", func() bool { return len(e.nodes[0].posts()) == 1 })
	if lt.cancelUndispatched("canceled by the test") {
		close(hold)
		t.Fatal("a ticket whose job is on a node is not canceled")
	}
	close(hold)
	waitFor(t, 10*time.Second, "the job ends", func() bool { return isDone(lt) })
	if !lt.res.OK {
		t.Errorf("the job's result is the caller's: %+v", lt.res)
	}
}

// Refusals in a row pause the lane longer each time (the node steps), and a reading of busy starts the count over: the lane's
// state really changed, so the next time it reads free is a new question.
func TestRefusalsInARowPauseTheLaneLongerAndABusyReadingStartsOver(t *testing.T) {
	e := newQEnv(t, 1, true)
	lost := core.Deferf("gpu queued: card(s) c2 held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	lost.DeferClass = core.DeferClassCapacity
	e.lr.local = lost
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	forgetFirstReading(lt)
	e.lr.set(core.LaneVerdict{Free: true})
	want := []time.Duration{backoffSteps[0], backoffSteps[1], backoffSteps[2], backoffSteps[3], backoffSteps[3]}
	for i, w := range want {
		runs := len(e.lr.ranCalls())
		q.cycle(context.Background())
		waitFor(t, 5*time.Second, "the refused run comes back", func() bool {
			lt.mu.Lock()
			defer lt.mu.Unlock()
			return len(e.lr.ranCalls()) == runs+1 && !lt.running && lt.t.State == stWaiting
		})
		lt.mu.Lock()
		left, n := time.Until(lt.lostUntil), lt.lostN
		lt.lostUntil = time.Now().Add(-time.Second) // the test does not wait the pause out
		lt.mu.Unlock()
		if n != i+1 || left > w || left < w-2*time.Second {
			t.Errorf("refusal %d: %d in a row, paused for %s, want about %s", i+1, n, left, w)
		}
	}
	e.lr.set(heldLane())
	q.cycle(context.Background())
	lt.mu.Lock()
	n := lt.lostN
	lt.mu.Unlock()
	if n != 0 {
		t.Errorf("a reading of busy starts the count over: %d", n)
	}
}

// The place of a ticket whose job is on a node (or running here) is not kept alive: it would hold a card of this machine's line
// against others for a job that is not waiting for it.
func TestARunningTicketsPlaceIsNotKeptAlive(t *testing.T) {
	e := newQEnv(t, 1, true)
	a := answerOf(t, runDoor(e.cfg, e.lr, e.req("a red door", nil), AtOnce))
	st, _ := storeFor(e.cfg)
	tf, _ := st.read(a.Ticket)
	if tf.Place == "" {
		t.Fatal("the ticket holds a place")
	}
	defaultQ.mu.Lock()
	lt := defaultQ.live[a.Ticket]
	defaultQ.mu.Unlock()
	lt.mu.Lock()
	lt.running, lt.t.State = true, stPlaced
	lt.mu.Unlock()
	before := e.lr.keptCount(tf.Place)
	time.Sleep(300 * time.Millisecond) // seven keeper ticks at the test's rhythm
	if got := e.lr.keptCount(tf.Place) - before; got != 0 {
		t.Errorf("a ticket that is not waiting does not keep its place alive: %d touches", got)
	}
	lt.mu.Lock()
	lt.running, lt.t.State = false, stWaiting
	lt.mu.Unlock()
}

// The answer says how long a finished result is kept.
func TestTheAnswerSaysHowLongAFinishedResultIsKept(t *testing.T) {
	if got := nextText("mq-abc123def456"); !strings.Contains(got, "kept for 6 hours") {
		t.Errorf("the instruction to collect says until when: %s", got)
	}
}
