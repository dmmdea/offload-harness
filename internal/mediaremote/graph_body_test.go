package mediaremote

// A graph ticket is the BYTES it was admitted with (ADR 0083 Amendment 1). A ticket may wait for hours, and the caller's graph file is
// the caller's: it is reused for the next shot, edited, or removed. The ticket's identity (the key), the payload a node receives, the
// graph this machine's own lane runs and the graph an adopter plans from all have to be those one set of bytes, so the ticket does
// the same thing whichever lane serves it and a finished result can always be collected by the token it was promised under.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// bodyRunner records what the graph and manifest files held at the moment a run was handed them: the fake lane reads nothing, and
// the real one reads the file when it starts, which is the moment that matters.
type bodyRunner struct {
	*keeperRunner
	bm        sync.Mutex
	graphs    []string
	manifests []string
}

func (b *bodyRunner) Run(ctx context.Context, req core.Request) core.Result {
	read := func(k string) string {
		path := str(req.Params, k)
		if path == "" {
			return ""
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "UNREADABLE: " + err.Error()
		}
		return string(raw)
	}
	b.bm.Lock()
	b.graphs = append(b.graphs, read("graph_path"))
	b.manifests = append(b.manifests, read("manifest_path"))
	b.bm.Unlock()
	return b.keeperRunner.Run(ctx, req)
}

func (b *bodyRunner) ran() (graphs, manifests []string) {
	b.bm.Lock()
	defer b.bm.Unlock()
	return append([]string(nil), b.graphs...), append([]string(nil), b.manifests...)
}

const (
	bodyManifestA = `{"models":[{"name":"x"}]}`
	bodyManifestB = `{"models":[{"name":"y"}]}`
)

// A caller that reuses one graph file writes shot 1, is told "queued", and writes shot 2 over it. The ticket of shot 1 renders shot 1
// on this machine's lane exactly as a node would have (the snapshot it was admitted with), the repeat of the path with other content
// is another request with a ticket of its own, and the copies are gone with the ticket that held them.
func TestAGraphTicketRunsTheBytesItWasAdmittedWith(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	br := &bodyRunner{keeperRunner: e.lr}
	q1, q2 := manual(t)
	ctx := context.Background()
	mpath := writeFile(t, t.TempDir(), "m.json", []byte(bodyManifestA))
	req := e.req(krea2Graph, map[string]any{"manifest_path": mpath})
	first := admitOn(t, q1, e.cfg, br, req)

	gpath := req.Params["graph_path"].(string)
	writeFile(t, filepath.Dir(gpath), filepath.Base(gpath), []byte(plainGraph))
	writeFile(t, filepath.Dir(mpath), filepath.Base(mpath), []byte(bodyManifestB))
	second := admitOn(t, q2, e.cfg, br, req) // the same paths, other content: another request
	if second.snapshot().ID == first.snapshot().ID {
		t.Fatalf("a graph file edited in place is another request, not the first ticket again")
	}

	q1.cycle(ctx) // the first scan stands on the reading the ticket was made with; the lane is still held
	e.lr.set(core.LaneVerdict{Free: true})
	q1.cycle(ctx) // only the first ticket's process scans: this machine's lane serves shot 1
	waitFor(t, 5*time.Second, "the first ticket runs on this machine's lane", func() bool { return isDone(first) })
	graphs, manifests := br.ran()
	if len(graphs) != 1 || graphs[0] != krea2Graph || manifests[0] != bodyManifestA {
		t.Fatalf("the lane must run the bytes shot 1 was admitted with (graph %q, manifest %q), got graphs %q manifests %q", krea2Graph, bodyManifestA, graphs, manifests)
	}
	kept := queueFiles(t, e.cfg, "body-")
	if len(kept) != 2 {
		t.Fatalf("shot 1's copies go with its ticket and shot 2's stay with the ticket that still waits: %v", kept)
	}
	for _, name := range kept {
		if !strings.Contains(name, second.snapshot().ID) {
			t.Errorf("%s is not the waiting ticket's copy", name)
		}
	}
}

// The same bytes reach a node: whichever lane serves the ticket, the graph it renders is the one it was admitted with.
func TestAGraphTicketSendsTheBytesItWasAdmittedWithToANode(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	req := e.req(krea2Graph, nil)
	graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	gpath := req.Params["graph_path"].(string)
	writeFile(t, filepath.Dir(gpath), filepath.Base(gpath), []byte(plainGraph))
	e.free(0)
	waitFor(t, 5*time.Second, "the ticket reaches the node", func() bool { return len(e.nodes[0].posts()) == 1 })
	if p := graphPayload(t, e.nodes[0]); string(p["graph"]) != krea2Graph {
		t.Errorf("the node is sent the graph the ticket was admitted with: %s", p["graph"])
	}
}

// The result a ticket promised is collectable by its token however the caller's files fared meanwhile: removed, or overwritten
// with the next shot. (The token is the handle; a repeat WITHOUT it attaches only by content, which the first test holds.)
func TestAGraphTicketIsCollectedByItsTokenOnceItsFilesAreGoneOrEdited(t *testing.T) {
	for name, fate := range map[string]func(t *testing.T, graph, manifest string){
		"removed": func(t *testing.T, graph, manifest string) {
			for _, p := range []string{graph, manifest} {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
		},
		"edited": func(t *testing.T, graph, manifest string) {
			writeFile(t, filepath.Dir(graph), filepath.Base(graph), []byte(plainGraph))
			writeFile(t, filepath.Dir(manifest), filepath.Base(manifest), []byte(bodyManifestB))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newGEnv(t)
			e.node("node-a", true, nodeOpts{}, krea2File)
			e.start()
			mpath := writeFile(t, t.TempDir(), "m.json", []byte(bodyManifestA))
			req := e.req(krea2Graph, map[string]any{"manifest_path": mpath})
			a := graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
			e.free(0)
			st, _ := storeFor(e.cfg)
			waitFor(t, 5*time.Second, "the ticket finishes on the node", func() bool {
				tf, ok := st.read(a.Ticket)
				return ok && terminalState(tf.State)
			})
			fate(t, req.Params["graph_path"].(string), mpath)

			collect := core.Request{Task: req.Task, Door: req.Door, Resumable: true, Params: map[string]any{}}
			for k, v := range req.Params {
				collect.Params[k] = v
			}
			collect.Params["waiter_token"] = a.Ticket
			res := runDoor(e.cfg, e.lr, collect, AtOnce)
			if !res.OK || res.Meta.Node != "node-a" {
				t.Fatalf("the token collects the finished result although the file is %s: %+v", name, res)
			}
		})
	}
}

// A job the dead owner of a ticket sent is adopted by the next call that names the ticket, and the adopter plans from the copies the
// ticket kept, so the caller's own files being gone does not turn "adopt" into "could not be adopted".
func TestADeadOwnersPlacedGraphIsAdoptedAfterTheCallersFilesAreGone(t *testing.T) {
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{}, krea2File)
	e.start()
	req := e.req(krea2Graph, nil)
	pl, err := plan(req)
	if err != nil {
		t.Fatal(err)
	}
	jobID := "media-deadbeef00000004"
	body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": pl.payload})
	if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the node finishes the job", func() bool {
		j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
		return err == nil && j.State == "done"
	})
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadbeef0004", ReqKey: reqKey(req), PathKey: pathKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: string(core.TaskRunGraph), Out: req.Params["out_dir"].(string), State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
		LaneWhy: "card(s) c2 held by a media-class lease"}
	if _, err := st.writeBodies(dead.ID, pl); err != nil {
		t.Fatal(err)
	}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(req.Params["graph_path"].(string)); err != nil {
		t.Fatal(err)
	}

	again := core.Request{Task: req.Task, Door: req.Door, Resumable: true, Params: map[string]any{"waiter_token": dead.ID}}
	for k, v := range req.Params {
		again.Params[k] = v
	}
	res := runDoor(e.cfg, e.lr, again, callWindow)
	if !res.OK || res.Meta.Node != "node-a" || !strings.Contains(res.Meta.Placement, "taken over") {
		t.Fatalf("the call that names the ticket adopts the job and brings its result, whatever became of the caller's file: %+v", res)
	}
	if got := len(n.posts()); got != 1 {
		t.Errorf("the graph was sent once, by the process that died: %d POSTs", got)
	}
	if left := queueFiles(t, e.cfg, "body-"); len(left) != 0 {
		t.Errorf("the copies go with the finished ticket: %v", left)
	}
}

// The copies live exactly as long as their ticket: removed when it is canceled or finished, kept for the adoption window of a dead
// owner's placed ticket, swept when nothing owns them, and never swept from under a ticket that is being made.
func TestAGraphTicketsCopiesLiveAndDieWithTheirTicket(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q, _ := manual(t)
	st, _ := storeFor(e.cfg)
	mpath := writeFile(t, t.TempDir(), "m.json", []byte(bodyManifestA))
	lt := admitOn(t, q, e.cfg, e.lr, e.req(krea2Graph, map[string]any{"manifest_path": mpath}))
	id := lt.snapshot().ID
	for what, want := range map[string]string{"graph": krea2Graph, "manifest": bodyManifestA} {
		if b, err := os.ReadFile(st.bodyPath(id, what)); err != nil || string(b) != want {
			t.Fatalf("the ticket keeps its %s as it was admitted: %q %v", what, b, err)
		}
	}

	if !lt.cancelUndispatched("test") {
		t.Fatal("a waiting ticket cancels")
	}
	waitFor(t, 5*time.Second, "the canceled ticket's copies are gone", func() bool { return len(queueFiles(t, e.cfg, "body-")) == 0 })

	old := time.Now().Add(-2 * tmpStale)
	orphanOld, orphanNew := st.bodyPath("mq-0rphan000001", "graph"), st.bodyPath("mq-0rphan000002", "graph")
	for _, p := range []string{orphanOld, orphanNew} {
		if err := os.WriteFile(p, []byte(krea2Graph), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(orphanOld, old, old); err != nil {
		t.Fatal(err)
	}
	// A dead owner's placed ticket is adoptable for deadKeep: its copies stay. A finished ticket needs none.
	placed := ticketFile{ID: "mq-0placed00001", ReqKey: "k", Created: nowMs(), Owner: deadOwner, Beat: nowMs(), State: stPlaced, Node: "node-a", Base: "http://x", Job: "j"}
	done := ticketFile{ID: "mq-0done000001", ReqKey: "k2", Created: nowMs(), Owner: deadOwner, Beat: nowMs(), State: stDone, Collected: true}
	for _, tf := range []ticketFile{placed, done} {
		if err := st.write(tf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st.bodyPath(tf.ID, "graph"), []byte(krea2Graph), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A waiting ticket whose owner died sent nothing anywhere: the sweep removes the ticket and its copies together.
	abandoned := ticketFile{ID: "mq-0abandon0001", ReqKey: "k3", Created: nowMs(), Owner: deadOwner, Beat: nowMs(), State: stWaiting}
	if err := st.write(abandoned); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.bodyPath(abandoned.ID, "graph"), []byte(krea2Graph), 0o644); err != nil {
		t.Fatal(err)
	}
	st.sweep(st.list())
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if _, found := st.read(abandoned.ID); found || exists(st.bodyPath(abandoned.ID, "graph")) {
		t.Error("an abandoned waiting ticket is swept with its copies")
	}
	if exists(orphanOld) {
		t.Error("a copy no ticket owns, left more than a minute ago, is debris and is swept")
	}
	if !exists(orphanNew) {
		t.Error("a copy written a moment ago may belong to a ticket that is being made: it is not swept")
	}
	if !exists(st.bodyPath(placed.ID, "graph")) {
		t.Error("a dead owner's placed ticket is adoptable, so its copy stays")
	}
	if exists(st.bodyPath(done.ID, "graph")) {
		t.Error("a finished ticket needs no copy")
	}
}

// The key of a ticket is made from the bytes the plan holds, so a file edited between the call's first look and the plan cannot give
// a ticket the identity of one graph and the payload of another; and the path key is the one thing that does not move with the file.
func TestAGraphTicketsKeyComesFromTheBytesItHolds(t *testing.T) {
	dir := t.TempDir()
	req := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "a.json", []byte(krea2Graph)), "out_dir": "o"}}
	pl, err := plan(req)
	if err != nil {
		t.Fatal(err)
	}
	before, paths := reqKey(req), pathKey(req)
	writeFile(t, dir, "a.json", []byte(plainGraph))
	if got := payloadKey(req, pl); got != before {
		t.Errorf("the key of the plan's bytes is the key the file had when it was read: %s vs %s", got, before)
	}
	if reqKey(req) == before {
		t.Error("control: the file's content changed, so a key made from the file now is another")
	}
	if pathKey(req) != paths {
		t.Error("the path key does not read the file")
	}
	if err := os.Remove(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	if pathKey(req) != paths {
		t.Error("the path key survives the file")
	}
	other := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": filepath.Join(dir, "b.json"), "out_dir": "o"}}
	if pathKey(other) == paths {
		t.Error("another path is another path key")
	}
}

// A request identical to a ticket that appeared meanwhile (two processes asked at once) is not made twice, and the loser leaves no
// copies of a graph for a ticket that does not exist.
func TestALosingCreateLeavesNoCopies(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q1, q2 := manual(t)
	st, _ := storeFor(e.cfg)
	req := e.req(krea2Graph, nil)
	winner := admitOn(t, q1, e.cfg, e.lr, req)
	lt, existing, err := q2.create(context.Background(), e.cfg, e.lr, req, nil, Wait{}, st, reqKey(req), localID{}, heldLane(), time.Now(), time.Time{})
	if err != nil || lt != nil || existing == nil || existing.ID != winner.snapshot().ID {
		t.Fatalf("the second create finds the first ticket: lt=%v existing=%+v err=%v", lt, existing, err)
	}
	if kept := queueFiles(t, e.cfg, "body-"); len(kept) != 1 || !strings.Contains(kept[0], winner.snapshot().ID) {
		t.Errorf("only the winner's copy remains: %v", kept)
	}
}

// The ticket is keyed by the bytes it holds and not by the key the call computed a moment earlier: a file edited between the call's
// first look and the plan gives the ticket the identity of what it will run. It also records the path key a token collects by.
func TestACreatedGraphTicketIsKeyedByWhatItHolds(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q, _ := manual(t)
	st, _ := storeFor(e.cfg)
	req := e.req(krea2Graph, nil)
	lt, _, err := q.create(context.Background(), e.cfg, e.lr, req, nil, Wait{}, st, "a-key-made-from-an-earlier-reading", localID{}, heldLane(), time.Now(), time.Time{})
	if err != nil || lt == nil {
		t.Fatalf("create: lt=%v err=%v", lt, err)
	}
	if got := lt.snapshot(); got.ReqKey != reqKey(req) || got.PathKey != pathKey(req) {
		t.Errorf("the ticket's key is the key of its bytes (%s) and it records the path key (%s): %+v", reqKey(req), pathKey(req), got)
	}
}

// A token whose ticket was abandoned before it sent anything starts the request over. When the files are what they were, the new
// ticket keeps the old one's arrival (its seniority in every line); when the caller has overwritten them, what is asked for now is a
// different request, and it takes its own place at the back.
func TestATokenOfAnAbandonedGraphTicketInheritsItsArrivalOnlyForTheSameContent(t *testing.T) {
	for name, edited := range map[string]bool{"the same content": false, "the file overwritten": true} {
		t.Run(name, func(t *testing.T) {
			e := newGEnv(t)
			e.node("node-a", true, nodeOpts{}, krea2File)
			e.start()
			q, _ := manual(t)
			st, _ := storeFor(e.cfg)
			req := e.req(krea2Graph, nil)
			long := nowMs() - int64(time.Hour.Milliseconds())
			dead := ticketFile{ID: "mq-0abandon0002", ReqKey: reqKey(req), PathKey: pathKey(req), Created: long, Owner: deadOwner, Beat: long,
				Task: string(core.TaskRunGraph), State: stWaiting}
			if err := st.write(dead); err != nil {
				t.Fatal(err)
			}
			if edited {
				gpath := req.Params["graph_path"].(string)
				writeFile(t, filepath.Dir(gpath), filepath.Base(gpath), []byte(plainGraph))
			}
			again := core.Request{Task: req.Task, Door: req.Door, Resumable: true, Params: map[string]any{"waiter_token": dead.ID}}
			for k, v := range req.Params {
				again.Params[k] = v
			}
			res, lt := q.admit(context.Background(), e.cfg, e.lr, again, nil, Wait{})
			if lt == nil {
				t.Fatalf("the abandoned ticket is replaced by a new one: %+v", res)
			}
			if _, found := st.read(dead.ID); found {
				t.Error("the abandoned ticket is swept")
			}
			created := lt.snapshot().Created
			if inherited := created == long; inherited == edited {
				t.Errorf("arrival inherited=%v, want %v (created %d, the abandoned one %d)", inherited, !edited, created, long)
			}
		})
	}
}
