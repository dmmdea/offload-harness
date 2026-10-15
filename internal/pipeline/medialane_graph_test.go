package pipeline

// MediaLaneFree for a whole-node run_graph (P0 plan, S4). A graph the operator did not pin to cards holds the WHOLE node, so
// the question is the whole-node one: any lease, in-process slot or place in line is in its way, and so is a host that
// cannot take what the call declares (the weight files the graph's own loaders name, by the family estimate's fit rule, or the
// media class default, the largest family the box binds, when the graph cannot be read or sized: graphRAM). The contract is the one the image probe has, held by the same kind of differential
// test against the REAL wait-0 admission: a lane the grant would serve is never called busy.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func graphLaneReq(params map[string]any) core.Request {
	p := map[string]any{"graph_path": "g.json"}
	for k, v := range params {
		p[k] = v
	}
	return core.Request{Task: core.TaskRunGraph, Door: "offload_run_graph", Params: p, Resumable: true}
}

// graphGrant is what the real admission does for that graph request at wait 0: the oracle.
func graphGrant(f *admitFixture, req core.Request) (bool, error) {
	need := declaredNeed(declaredDevices(req.Params), paramStr(req.Params, "waiter_token")).declaresRAM(graphRAM(f.p.cfg, req.Params)).resumableBy(req)
	g, err := f.p.acquireMediaLease(context.Background(), "run-graph", time.Hour, 0, need)
	if err == nil {
		g.Release()
	}
	return err == nil, err
}

func graphLaneCases() []laneCase {
	std := admitSpec{order: admitOrder}
	legacy := admitSpec{order: admitOrder, noAudit: true}
	krea2 := admitSpec{order: admitOrder, mutate: krea2Binding}
	krea2Legacy := admitSpec{order: admitOrder, noAudit: true, mutate: krea2Binding}
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	ownPlace := func(age time.Duration) func(t *testing.T, f *admitFixture) map[string]any {
		return func(t *testing.T, f *admitFixture) map[string]any {
			tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "run-graph"}, time.Now().Add(-age))
			if err != nil {
				t.Fatal(err)
			}
			return map[string]any{"waiter_token": tok.ID}
		}
	}
	holdWhole := func(t *testing.T, f *admitFixture) {
		l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a whole-node render", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Release() })
	}
	return []laneCase{
		{name: "card-scoped host, idle", spec: std, grants: true},
		{name: "legacy host, idle", spec: legacy, grants: true},
		{name: "card-scoped host, a card is held by a media lease", spec: std, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "a bench render", A) }},
		{name: "card-scoped host, a whole-node lease is held", spec: std, grants: false, mustSeeBusy: true, setup: holdWhole},
		{name: "legacy host, a whole-node lease is held", spec: legacy, grants: false, mustSeeBusy: true, setup: holdWhole},
		{name: "an in-process slot is held on a card", spec: std, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if !mediaSlots.tryTake([]string{C}) {
					t.Fatal("slot busy before the test")
				}
				t.Cleanup(func() { mediaSlots.release([]string{C}) })
			}},
		{name: "a live place in line on a card", spec: std, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a registered waiter on a card", spec: std, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav", A) }},
		{name: "a short host, a graph that declares the class default (card-scoped)", spec: krea2, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "a short host, a graph that declares the class default (legacy)", spec: krea2Legacy, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "a short host that can never admit the class default", spec: krea2, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory {
					return gpuprobe.HostMemory{PhysicalGiB: 20, AvailableGiB: 15, CommitUsedGiB: 5, CommitLimitGiB: 40}
				})
			}},
		{name: "a roomy host, the class default", spec: krea2, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory {
					return gpuprobe.HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 20, CommitLimitGiB: 200}
				})
			}},
		// The graph's own files are what the lease declares (graphRAM): on a host with room for the 23.09 GiB the incident's graph
		// loads and not for the 32.8 GiB class default, the graph is served; a graph the box cannot size keeps the class default.
		{name: "a host with room for the graph's own files and not for the class default", spec: krea2, grants: true,
			setup:  func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(65) }) },
			params: func(t *testing.T, f *admitFixture) map[string]any { return graphOnTheBox(t, f.p, "qwen_image_fp8.safetensors") }},
		{name: "the same host, a graph that names a file the box does not hold (the class default)", spec: krea2, grants: false, mustSeeBusy: true,
			setup:  func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(65) }) },
			params: func(t *testing.T, f *admitFixture) map[string]any { return graphOnTheBox(t, f.p, "not_on_this_box.safetensors") }},
		{name: "another lane is still loading the memory it declared", spec: krea2, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a lane that is loading", Origin: "test", TTL: time.Hour,
					Devices: []string{C}, HostRAMGiB: 32.8})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		// A call that resumes its place in line is ordered by arrival on the whole-node plan: a caller that joined after it
		// is behind it, one that joined before is ahead.
		{name: "the graph resumes its own place and a waiter arrived AFTER it", spec: std, grants: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") },
			params: ownPlace(5 * time.Second)},
		{name: "the graph resumes its own place and a waiter arrived BEFORE it", spec: std, grants: false, mustSeeBusy: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") },
			params: ownPlace(0)},
		{name: "the graph resumes its own place and it has expired", spec: std, grants: true,
			params: func(t *testing.T, f *admitFixture) map[string]any {
				return map[string]any{"waiter_token": leaveExpiredToken(t, f, nil)}
			}},
		{name: "a waiter that stopped polling", spec: std, grants: true,
			setup: func(t *testing.T, f *admitFixture) { staleWaiter(t, f, "transcribe a.wav") }},
		{name: "a place another caller left has expired", spec: std, grants: true,
			setup: func(t *testing.T, f *admitFixture) { leaveExpiredToken(t, f, nil) }},
	}
}

// nextMillisecond waits until the wall clock has left the millisecond it is in. The lease queue orders the places in a line by the
// millisecond each was left (Token.SinceMs, Waiter.SinceMs) and does not count a tie as "ahead" (tokens.go tokenBlocks and
// gpualloc.QueuedClaimsBefore count only a place strictly earlier; waiters.go: a tie breaks on the waiter's file name), so two
// events stamped in one millisecond are not in the order a fixture meant. A case that leaves a place with time.Now() and lets the
// call arrive straight after it tied on a host fast enough to do both inside one millisecond (a Linux disk: 36 of 50 runs of
// "a live place in line on a card" failed on one box, "the table is stale: the real wait-0 admission granted=true"), and the real
// admission, which is right not to wait on a tie, granted a call the table says is behind a place. The code is as designed; this
// makes the order the table states a strict one by leaving the next millisecond between the step that leaves a place and the
// step that arrives.
func nextMillisecond() {
	start := time.Now().UnixMilli()
	for time.Now().UnixMilli() == start {
		time.Sleep(100 * time.Microsecond)
	}
}

// THE CONTRACT, for a whole-node graph. Over every state of the table a lane the real wait-0 admission would grant is never
// called busy, and a state the prober reads is called busy.
func TestMediaLaneFreeGraphNeverRefusesAGrant(t *testing.T) {
	for _, c := range graphLaneCases() {
		t.Run(c.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, c.spec)
			if c.setup != nil {
				c.setup(t, f)
			}
			nextMillisecond() // the places the setup left are strictly earlier than the place the params leave and than the call
			params := map[string]any{}
			if c.params != nil {
				params = c.params(t, f)
			}
			nextMillisecond() // ...and than the arrival of the call, which is stamped inside the probe and the admission below
			req := graphLaneReq(params)
			v := f.p.MediaLaneFree(context.Background(), req)
			granted, gerr := graphGrant(f, req)
			if granted != c.grants {
				t.Fatalf("the table is stale: the real wait-0 admission granted=%v (%v), the table says %v", granted, gerr, c.grants)
			}
			if granted && !v.Free {
				t.Fatalf("THE CONTRACT IS BROKEN: the lane was called busy (%s) and the real admission granted it", v.Why)
			}
			if c.mustSeeBusy && v.Free {
				t.Errorf("the prober missed a state it reads (%s): it called the lane free (%s)", c.name, v.Why)
			}
			if !v.Free && v.Why == "" {
				t.Error("a busy verdict must say what is in the way")
			}
		})
	}
}

func leaseRootSnapshot(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		inWaiters := strings.Contains(filepath.ToSlash(rel), "waiters/")
		if inWaiters && strings.HasSuffix(p, ".tmp") {
			return nil // a live waiter's refresher writes a temp file beside its record: the fixture's write, not the probe's
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		var b []byte
		for attempt := 0; attempt < 20; attempt++ {
			var rerr error
			if b, rerr = os.ReadFile(p); rerr == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		stamp := info.ModTime().UTC().Format(time.RFC3339Nano)
		if inWaiters {
			stamp = ""
		}
		out = append(out, rel+"|"+stamp+"|"+string(b))
		return nil
	})
	sort.Strings(out)
	return out
}

// The graph probe creates and removes nothing, in every state of the table (the pruned records included).
func TestMediaLaneFreeGraphWritesNothing(t *testing.T) {
	for _, c := range graphLaneCases() {
		t.Run(c.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, c.spec)
			if c.setup != nil {
				c.setup(t, f)
			}
			params := map[string]any{}
			if c.params != nil {
				params = c.params(t, f)
			}
			before := leaseRootSnapshot(f.root)
			leases := len(f.m.Leases())
			f.p.MediaLaneFree(context.Background(), graphLaneReq(params))
			after := leaseRootSnapshot(f.root)
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Errorf("the probe changed the lease root:\nbefore %v\nafter  %v", before, after)
			}
			if len(f.m.Leases()) != leases || len(f.started()) != 0 {
				t.Errorf("the probe took a lease or started a runner")
			}
		})
	}
}

// What the verdict names for a graph: the holder with its reason, and the guard's own sentence when the host is what is short.
func TestMediaLaneFreeGraphNamesHolderAndHostRAM(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	l := hold(t, f, "bench render of the spring set", leaseIDOf(admitUUIDA))
	v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil))
	if v.Free || len(v.Holders) != 1 || v.Holders[0].Epoch != l.Epoch() || v.Holders[0].Reason != "bench render of the spring set" {
		t.Fatalf("a graph holds the whole node, so a lease on any card is in its way: %+v", v)
	}
	for _, want := range []string{"the whole node", "a media-class lease", "bench render of the spring set"} {
		if !strings.Contains(v.Why, want) {
			t.Errorf("why must say %q: %s", want, v.Why)
		}
	}

	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	g.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) })
	hv := g.p.MediaLaneFree(context.Background(), graphLaneReq(nil))
	if hv.Free || !strings.HasPrefix(hv.HostRAM, "waiting for host RAM") || !strings.Contains(hv.Why, hv.HostRAM) {
		t.Fatalf("a short host must carry the guard's own sentence: %+v", hv)
	}
	// It is the sentence the real admission refuses with, word for word: the class default the graph declares, not an estimate of its own.
	if granted, gerr := graphGrant(g, graphLaneReq(nil)); granted || gerr == nil || !strings.Contains(gerr.Error(), hv.HostRAM) {
		t.Errorf("the admission must refuse with the probe's sentence %q, got granted=%v err=%v", hv.HostRAM, granted, gerr)
	}
}

// The prober answers only what it models. A graph with the operator's devices is a card-scoped graph the delegator never
// places on another node, an inherited lease is the parent's lane, and a card table that does not read is "cannot refute".
func TestMediaLaneFreeGraphIsFreeForWhatItDoesNotModel(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	hold(t, f, "a bench render", leaseIDOf(admitUUIDA))
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); v.Free {
		t.Fatal("control: the plain graph is busy behind the lease")
	}
	for name, params := range map[string]map[string]any{
		"a declared device list":  {"devices": []string{"0"}},
		"declared devices (any)":  {"devices": []any{"GPU-aaaa"}},
		"declared devices (text)": {"devices": "0,2"},
	} {
		if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(params)); !v.Free || !strings.Contains(v.Why, "devices") {
			t.Errorf("%s: want free (not judged) naming the devices, got %+v", name, v)
		} else if v.Unjudged {
			t.Errorf("%s: a card-scoped graph is not in question for the lane (it stays here), so it is not an UNREAD lane: %+v", name, v)
		}
	}

	l := hold(t, f, "the parent's reservation", leaseIDOf(admitUUIDC))
	for _, kv := range f.p.mediaGrantEnv(l, []string{leaseIDOf(admitUUIDC)}, "") {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); !v.Free || !strings.Contains(v.Why, "inherit") || v.Unjudged {
		t.Errorf("under an inherited lease the lane is the parent's, and not in question: %+v", v)
	}
}

// A card table that does not read is a lane that could not be READ, which a router must not take for a free lane (the card table
// is slowest to read exactly when a heavy render holds the card): free, with the reason, and Unjudged, as for an image. Without the
// flag a busy graph with an unreadable lane skipped the ticket queue and kept the old pipeline wait and its tk- token.
func TestMediaLaneFreeGraphIsFreeWhenTheCardTableCannotBeRead(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	hold(t, f, "a bench render", leaseIDOf(admitUUIDA))
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); v.Free || v.Unjudged {
		t.Fatalf("control: busy while the table reads, and the lane is judged: %+v", v)
	}
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", context.DeadlineExceeded
	}
	v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil))
	if !v.Free || !strings.Contains(v.Why, "card table") {
		t.Errorf("an unreadable table must read as free with the reason: %+v", v)
	} else if !v.Unjudged {
		// core.LaneVerdict.Unjudged: the card table is slowest to read exactly when a heavy render holds the card, so a lane that
		// could not be read must not be taken for a free one, a graph's lane no more than an image's.
		t.Errorf("an unreadable table is a lane that could not be read, which a router must not take for a free lane: %+v", v)
	}

	// A table that lists no card is as unread as one that failed.
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return nil, "", nil }
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); !v.Free || !v.Unjudged || !strings.Contains(v.Why, "card table") {
		t.Errorf("a table that lists no card is an unread lane: %+v", v)
	}
}

// A lease directory that cannot be opened is a lane that could not be read as well (the same flag as an unreadable card table).
func TestMediaLaneFreeGraphIsUnjudgedWhenTheLeaseDirectoryCannotBeOpened(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); !v.Free || v.Unjudged {
		t.Fatalf("control: the lane reads and nothing holds it: %+v", v)
	}
	notADir := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.p.cfg.GPULockPath = filepath.Join(notADir, "gpu.lock") // a lease root under a file: it cannot be made
	v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil))
	if !v.Free || !v.Unjudged || !strings.Contains(v.Why, "lease directory") {
		t.Errorf("an unopenable lease directory is an unread lane: %+v", v)
	}
}
