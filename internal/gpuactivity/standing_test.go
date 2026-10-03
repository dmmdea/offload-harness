package gpuactivity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// leaseFixture writes a whole-node lease record the way a wrapper would have, with the
// times set relative to now, and returns the state root and the lease directory. The
// holder is this test process (alive, heartbeating just now), so the lease is LIVE: every
// standing verdict below is about a lease the reclaim rule leaves alone.
func leaseFixture(t *testing.T, mutate func(rec map[string]any)) (root, leaseDir string) {
	t.Helper()
	root = t.TempDir()
	leaseDir = filepath.Join(root, "gpu", "lease")
	if err := os.MkdirAll(leaseDir, 0o777); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film clips (synthetic)",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-2 * time.Hour).UnixMilli(),
		"expires_at_ms":  now.Add(2 * time.Hour).UnixMilli(),
		"renewed_at_ms":  now.UnixMilli(),
	}
	if mutate != nil {
		mutate(rec)
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	return root, leaseDir
}

func stampOrphan(t *testing.T, leaseDir string, epoch int, ago time.Duration) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"since_ms": time.Now().Add(-ago).UnixMilli(), "by_pid": 1})
	if err := os.WriteFile(filepath.Join(leaseDir, "orphan."+strconv.Itoa(epoch)), b, 0o666); err != nil {
		t.Fatal(err)
	}
}

func busyCards(util int) func(context.Context) ([]GPU, error) {
	return func(context.Context) ([]GPU, error) {
		return []GPU{{Index: 0, UUID: "GPU-aaaa", Name: "RTX 5060 Ti", UtilPct: util, UtilKnown: true, MemTotalMiB: 16311}}, nil
	}
}

func snap(root string, util int, extra func(*Options)) View {
	o := Options{StateDir: root, SampleGPU: true, Sampler: busyCards(util)}
	if extra != nil {
		extra(&o)
	}
	return Snapshot(context.Background(), o)
}

// the owner of a lease that is gone: a pid that no process has, from a session the
// registry knew when the lease was taken.
const deadOwnerPID = 2000000000

func deadOwner() map[string]any {
	return map[string]any{"session": "sess-gone", "pid": deadOwnerPID, "tracked": true}
}

// THE INCIDENT, synthetic: a film render launched by a session that died; its wrapper is
// alive and renewing, the cards are busy, the declared window ended. The old verdict was
// held-working (a green label for twenty hours). Orphaned outranks it, and outranks overdue.
func TestIncidentFixtureIsHeldOrphaned(t *testing.T) {
	root, dir := leaseFixture(t, func(rec map[string]any) {
		rec["owner"] = deadOwner()
		rec["acquired_at_ms"] = time.Now().Add(-20 * time.Hour).UnixMilli()
		rec["expires_at_ms"] = time.Now().Add(-2 * time.Hour).UnixMilli() // overdue too
		rec["command"] = "python film.py --spec film.json"
	})
	stampOrphan(t, dir, 41, 40*time.Minute) // first seen gone 40 minutes ago, past the 15 minute grace
	v := snap(root, 90, nil)
	if v.Verdict != VerdictHeldOrphaned {
		t.Fatalf("verdict %q (%s), want held-orphaned", v.Verdict, v.Note)
	}
	for _, want := range []string{"sess-gone", "gone", "gpu takeover --epoch 41", "nothing is reclaimed or killed"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	h := v.Holder
	if h == nil || !h.Orphaned || h.OwnerState != "gone" || h.OwnerSession != "sess-gone" || h.OrphanedForS < 39*60 || h.OrphanedSince == "" {
		t.Fatalf("holder: %+v", h)
	}
	if !h.Overdue {
		t.Fatalf("the incident lease is also overdue: %+v", h)
	}
}

func TestOrphanedOutranksHeldWorking(t *testing.T) {
	now := time.Now()
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 88, UtilKnown: true, MemTotalMiB: 16000}}
	base := func() *Holder {
		return &Holder{PID: 60480, Alive: true, Class: "media", Epoch: 41, AgeSec: 3600, OwnerState: "gone", OwnerSession: "s", OrphanedForS: 1800}
	}
	h := base()
	h.Orphaned = true
	if v, n := Assess(View{At: now, Held: true, Holder: h, GPUs: busy, Seat: SeatState{Name: "agent-pool"}}); v != VerdictHeldOrphaned {
		t.Fatalf("orphaned must outrank held-working: %s (%s)", v, n)
	}
	// Gone but still inside the grace: not orphaned, the cards are busy: held-working.
	if v, n := Assess(View{At: now, Held: true, Holder: base(), GPUs: busy, Seat: SeatState{Name: "agent-pool"}}); v != VerdictHeldWorking {
		t.Fatalf("an owner gone inside the grace is not orphaned yet: %s (%s)", v, n)
	}
}

// The whole ladder, one rung at a time: each verdict outranks everything below it.
func TestStandingVerdictPrecedence(t *testing.T) {
	now := time.Now()
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 88, UtilKnown: true, MemTotalMiB: 16000}}
	prog := &ProgressState{File: "log.jsonl", State: "stalled", AgeSec: 9000, StallSec: 7200}
	all := Holder{PID: 7, Alive: true, Class: "media", Epoch: 9, TreeOrphan: true, Stalled: true, Orphaned: true, Overdue: true, Progress: prog}
	steps := []struct {
		drop func(h *Holder)
		want string
	}{
		{func(h *Holder) {}, VerdictTreeOrphan},
		{func(h *Holder) { h.TreeOrphan = false }, VerdictHeldStalled},
		{func(h *Holder) { h.Stalled = false; h.Progress = nil }, VerdictHeldOrphaned},
		{func(h *Holder) { h.Orphaned = false }, VerdictHeldOverdue},
		{func(h *Holder) { h.Overdue = false }, VerdictHeldWorking},
	}
	h := all
	for _, s := range steps {
		s.drop(&h)
		hh := h
		if v, n := Assess(View{At: now, Held: true, Holder: &hh, GPUs: busy, Seat: SeatState{Name: "agent-pool"}}); v != s.want {
			t.Fatalf("want %s, got %s (%s)", s.want, v, n)
		}
	}
	// And with the cards quiet the last rung is held-idle.
	quiet := []GPU{{Index: 0, Name: "RTX", UtilPct: 1, UtilKnown: true, MemTotalMiB: 16000}}
	hh := h
	if v, _ := Assess(View{At: now, Held: true, Holder: &hh, GPUs: quiet, Seat: SeatState{Name: "agent-pool"}}); v != VerdictHeldIdle {
		t.Fatalf("got %s", v)
	}
}

// A lease record whose holder is gone is stale-holder whatever its owner did: the next
// acquirer reclaims it, so "orphaned" (a live holder with an absent owner) never applies.
func TestStaleHolderOutranksOrphaned(t *testing.T) {
	now := time.Now()
	v, _ := Assess(View{At: now, Stale: true, Holder: &Holder{PID: 5, Orphaned: true, OwnerState: "gone", Stalled: true, Overdue: true}})
	if v != VerdictStaleHolder {
		t.Fatalf("got %s", v)
	}
	// Through the real reader: a record whose wrapper pid is dead is stale, never orphaned.
	root, dir := leaseFixture(t, func(rec map[string]any) {
		rec["holder"] = map[string]any{"pid": deadOwnerPID, "start_time_ms": 0}
		rec["owner"] = deadOwner()
		rec["expires_at_ms"] = time.Now().Add(-time.Hour).UnixMilli()
	})
	stampOrphan(t, dir, 41, time.Hour)
	if got := snap(root, 0, nil); got.Verdict != VerdictStaleHolder {
		t.Fatalf("a dead wrapper is a stale holder: %s (%s)", got.Verdict, got.Note)
	}
}

// `working` (a request or registered run in flight on the seat) keeps outranking the
// lease-standing verdicts: it is a statement about what is happening right now, and the
// standing it hides is named in its tail so it is not lost.
func TestWorkingStillOutranksLeaseStandingAndNamesIt(t *testing.T) {
	h := &Holder{PID: 7, Alive: true, Class: "text", Epoch: 9, Overdue: true, OverdueBySec: 3 * 3600}
	v, n := Assess(View{At: time.Now(), Held: true, Holder: h, Seat: SeatState{Name: "agent-pool", Inflight: 1, Loaded: true}})
	if v != VerdictWorking || !strings.Contains(n, "past its declared window") {
		t.Fatalf("got %s (%s)", v, n)
	}
}

func TestUnattendedWithoutProgressIsHeldWorkingUtilOnly(t *testing.T) {
	h := &Holder{PID: 7, Alive: true, Class: "media", Epoch: 9, Unattended: true}
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 80, UtilKnown: true, MemTotalMiB: 16000}}
	v, n := Assess(View{At: time.Now(), Held: true, Holder: h, GPUs: busy, Seat: SeatState{Name: "agent-pool"}})
	if v != VerdictHeldWorking {
		t.Fatalf("got %s (%s)", v, n)
	}
	for _, want := range []string{"util only", "no progress contract"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %s", want, n)
		}
	}
}

// One status call: an unattended lease whose progress file stopped moving reads
// held-stalled, with the file's last line, and the lease is not touched.
func TestUnattendedStalledWhenProgressFileDoesNotAdvance(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(prog, old, old); err != nil {
		t.Fatal(err)
	}
	root, dir := leaseFixture(t, func(rec map[string]any) {
		rec["unattended"] = true
		rec["owner"] = deadOwner()
		rec["progress"] = map[string]any{"file": prog, "stall_ms": (2 * time.Hour).Milliseconds()}
		rec["acquired_at_ms"] = time.Now().Add(-5 * time.Hour).UnixMilli()
		rec["expires_at_ms"] = time.Now().Add(15 * time.Hour).UnixMilli()
	})
	stampOrphan(t, dir, 41, 3*time.Hour) // the owner is long gone; "owner gone" is no escape and no verdict for an unattended lease
	v := snap(root, 90, nil)
	if v.Verdict != VerdictHeldStalled {
		t.Fatalf("verdict %q (%s), want held-stalled", v.Verdict, v.Note)
	}
	for _, want := range []string{"clip 4 of 17", "log.jsonl", "2h0m0s", "nothing is reclaimed or killed"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	if v.Holder == nil || !v.Holder.Stalled || v.Holder.Progress == nil || v.Holder.Progress.State != "stalled" || v.Holder.Orphaned {
		t.Fatalf("holder: %+v", v.Holder)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal("a status call must not touch the lease")
	}
	// The same lease with the file freshly written is working, with its progress detail.
	now := time.Now()
	_ = os.Chtimes(prog, now.Add(-time.Minute), now.Add(-time.Minute))
	v = snap(root, 0, nil) // cards quiet: progress alone says the holder is working
	if v.Verdict != VerdictHeldWorking || !strings.Contains(v.Note, "clip 4 of 17") {
		t.Fatalf("an advancing progress file is the holder working: %s (%s)", v.Verdict, v.Note)
	}
}

func TestUnknownOwnerNeverOrphaned(t *testing.T) {
	root, dir := leaseFixture(t, nil) // no owner on the record
	stampOrphan(t, dir, 41, 5*time.Hour)
	v := snap(root, 90, nil)
	if v.Verdict != VerdictHeldWorking || v.Holder.OwnerState != "unknown" || v.Holder.Orphaned {
		t.Fatalf("an unknown owner is never orphaned: %s (%s) %+v", v.Verdict, v.Note, v.Holder)
	}
	if !strings.Contains(v.Note, "util only") {
		t.Fatalf("with only utilisation as evidence the note must say so: %s", v.Note)
	}
}

func TestHeldOverdueIsSurfacedNeverReclaimed(t *testing.T) {
	root, dir := leaseFixture(t, func(rec map[string]any) {
		rec["acquired_at_ms"] = time.Now().Add(-6 * time.Hour).UnixMilli()
		rec["expires_at_ms"] = time.Now().Add(-3 * time.Hour).UnixMilli()
	})
	v := snap(root, 90, nil)
	if v.Verdict != VerdictHeldOverdue {
		t.Fatalf("verdict %q (%s)", v.Verdict, v.Note)
	}
	for _, want := range []string{"past its declared window", "still renewing", "nothing is reclaimed"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	if v.Holder.OverdueBySec < 3*3600-5 {
		t.Fatalf("overdue by %d s", v.Holder.OverdueBySec)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal("an overdue lease must not be released by a status call")
	}
	// A record that declares no window is not overdue against one.
	root2, _ := leaseFixture(t, func(rec map[string]any) { rec["expires_at_ms"] = 0 })
	if v := snap(root2, 90, nil); v.Verdict == VerdictHeldOverdue {
		t.Fatalf("no declared window is never overdue: %s", v.Note)
	}
}

// A legacy lease (no owner, no progress contract) gets two facts as INFORMATION; neither
// is a verdict input, and a marker the lease cannot be tied to proves nothing.
func TestLegacyLeaseShowsActivityFactsNotVerdict(t *testing.T) {
	comfy := t.TempDir()
	out := filepath.Join(comfy, "output", "sub")
	if err := os.MkdirAll(out, 0o777); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	png := filepath.Join(out, "clip_0004.png")
	_ = os.WriteFile(png, []byte("x"), 0o666)
	_ = os.Chtimes(png, now.Add(-4*time.Minute), now.Add(-4*time.Minute))
	logf := filepath.Join(comfy, "offload-comfyui.log")
	_ = os.WriteFile(logf, []byte("x"), 0o666)
	_ = os.Chtimes(logf, now.Add(-2*time.Minute), now.Add(-2*time.Minute))
	marker := func(startedAt time.Time) {
		b, _ := json.Marshal(map[string]any{"startedAt": startedAt.UnixMilli(), "pid": 4242})
		_ = os.WriteFile(filepath.Join(comfy, ".offload-launch.json"), b, 0o666)
	}

	root, _ := leaseFixture(t, nil)    // acquired 2h ago
	marker(now.Add(-90 * time.Minute)) // launched after the lease began: tied to it
	with := snap(root, 90, func(o *Options) { o.ComfyDir = comfy })
	without := snap(root, 90, nil)
	if with.Verdict != without.Verdict || with.Verdict != VerdictHeldWorking {
		t.Fatalf("activity facts are information, never a verdict input: %s vs %s", with.Verdict, without.Verdict)
	}
	joined := strings.Join(with.Holder.Facts, " | ")
	if !strings.Contains(joined, "newest ComfyUI output 4m") || !strings.Contains(joined, "ComfyUI log last written 2m") {
		t.Fatalf("facts: %v", with.Holder.Facts)
	}
	if !strings.Contains(with.Note, "newest ComfyUI output") {
		t.Fatalf("the note carries the facts: %s", with.Note)
	}
	if len(without.Holder.Facts) != 0 {
		t.Fatalf("no ComfyUI directory, no facts: %v", without.Holder.Facts)
	}

	// A marker from before the lease began is a leftover from someone else: no facts.
	marker(now.Add(-5 * time.Hour))
	if got := snap(root, 90, func(o *Options) { o.ComfyDir = comfy }); len(got.Holder.Facts) != 0 {
		t.Fatalf("a marker the lease cannot be tied to proves nothing: %v", got.Holder.Facts)
	}
	// A lease that names an owner is not a legacy lease: it has its own evidence.
	root2, _ := leaseFixture(t, func(rec map[string]any) { rec["owner"] = map[string]any{"pid": os.Getpid()} })
	marker(now.Add(-90 * time.Minute))
	if got := snap(root2, 90, func(o *Options) { o.ComfyDir = comfy }); len(got.Holder.Facts) != 0 {
		t.Fatalf("facts are for legacy leases only: %v", got.Holder.Facts)
	}
}

// An unattended lease is judged by its contract: its owner being gone is never held-orphaned.
func TestUnattendedLeaseIsNeverOrphaned(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	_ = os.WriteFile(prog, []byte("{\"done\":3,\"total\":10}\n"), 0o666)
	root, dir := leaseFixture(t, func(rec map[string]any) {
		rec["unattended"] = true
		rec["owner"] = deadOwner()
		rec["progress"] = map[string]any{"file": prog, "stall_ms": (2 * time.Hour).Milliseconds()}
	})
	stampOrphan(t, dir, 41, 5*time.Hour)
	v := snap(root, 0, nil)
	if v.Verdict != VerdictHeldWorking || v.Holder.Orphaned || !strings.Contains(v.Note, "3 of 10") {
		t.Fatalf("%s (%s) %+v", v.Verdict, v.Note, v.Holder)
	}
}

// The status surfaces say where the standing is: `gpu status --json` and offload_status
// carry the ownership block under activity.holder.
func TestMapCarriesTheOwnershipBlock(t *testing.T) {
	root, dir := leaseFixture(t, func(rec map[string]any) { rec["owner"] = deadOwner() })
	stampOrphan(t, dir, 41, time.Hour)
	m := snap(root, 90, nil).Map()
	h, ok := m["holder"].(*Holder)
	if !ok || h.OwnerState != "gone" || !h.Orphaned || h.OwnerSession != "sess-gone" {
		t.Fatalf("holder in Map: %#v", m["holder"])
	}
}

// A big output tree cannot be scanned whole on a status call. When the scan hits its cap
// the newest file it saw may NOT be the newest file there is, and an old time stated as
// "the newest output" would read a live job as stale. So a capped scan says so and states
// no time.
func TestLegacyFactsAdmitWhenTheOutputScanIsCapped(t *testing.T) {
	old := factsOutputWalkLimit
	factsOutputWalkLimit = 3
	t.Cleanup(func() { factsOutputWalkLimit = old })

	comfy := t.TempDir()
	now := time.Now()
	for i, name := range []string{"a", "b", "c", "d", "e", "f"} {
		dir := filepath.Join(comfy, "output", name)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "x.png")
		_ = os.WriteFile(f, []byte("x"), 0o666)
		// the newest file sorts LAST, past the cap
		_ = os.Chtimes(f, now.Add(-time.Duration(60-i)*time.Minute), now.Add(-time.Duration(60-i)*time.Minute))
	}
	b, _ := json.Marshal(map[string]any{"startedAt": now.Add(-90 * time.Minute).UnixMilli(), "pid": 4242})
	_ = os.WriteFile(filepath.Join(comfy, ".offload-launch.json"), b, 0o666)
	root, _ := leaseFixture(t, nil)
	v := snap(root, 90, func(o *Options) { o.ComfyDir = comfy })
	joined := strings.Join(v.Holder.Facts, " | ")
	if !strings.Contains(joined, "scan capped") {
		t.Fatalf("a capped scan must say so: %v", v.Holder.Facts)
	}
	if strings.Contains(joined, "newest ComfyUI output") && !strings.Contains(joined, "unknown") {
		t.Fatalf("a capped scan must not state a time as the newest output: %v", v.Holder.Facts)
	}
}

// Directories count toward the scan cap: a large output tree is mostly directories, and a
// scan that only counted files would walk them all.
func TestOutputScanCapCountsDirectories(t *testing.T) {
	old := factsOutputWalkLimit
	factsOutputWalkLimit = 4
	t.Cleanup(func() { factsOutputWalkLimit = old })
	out := t.TempDir()
	for _, d := range []string{"a", "b", "c", "d", "e"} { // empty directories
		if err := os.MkdirAll(filepath.Join(out, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(out, "z"), 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(out, "z", "new.png"), []byte("x"), 0o666)
	if _, found, capped := newestOutput(out); !capped || found {
		t.Fatalf("six directories and one file against a cap of 4 entries must report capped and no answer: found=%v capped=%v", found, capped)
	}
	factsOutputWalkLimit = 100
	if _, found, capped := newestOutput(out); capped || !found {
		t.Fatalf("under the cap the scan answers: found=%v capped=%v", found, capped)
	}
}

// A progress file that never appears stays UNKNOWN (never stalled), but the note must not
// keep saying "does not exist yet" for the whole window: past the stall window it says how
// long the file has been missing, that a wrong path is as likely as a slow job, and that the
// verdict is unchanged. A stat failure that is not "not found" is named as what it is.
func TestMissingProgressFileIsNamedInTheNotePastItsWindow(t *testing.T) {
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 80, UtilKnown: true, MemTotalMiB: 16000}}
	assess := func(p *ProgressState) (string, string) {
		h := &Holder{PID: 7, Alive: true, Class: "media", Epoch: 9, Unattended: true, Progress: p}
		return Assess(View{At: time.Now(), Held: true, Holder: h, GPUs: busy, Seat: SeatState{Name: "agent-pool"}})
	}
	// Inside the window: the job may simply not have written yet.
	v, n := assess(&ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 600, StallSec: 7200})
	if v != VerdictHeldWorking || !strings.Contains(n, "does not exist yet") {
		t.Fatalf("inside its window: %s (%s)", v, n)
	}
	// Past the window: still held-working (unknown is never stalled), and the note says so.
	v, n = assess(&ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 5 * 3600, StallSec: 3600})
	if v != VerdictHeldWorking {
		t.Fatalf("a missing file never changes the verdict: %s (%s)", v, n)
	}
	for _, want := range []string{"log.jsonl", "has not appeared in 5h0m0s", "1h0m0s", "path is wrong"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %s", want, n)
		}
	}
	if strings.Contains(n, "does not exist yet") {
		t.Errorf("past the window the file is not 'not written yet': %s", n)
	}
	// Not "not found": the stat failure is named.
	_, n = assess(&ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "cannot be read: Access is denied", AgeSec: 600, StallSec: 7200})
	if !strings.Contains(n, "cannot be read") || !strings.Contains(n, "Access is denied") || strings.Contains(n, "does not exist") {
		t.Fatalf("a permission failure is not 'does not exist': %s", n)
	}
}

// The reading reaches the Holder JSON: why the file is unknown, and how long the lease has
// been waiting for it.
func TestHolderCarriesTheProgressProblem(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never.jsonl")
	root, _ := leaseFixture(t, func(rec map[string]any) {
		rec["unattended"] = true
		rec["progress"] = map[string]any{"file": missing, "stall_ms": time.Hour.Milliseconds()}
	})
	v := snap(root, 90, nil)
	h := v.Holder
	if h == nil || h.Progress == nil || h.Progress.State != "unknown" || h.Progress.Problem != "does not exist" {
		t.Fatalf("holder progress: %+v", h)
	}
	if h.Progress.AgeSec < 2*3600-5 || h.Progress.AgeSec > 2*3600+60 {
		t.Fatalf("the age of a missing file is the lease's age (acquired 2h ago): %d s", h.Progress.AgeSec)
	}
	if v.Verdict != VerdictHeldWorking || !strings.Contains(v.Note, "has not appeared in") {
		t.Fatalf("verdict %s: %s", v.Verdict, v.Note)
	}
}

// `working` is the right word for what the SEAT is doing, but it must not bury what is
// wrong with the lease: the note leads with the standing, ahead of the work description, so
// a reader (and a clip to a fixed width) reaches it first.
func TestWorkingNoteLeadsWithTheLeaseStanding(t *testing.T) {
	h := &Holder{PID: 77, Alive: true, Class: "media", Epoch: 41, Stalled: true, Orphaned: true, Overdue: true,
		OwnerState: "gone", OwnerSession: "sess-gone", OrphanedForS: 2400, OrphanGraceS: 900, OverdueBySec: 7200,
		Progress: &ProgressState{File: "/w/log.jsonl", State: "stalled", AgeSec: 3 * 3600, StallSec: 7200}}
	long := strings.Repeat("refactor the ledger reader and rewrite every caller ", 6)
	runs := []Run{
		{Kind: "agent_run", PID: 11, Seat: "agent-pool", Phase: "running", Step: 3, MaxSteps: 12, Goal: long, StartedAtMs: time.Now().Add(-10 * time.Minute).UnixMilli()},
		{Kind: "contract", PID: 12, Seat: "agent-pool", Phase: "running", Goal: long, StartedAtMs: time.Now().Add(-10 * time.Minute).UnixMilli()},
	}
	v, n := Assess(View{At: time.Now(), Held: true, Holder: h, Runs: runs, Seat: SeatState{Name: "agent-pool", Inflight: 2, Loaded: true}})
	if v != VerdictWorking {
		t.Fatalf("work in flight is still `working`: %s", v)
	}
	want := []string{"no activity for 3h0m0s", "gone for 40m0s", "past its declared window by 2h0m0s"}
	pos := -1
	for _, w := range want {
		i := strings.Index(n, w)
		if i < 0 {
			t.Fatalf("note lacks %q: %s", w, n)
		}
		pos = max(pos, i)
	}
	if workAt := strings.Index(n, "refactor the ledger"); workAt < 0 || workAt < pos {
		t.Fatalf("the lease's standing must come BEFORE the work description (standing ends at %d, work at %d): %s", pos, workAt, n)
	}
	if pos > 400 {
		t.Fatalf("the standing must sit near the front of the note (ends at %d): %s", pos, n)
	}
}

// The standing word a holder carries, in the order the verdicts outrank each other.
func TestHolderStandingWordFollowsThePrecedence(t *testing.T) {
	for _, tc := range []struct {
		h    *Holder
		want string
	}{
		{nil, ""},
		{&Holder{}, ""},
		{&Holder{Overdue: true}, VerdictHeldOverdue},
		{&Holder{Overdue: true, Orphaned: true}, VerdictHeldOrphaned},
		{&Holder{Overdue: true, Orphaned: true, Stalled: true}, VerdictHeldStalled},
		{&Holder{Overdue: true, Orphaned: true, Stalled: true, TreeOrphan: true}, VerdictTreeOrphan},
	} {
		if got := tc.h.StandingWord(); got != tc.want {
			t.Errorf("%+v: StandingWord = %q want %q", tc.h, got, tc.want)
		}
	}
}

// Two live leases: the verdict is about the MOST ESCALATED one even when it is not the
// lowest epoch, and the Holder is that lease's own record.
func TestMostEscalatedLeaseIsTheHeadlineAndKeepsItsOwnRecord(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	healthy, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "healthy render", Devices: []string{"GPU-TEST-0"}, TTL: time.Hour,
		Owner: gpulease.Owner{Session: "sess-healthy", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = healthy.Release() }()
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(prog, old, old)
	stalled, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", Devices: []string{"GPU-TEST-1"}, TTL: time.Hour,
		Unattended: true, ProgressFile: prog, Stall: 10 * time.Millisecond, Owner: gpulease.Owner{Session: "sess-film", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Release() }()
	time.Sleep(60 * time.Millisecond)
	if stalled.Epoch() <= healthy.Epoch() {
		t.Fatalf("the stalled lease must be the HIGHER epoch for this test (healthy %d, stalled %d)", healthy.Epoch(), stalled.Epoch())
	}

	v := snap(root, 90, nil)
	if v.Verdict != VerdictHeldStalled {
		t.Fatalf("the verdict is about the most escalated lease: %s (%s)", v.Verdict, v.Note)
	}
	if v.Holder == nil || v.Holder.Epoch != stalled.Epoch() || v.Holder.OwnerSession != "sess-film" || v.Holder.Progress == nil {
		t.Fatalf("the Holder must be the stalled lease's own record, not the lowest epoch's: %+v", v.Holder)
	}
	if len(v.Leases) != 2 {
		t.Fatalf("both live leases are listed: %+v", v.Leases)
	}
}

// An owner reading that cannot be trusted from here says so in the note: the registry could
// not be read, or the moment the owner went could not be recorded. Otherwise a reader sees
// "held-working" or "gone for 0s" and believes the lease has an owner it can see.
func TestNoteSaysWhenTheOwnerCannotBeTrustedFromHere(t *testing.T) {
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 80, UtilKnown: true, MemTotalMiB: 16000}}
	assess := func(h *Holder) string {
		h.PID, h.Alive, h.Class, h.Epoch = 7, true, "media", 9
		_, n := Assess(View{At: time.Now(), Held: true, Holder: h, GPUs: busy, Seat: SeatState{Name: "agent-pool"}})
		return n
	}
	n := assess(&Holder{OwnerState: "gone", OwnerSession: "s", OrphanedForS: 0, OrphanMarkErr: "orphan marker could not be recorded: the lease directory is not writable from here: read-only file system"})
	for _, want := range []string{"orphan marker could not be recorded", "read-only file system", "cannot be tracked from here"} {
		if !strings.Contains(n, want) {
			t.Errorf("held-working note lacks %q: %s", want, n)
		}
	}
	n = assess(&Holder{OwnerState: "unknown", OwnerSession: "s", OwnerNote: "the session registry could not be read (open owners: Access is denied), so whether the owner is still there cannot be told"})
	if !strings.Contains(n, "registry could not be read") {
		t.Errorf("a registry failure must reach the note: %s", n)
	}
	// An owner that was simply never tracked is not repeated in every note.
	n = assess(&Holder{OwnerState: "unknown", OwnerSession: "s", OwnerNote: "the session was not in the session registry when the lease was taken, so whether it is still there cannot be told"})
	if strings.Contains(n, "session registry") {
		t.Errorf("the ordinary untracked case stays in `gpu status`, not in every note: %s", n)
	}
}
