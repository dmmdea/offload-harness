package pipeline

// MediaLaneFree (medialane.go): the read-only question "would this image call take its local lane right now?" that a
// cluster router asks before it places the call on another node (P0 plan, S1). It is a RELAXATION of the real
// admission: it says "busy" only when something that makes the real wait-0 grant refuse is present, so it never
// calls a lane busy that acquireMediaLease would have granted. The differential test below drives the REAL
// admission over a table of lane states and holds the prober to that, in the one direction that matters; the rest
// pin what it reads, what it names and that it writes nothing.

import (
	"context"
	"encoding/json"
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

func laneReq(params map[string]any) core.Request {
	return core.Request{Task: core.TaskGenerateImage, Door: "offload_generate_image", Input: "a calm ocean at dawn", Params: params, Resumable: true}
}

// pinA pins the fixture's image route to card A (ComfyUI position 1 is nvidia index 0 under admitOrder).
func pinA(c *config.Config) { c.ComfyCudaDevice = "1" }

func pinAKrea2(c *config.Config) { krea2Binding(c); pinA(c) }

// pinSeveral names two cards as the pin, which turns the image call's plan into the whole-node one on a host that leases
// cards (planMedia: "comfy_cuda_device names several cards: this call holds the whole node"): the plan whose queue order is
// the lease queue's own, by arrival time.
func pinSeveral(c *config.Config) { c.ComfyCudaDevice = "1,2" }

// leaveExpiredToken leaves a place in line for devices (nil = the whole node) whose poller left longer ago than a token
// can be resumed (gpulease.TokenTTL): the file exists, and every reader that prunes finds it expired. The Manager's
// clock is not settable from here, so the place is left and its poll time is written back into the record.
func leaveExpiredToken(t *testing.T, f *admitFixture, devices []string) string {
	t.Helper()
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devices}, time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "gpu", "tokens", tok.ID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec["polled_ms"] = time.Now().Add(-gpulease.TokenTTL - 5*time.Minute).UnixMilli()
	if b, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
	return tok.ID
}

// staleWaiter registers a waiter whose process is alive but that stopped re-stamping its record an hour ago: every
// reader that prunes finds it stale. It is registered once and never refreshed.
func staleWaiter(t *testing.T, f *admitFixture, reason string, devices ...string) {
	t.Helper()
	_, unregister := gpulease.RegisterSeatWaiter(filepath.Join(f.root, "gpu", "lease"), reason, devices)
	t.Cleanup(unregister)
	entries, err := os.ReadDir(filepath.Join(f.root, "gpu", "waiters"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("the waiter left no record: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(f.root, "gpu", "waiters", e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// hold takes a media lease on cards (lease ids) for an hour, released with the test.
func hold(t *testing.T, f *admitFixture, reason string, devices ...string) *gpulease.Lease {
	t.Helper()
	l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: reason, Origin: "test", TTL: time.Hour, Devices: devices})
	if err != nil {
		t.Fatalf("could not hold %v: %v", devices, err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

type laneCase struct {
	name string
	spec admitSpec
	away bool
	// setup makes the state of the lane; nil = idle.
	setup func(t *testing.T, f *admitFixture)
	// params makes the request's parameters, after the state exists (a resumed call carries the waiter_token of a place
	// it left); nil = none.
	params func(t *testing.T, f *admitFixture) map[string]any
	// grants is what the REAL wait-0 admission does in that state. Asserted, so the table cannot rot into a list of
	// states nobody checked.
	grants bool
	// mustSeeBusy: the state is one the prober READS (a lease, a place in line, a slot, the host's memory, the
	// allocator's verdict), so it must call the lane busy, not merely be allowed to.
	mustSeeBusy bool
}

func laneCases() []laneCase {
	std := admitSpec{order: admitOrder}
	pinned := admitSpec{order: admitOrder, mutate: pinA}
	krea2Auto := admitSpec{order: admitOrder, mutate: krea2Binding}
	krea2Pinned := admitSpec{order: admitOrder, mutate: pinAKrea2}
	whole := admitSpec{order: admitOrder, noAudit: true}
	wholeLeased := admitSpec{order: admitOrder, mutate: pinSeveral} // a card-scoped host, the whole-node plan
	A, B, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDB), leaseIDOf(admitUUIDC)
	_ = B
	// ownPlace leaves the call's own place in line, arrived `age` ago, and returns the waiter_token that resumes it.
	ownPlace := func(devices []string, age time.Duration) func(t *testing.T, f *admitFixture) map[string]any {
		return func(t *testing.T, f *admitFixture) map[string]any {
			tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devices}, time.Now().Add(-age))
			if err != nil {
				t.Fatal(err)
			}
			return map[string]any{"waiter_token": tok.ID}
		}
	}
	return []laneCase{
		{name: "idle, auto", spec: std, grants: true},
		{name: "idle, pinned", spec: pinned, grants: true},
		{name: "pinned card held by a media lease", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "a bench render", A) }},
		{name: "pinned card free, another card held", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "a bench render", C) }},
		{name: "auto, every card the operator lets us use is held", spec: std, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", A); hold(t, f, "r2", C) }},
		{name: "auto, one usable card still free", spec: std, grants: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", A) }},
		{name: "auto, only the display card is free and the operator is away", spec: std, away: true, grants: true,
			setup: func(t *testing.T, f *admitFixture) { hold(t, f, "r1", A); hold(t, f, "r2", C) }},
		{name: "whole node, idle", spec: whole, grants: true},
		{name: "whole node, a lease is held", spec: whole, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a whole-node render", TTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		{name: "card-scoped host, a whole-node lease is held", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a whole-node render", TTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		{name: "in-process slot held on the pinned card", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if !mediaSlots.tryTake([]string{A}) {
					t.Fatal("slot busy before the test")
				}
				t.Cleanup(func() { mediaSlots.release([]string{A}) })
			}},
		{name: "in-process slot held on another card", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				if !mediaSlots.tryTake([]string{C}) {
					t.Fatal("slot busy before the test")
				}
				t.Cleanup(func() { mediaSlots.release([]string{C}) })
			}},
		{name: "a live place in line on the pinned card", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a live place in line on another card", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{C}}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a registered waiter on the pinned card", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav", A) }},
		{name: "a registered waiter on another card", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav", C) }},
		{name: "a whole-node waiter ahead", spec: pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") }},
		{name: "a waiter that waits only on host RAM, a call that declares none", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { startHostRAMWaiter(t, f, admitUUIDA) }},
		{name: "a waiter that waits only on host RAM, a call that declares 32.8 GiB", spec: krea2Pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { startHostRAMWaiter(t, f, admitUUIDA) }},
		{name: "a short host, a lane that declares 32.8 GiB (pinned)", spec: krea2Pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "a short host, a lane that declares 32.8 GiB (auto)", spec: krea2Auto, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "a short host, a lane that declares nothing", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "a host that can never admit the lane", spec: krea2Pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory {
					return gpuprobe.HostMemory{PhysicalGiB: 20, AvailableGiB: 15, CommitUsedGiB: 5, CommitLimitGiB: 40}
				})
			}},
		{name: "a short host, whole node", spec: admitSpec{order: admitOrder, noAudit: true, mutate: krea2Binding}, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) { f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) }) }},
		{name: "another lane is still loading the memory it declared, a lane that declares 32.8 GiB", spec: krea2Pinned, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a lane that is loading", Origin: "test", TTL: time.Hour,
					Devices: []string{C}, HostRAMGiB: 32.8})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		{name: "another lane is still loading the memory it declared, a lane that declares nothing", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })
				l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "a lane that is loading", Origin: "test", TTL: time.Hour,
					Devices: []string{C}, HostRAMGiB: 32.8})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Release() })
			}},
		// The whole-node plan queues by ARRIVAL TIME: a call that resumes a place registers with the arrival time it left
		// with, so a caller that joined the line after it is behind it, and only a caller that arrived before it is ahead.
		// (The first cut counted everyone in the directory: a call resuming its place was called busy while the real
		// admission granted it, on the whole-node plan only; found by a randomized differential in review.)
		{name: "whole node: a registered waiter that arrived AFTER the call's own place", spec: wholeLeased, grants: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") },
			params: ownPlace(nil, 5*time.Second)},
		{name: "whole node: a place another caller left AFTER the call's own place", spec: wholeLeased, grants: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			},
			params: ownPlace(nil, 5*time.Second)},
		{name: "whole node, no card leases: a registered waiter that arrived AFTER the call's own place", spec: whole, grants: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") },
			params: ownPlace(nil, 5*time.Second)},
		{name: "whole node: a registered waiter that arrived BEFORE the call's own place", spec: wholeLeased, grants: false, mustSeeBusy: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav") },
			params: ownPlace(nil, 0)},
		{name: "whole node: a place another caller left BEFORE the call's own place", spec: wholeLeased, grants: false, mustSeeBusy: true,
			setup: func(t *testing.T, f *admitFixture) {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen"}, time.Now().Add(-5*time.Second)); err != nil {
					t.Fatal(err)
				}
			},
			params: ownPlace(nil, 0)},
		// The pinned and the allocated plans do not queue by arrival time: every other caller in line claims the card
		// before the call looks (gpualloc.QueuedClaims), a call with a place included, so a later waiter IS in its way.
		{name: "pinned: a registered waiter that arrived after the call's own place is still in its way", spec: pinned, grants: false, mustSeeBusy: true,
			setup:  func(t *testing.T, f *admitFixture) { seatWaiterAt(t, f.root, "transcribe a.wav", A) },
			params: ownPlace([]string{A}, 5*time.Second)},
		{name: "the call's own place in line is not in its way", spec: pinned, grants: true,
			params: func(t *testing.T, f *admitFixture) map[string]any {
				tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, time.Now().Add(-5*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				return map[string]any{"waiter_token": tok.ID}
			}},
		// Records the readers of the line PRUNE (an expired place, a waiter that stopped polling) are absent to the
		// admission, so they are not in the call's way; and the probe, which reads through the same readers, must skip
		// them WITHOUT removing them (TestMediaLaneFreeWritesNothing snapshots the lease root over each of these).
		{name: "whole node: the call resumes its own place and it has expired", spec: wholeLeased, grants: true,
			params: func(t *testing.T, f *admitFixture) map[string]any {
				return map[string]any{"waiter_token": leaveExpiredToken(t, f, nil)}
			}},
		{name: "whole node: a place another caller left has expired", spec: wholeLeased, grants: true,
			setup: func(t *testing.T, f *admitFixture) { leaveExpiredToken(t, f, nil) }},
		{name: "pinned: a place another caller left on the pinned card has expired", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { leaveExpiredToken(t, f, []string{A}) }},
		{name: "pinned: a waiter on the pinned card stopped polling", spec: pinned, grants: true,
			setup: func(t *testing.T, f *admitFixture) { staleWaiter(t, f, "transcribe a.wav", A) }},
		{name: "whole node: a waiter stopped polling", spec: wholeLeased, grants: true,
			setup: func(t *testing.T, f *admitFixture) { staleWaiter(t, f, "transcribe a.wav") }},
	}
}

// THE CONTRACT. Over every state of the table, a lane the REAL wait-0 admission would grant is never called busy.
// The converse is not required (the prober may call free a lane the grant then refuses), except where the state is
// one the prober reads: there it must see it, or it would never place anything.
func TestMediaLaneFreeNeverRefusesAGrant(t *testing.T) {
	for _, c := range laneCases() {
		t.Run(c.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, c.spec)
			f.away = c.away
			if c.setup != nil {
				c.setup(t, f)
			}
			params := map[string]any{}
			if c.params != nil {
				params = c.params(t, f)
			}
			req := laneReq(params)
			v := f.p.MediaLaneFree(context.Background(), req)

			cfg, _, err := f.p.cfg.ResolveImageFamily("")
			if err != nil {
				t.Fatal(err)
			}
			g, gerr := f.p.acquireMediaLease(context.Background(), "image-gen", time.Hour, 0, imageNeed(cfg, paramStr(params, "waiter_token")).resumableBy(req))
			granted := gerr == nil
			if granted {
				g.Release()
			}
			if granted != c.grants {
				t.Fatalf("the table is stale: the real wait-0 admission granted=%v (%v), the table says %v", granted, gerr, c.grants)
			}
			if granted && !v.Free {
				t.Fatalf("THE CONTRACT IS BROKEN: the lane was called busy (%s) and the real admission granted it", v.Why)
			}
			if c.mustSeeBusy && v.Free {
				t.Errorf("the prober missed a state it reads (%s): it called the lane free", c.name)
			}
			if !v.Free && v.Why == "" {
				t.Error("a busy verdict must say what is in the way")
			}
		})
	}
}

// What the prober reads and creates: nothing is written. The lease root (every file's path, size, modification time
// and content) is the same before and after, in an idle lane, behind a lease, with a place in line, a waiter and a
// short host, and with the records the readers of the line prune as they read (a place whose poller left more than
// TokenTTL ago, a waiter that stopped polling): the probe skips them and leaves them where they are.
func TestMediaLaneFreeWritesNothing(t *testing.T) {
	snapshot := func(root string) []string {
		var out []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			inWaiters := strings.Contains(filepath.ToSlash(rel), "waiters/")
			if inWaiters && strings.HasSuffix(p, ".tmp") {
				// A live waiter's refresher (the fixture's own, ten milliseconds apart) rewrites its record by writing a
				// temp file beside it and renaming it over. That temp file is the FIXTURE's write, it exists for an
				// instant, and whether a walk sees it is luck: it made this test fail in 4 runs of 12 on the host it
				// was written on. It is not the probe's, so it is not part of what the probe may change.
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil // renamed away between the listing and the stat: it was a temp file of somebody's refresher
			}
			// The record is read with the retry the lease readers use (gpulease.readWaiterFile): on Windows the refresher's
			// rename can fail a read for a moment, and one snapshot that read nothing is not a change the probe made.
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
				stamp = "" // a live waiter re-stamps its own record every poll; its content is what must not change
			}
			out = append(out, rel+"|"+stamp+"|"+string(b))
			return nil
		})
		sort.Strings(out)
		return out
	}
	for _, c := range laneCases() {
		t.Run(c.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, c.spec)
			f.away = c.away
			if c.setup != nil {
				c.setup(t, f)
			}
			params := map[string]any{}
			if c.params != nil {
				params = c.params(t, f)
			}
			before := snapshot(f.root)
			epochBefore := len(f.m.Leases())
			f.p.MediaLaneFree(context.Background(), laneReq(params))
			after := snapshot(f.root)
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Errorf("the probe changed the lease root:\nbefore %v\nafter  %v", before, after)
			}
			if len(f.m.Leases()) != epochBefore || len(f.started()) != 0 {
				t.Errorf("the probe took a lease or started a runner")
			}
		})
	}
}

// What the verdict names: the holder with its reason and the time its lease declared, the callers in line ahead, and
// the guard's own sentence when the host's memory is what is short.
func TestMediaLaneFreeNamesHolderAndHostRAM(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	l := hold(t, f, "bench render of the spring set", A)
	if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if v.Free {
		t.Fatal("a pinned card held by a lease is not free")
	}
	if len(v.Holders) != 1 {
		t.Fatalf("holders = %+v", v.Holders)
	}
	h := v.Holders[0]
	if h.Epoch != l.Epoch() || h.Class != "media" || h.Reason != "bench render of the spring set" || h.RemainingSec < 3000 || h.RemainingSec > 3600 || len(h.Devices) != 1 || h.Devices[0] != A {
		t.Errorf("holder = %+v, want epoch %d, class media, the reason, about an hour left, card A", h, l.Epoch())
	}
	if v.Ahead != 1 {
		t.Errorf("ahead = %d, want the one place in line", v.Ahead)
	}
	for _, want := range []string{"a media-class lease", "bench render of the spring set", "1 caller"} {
		if !strings.Contains(v.Why, want) {
			t.Errorf("why must say %q: %s", want, v.Why)
		}
	}

	g := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinAKrea2})
	g.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) })
	hv := g.p.MediaLaneFree(context.Background(), laneReq(nil))
	if hv.Free || hv.HostRAM != waitZeroSentence || !strings.Contains(hv.Why, waitZeroSentence) {
		t.Errorf("a short host must carry the guard's own sentence %q: %+v", waitZeroSentence, hv)
	}
	if len(hv.Holders) != 0 || hv.Ahead != 0 {
		t.Errorf("nobody holds the card, nobody is in line: %+v", hv)
	}
}

// The prober answers only what it models: every other shape of call is "free", so the call runs where it always did.
func TestMediaLaneFreeIsFreeForWhatItDoesNotModel(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	hold(t, f, "a bench render", A)
	for name, req := range map[string]core.Request{
		"another task":             {Task: core.TaskGenerateVideo, Input: "a slow pan", Params: map[string]any{}},
		"an unknown family":        laneReq(map[string]any{"family": "no-such-family"}),
		"a request with no prompt": {Task: core.TaskGenerateImage, Params: map[string]any{}},
	} {
		if v := f.p.MediaLaneFree(context.Background(), req); !v.Free {
			t.Errorf("%s: want free (not judged), got busy: %s", name, v.Why)
		}
	}
	if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); v.Free {
		t.Fatal("control: the plain request is busy behind the lease")
	}

	// A call that runs under a lease its parent holds (gpu reserve -- local-offload ...) has no lane of its own to
	// overflow: its card was chosen for it.
	l := hold(t, f, "the parent's reservation", leaseIDOf(admitUUIDC))
	for _, kv := range f.p.mediaGrantEnv(l, []string{leaseIDOf(admitUUIDC)}, "") {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); !v.Free || !strings.Contains(v.Why, "inherit") {
		t.Errorf("under an inherited lease the lane is the parent's: %+v", v)
	}
}

// A card table that cannot be read is "cannot refute": the call goes on locally, where the admission reads (and
// reports) it itself.
func TestMediaLaneFreeIsFreeWhenTheCardTableCannotBeRead(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	hold(t, f, "a bench render", A)
	if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); v.Free {
		t.Fatal("control: busy while the table reads")
	}
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", context.DeadlineExceeded
	}
	if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); !v.Free || !strings.Contains(v.Why, "card table") {
		t.Errorf("an unreadable table must read as free with the reason: %+v", v)
	}
}

// The explanation counts exactly the callers who hold the call back: a waiter that waits only on host RAM does not
// hold its card against a call that declares none (G6), and the call's own place in line is never ahead of it.
func TestMediaLaneFreeCountsOnlyTheCallersThatHoldTheCallBack(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	startHostRAMWaiter(t, f, admitUUIDA)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// The place is dropped before the waiter's cleanup runs: a live token ahead of the waiter would hold it for the
	// length of its wait.
	defer f.m.DropToken(tok.ID)
	count := func(token string, askRAM float64) int {
		r := &laneReading{p: f.p, m: f.m, cards: f.cards, token: token, askRAM: askRAM}
		r.callersAhead([]string{A}, time.Time{})
		return r.ahead
	}
	if got := count("", 0); got != 1 {
		t.Errorf("a call that declares nothing is held back by the token only, not by the host-RAM waiter: %d ahead", got)
	}
	if got := count("", 30); got != 2 {
		t.Errorf("a call that declares host RAM is held back by both: %d ahead", got)
	}
	if got := count(tok.ID, 0); got != 0 {
		t.Errorf("a call's own place in line is never ahead of it: %d ahead", got)
	}
	// On the plan that queues by arrival time only the callers that arrived BEFORE the call's place are ahead of it: the
	// waiter and the token below both arrived after a place left 10 minutes ago, and not before one left a moment from now.
	since := func(d time.Duration) int {
		r := &laneReading{p: f.p, m: f.m, cards: f.cards, token: "", askRAM: 30}
		r.callersAhead([]string{A}, time.Now().Add(d))
		return r.ahead
	}
	if got := since(-10 * time.Minute); got != 0 {
		t.Errorf("callers that arrived after the call's place are behind it: %d ahead", got)
	}
	if got := since(time.Minute); got != 2 {
		t.Errorf("callers that arrived before the call's place are ahead of it: %d ahead", got)
	}
}

// Arrival order decides only on the whole-node plan. A call that resumes its place, with a waiter that joined the line
// AFTER that place: on a plan that names its cards the waiter claims them before the call looks, so it is in the way and the
// verdict counts it; on the whole-node plan, which queues by arrival time, it is behind the call and nothing is in the way.
func TestMediaLaneFreeOrdersByArrivalOnlyOnTheWholeNodePlan(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	for _, tc := range []struct {
		name      string
		spec      admitSpec
		devices   []string
		wantFree  bool
		wantAhead int
	}{
		{"a pin names its cards", admitSpec{order: admitOrder, mutate: pinA}, []string{A}, false, 1},
		{"the whole node", admitSpec{order: admitOrder, mutate: pinSeveral}, nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, tc.spec)
			seatWaiterAt(t, f.root, "transcribe a.wav", tc.devices...)
			tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: tc.devices}, time.Now().Add(-5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"waiter_token": tok.ID}))
			if v.Free != tc.wantFree || v.Ahead != tc.wantAhead {
				t.Errorf("free=%v ahead=%d (%s), want free=%v ahead=%d", v.Free, v.Ahead, v.Why, tc.wantFree, tc.wantAhead)
			}
		})
	}
}

// An in-process job holding the card is named: the lease directory shows nothing for a job that inherits its
// parent's lease, so the sentence is the only place the caller learns what is in the way.
func TestMediaLaneFreeNamesAnInProcessJob(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	if !mediaSlots.tryTake([]string{A}) {
		t.Fatal("slot busy before the test")
	}
	t.Cleanup(func() { mediaSlots.release([]string{A}) })
	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if v.Free || !strings.Contains(v.Why, "another generation job in this process holds card(s) "+A) {
		t.Errorf("verdict = %+v", v)
	}
	if len(v.Holders) != 0 {
		t.Errorf("no lease holds it: %+v", v.Holders)
	}
}
