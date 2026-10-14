package gpuactivity

// What a status call says about the yield request (plan P11, ADR 0085): the holder of an overdue
// lease was asked to yield, ran the job's own on_yield, and still holds the card. The words come
// from gpulease.YieldViewOf, one source for every surface; these tests pin that a status snapshot
// carries them, that a lease nobody asked adds nothing to the wire, and that nothing here writes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func yieldPolicy(t *testing.T, raw string) {
	t.Helper()
	gpulease.SetDefaultOnOverdue(raw)
	t.Cleanup(func() { gpulease.SetDefaultOnOverdue("") })
}

// overdueLease is a live lease whose window ended 30 minutes ago, held by this process.
func overdueLease(t *testing.T, yields string) (root, dir string) {
	t.Helper()
	return leaseFixture(t, func(rec map[string]any) {
		rec["acquired_at_ms"] = time.Now().Add(-3 * time.Hour).UnixMilli()
		rec["expires_at_ms"] = time.Now().Add(-30 * time.Minute).UnixMilli()
		rec["wrapper_version"] = "0.186.0"
		rec["command"] = "python film.py"
		if yields != "" {
			rec["yields"] = yields
		}
	})
}

func plantYield(t *testing.T, dir string, epoch int, askedAgo time.Duration, action string) string {
	t.Helper()
	req := map[string]any{
		"epoch": epoch, "asked_ms": time.Now().Add(-askedAgo).UnixMilli(), "by_pid": os.Getpid(),
		"basis": "overdue by 30m0s", "waiters": []map[string]any{{"pid": 4242, "class": "text", "reason": "kv bench"}}, "policy": "request",
	}
	if action != "" {
		req["action"], req["acted_ms"] = action, time.Now().Add(-askedAgo).UnixMilli()
	}
	b, _ := json.Marshal(req)
	p := filepath.Join(dir, "yield."+strconv.Itoa(epoch))
	if err := os.WriteFile(p, b, 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

// A lease nobody asked and that cannot be (or no policy) adds nothing to the wire: consumers that
// read the holder see the bytes they saw before.
func TestHolderYieldFieldsOmittedWhenThereIsNothingToSay(t *testing.T) {
	yieldPolicy(t, "")
	root, _ := overdueLease(t, "")
	v := snap(root, 5, nil)
	if v.Verdict != VerdictHeldOverdue || v.Holder == nil {
		t.Fatalf("setup: %q (%s)", v.Verdict, v.Note)
	}
	b, err := json.Marshal(v.Holder)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"yield"`, `"yield_file"`, `"yield_note"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("the holder of a lease with no yield state carries %s: %s", key, b)
		}
	}
	if strings.Contains(v.Note, "yield") {
		t.Errorf("the default policy keeps the note of before the key existed: %s", v.Note)
	}
	// A holder that runs the request, under the default policy, adds nothing either: nobody is asked, so the
	// path of a file that will never exist is not advertised.
	root2, _ := overdueLease(t, gpulease.YieldsWrapper)
	h := snap(root2, 5, nil).Holder
	if h == nil || h.YieldFile != "" {
		t.Fatalf("the default policy asks nobody and so advertises no yield file: %+v", h)
	}
	// The control: the same lease under the request policy carries the path.
	yieldPolicy(t, "request")
	if h := snap(root2, 5, nil).Holder; h == nil || h.YieldFile == "" {
		t.Fatalf("a lease whose holder runs the request carries the path of its yield file under the request policy: %+v", h)
	}
}

// The path is a statement that a request may appear there: it is made only while a request can be made
// (the policy is request), or once one was made, whatever the policy says by then.
func TestYieldFileIsOnTheWireOnlyWhenARequestCanBeMadeOrWas(t *testing.T) {
	root, dir := overdueLease(t, gpulease.YieldsWrapper)
	for _, tc := range []struct {
		policy string
		want   bool
	}{{"", false}, {"hold", false}, {"kill", false}, {"request", true}} {
		yieldPolicy(t, tc.policy)
		h := snap(root, 5, nil).Holder
		if h == nil {
			t.Fatal("no holder")
		}
		b, _ := json.Marshal(h)
		if got := h.YieldFile != "" && strings.Contains(string(b), `"yield_file"`); got != tc.want {
			t.Errorf("policy %q: yield_file on the wire = %v, want %v: %s", tc.policy, got, tc.want, b)
		}
	}
	yieldPolicy(t, "")
	plantYield(t, dir, 41, time.Minute, gpulease.YieldActionRan)
	if h := snap(root, 5, nil).Holder; h == nil || h.YieldFile == "" {
		t.Errorf("once a request was made its file is named whatever the policy says now: %+v", h)
	}
}

// An ask is once per epoch and its sidecar stays until the lease ends, so a lease can be asked and then
// renewed. The status of a healthy lease must not call its ask unanswered.
func TestAnAskedLeaseThatWasRenewedIsNotUnanswered(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := leaseFixture(t, func(rec map[string]any) { // a window that has not ended
		rec["wrapper_version"] = "0.186.0"
		rec["yields"] = gpulease.YieldsWrapper
	})
	plantYield(t, dir, 41, 25*time.Minute, gpulease.YieldActionRan)
	v := snap(root, 5, nil)
	y := v.Holder.Yield
	if y == nil || y.Action != gpulease.YieldActionRan {
		t.Fatalf("the sidecar of the epoch stands: %+v", y)
	}
	if y.Unanswered {
		t.Errorf("a lease that is not overdue is not an unanswered ask: %+v", y)
	}
	for _, bad := range []string{"has not ended", "ask whoever owns it", "stop its job by hand"} {
		if strings.Contains(v.Holder.YieldNote, bad) {
			t.Errorf("the sentence about a renewed lease must not say %q: %q", bad, v.Holder.YieldNote)
		}
	}
	if !strings.Contains(v.Holder.YieldNote, "renewed") {
		t.Errorf("the sentence says the lease has since been renewed: %q", v.Holder.YieldNote)
	}
}

func TestHolderCarriesTheYieldFileOfACapableLease(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := overdueLease(t, gpulease.YieldsDetached)
	h := snap(root, 5, nil).Holder
	if h == nil {
		t.Fatal("no holder")
	}
	if want := filepath.Join(dir, "yield.41"); h.YieldFile != want {
		t.Fatalf("yield_file = %q, want %q", h.YieldFile, want)
	}
	if h.Yield != nil {
		t.Errorf("nobody asked: %+v", h.Yield)
	}
	b, _ := json.Marshal(h)
	if !strings.Contains(string(b), `"yield_file"`) {
		t.Errorf("the path is on the wire (gpu status --json): %s", b)
	}
}

func TestOverdueNoteNamesTheAskAndWhatTheHolderDid(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := overdueLease(t, gpulease.YieldsWrapper)
	plantYield(t, dir, 41, 2*time.Minute, gpulease.YieldActionRan)
	v := snap(root, 5, nil)
	if v.Verdict != VerdictHeldOverdue {
		t.Fatalf("verdict %q (%s)", v.Verdict, v.Note)
	}
	for _, want := range []string{"was asked to yield", "ran its on_yield", "still holds the card", "nothing is reclaimed or killed", "gpu takeover --epoch 41"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	y := v.Holder.Yield
	if y == nil || y.Action != gpulease.YieldActionRan || y.Waiters != 1 || y.Unanswered || y.AskedAt == "" {
		t.Fatalf("holder.yield = %+v", y)
	}
	if _, err := time.Parse(time.RFC3339, y.AskedAt); err != nil {
		t.Errorf("asked_at %q is not RFC 3339: %v", y.AskedAt, err)
	}
	if v.Holder.YieldNote == "" || !strings.Contains(v.Note, v.Holder.YieldNote) {
		t.Errorf("the note carries the holder's yield sentence verbatim: note %q, sentence %q", v.Note, v.Holder.YieldNote)
	}
}

func TestAnUnansweredAskIsSaidAndNeverNamesReleaseForAWrapper(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := overdueLease(t, gpulease.YieldsWrapper)
	plantYield(t, dir, 41, 25*time.Minute, gpulease.YieldActionRan)
	v := snap(root, 5, nil)
	y := v.Holder.Yield
	if y == nil || !y.Unanswered {
		t.Fatalf("asked 25m ago with no grace declared (10m display threshold): %+v", y)
	}
	for _, want := range []string{"has not ended", "nothing is killed", "stop its job by hand", "the claim frees when the wrapper ends"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	if strings.Contains(v.Note, "gpu release") {
		t.Errorf("a live wrapper ends its own job within a tick of its claim being released; the note must not send anyone there: %s", v.Note)
	}
}

func TestADetachedHoldersUnansweredAskNamesReleaseWithWhatItDoes(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := overdueLease(t, gpulease.YieldsDetached)
	plantYield(t, dir, 41, 25*time.Minute, gpulease.YieldActionRan)
	v := snap(root, 5, nil)
	for _, want := range []string{"gpu release --epoch 41", "frees the claim at once", "keeps running"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
}

func TestAHolderThatDoesNotRunTheRequestIsSaidToBeUnaskable(t *testing.T) {
	yieldPolicy(t, "request")
	root, _ := overdueLease(t, "") // a record with no yields stamp: an older build, or an in-process holder
	v := snap(root, 5, nil)
	if !strings.Contains(v.Note, "does not run the yield request") || !strings.Contains(v.Note, "media pipeline") || !strings.Contains(v.Note, "ask whoever owns it") {
		t.Fatalf("note: %s", v.Note)
	}
	if strings.Contains(v.Note, "predates") {
		t.Errorf("an in-process holder of this build predates nothing: %s", v.Note)
	}
	if v.Holder.YieldFile != "" || v.Holder.Yield != nil {
		t.Errorf("a holder that cannot be asked has no yield file: %+v", v.Holder)
	}
}

func TestStatusNeverWritesTheYieldSidecar(t *testing.T) {
	yieldPolicy(t, "request")
	root, dir := overdueLease(t, gpulease.YieldsWrapper)
	waiters := filepath.Join(root, "gpu", "waiters")
	if err := os.MkdirAll(waiters, 0o777); err != nil {
		t.Fatal(err)
	}
	// A live waiter on the whole node: a process of its own (the parent of the test binary), because a pid no
	// process has is pruned as dead and the test would then prove nothing, and the holder's own pid is never demand.
	b, _ := json.Marshal(map[string]any{"pid": os.Getppid(), "class": "text", "since_ms": time.Now().UnixMilli()})
	waiter := filepath.Join(waiters, "w.json")
	if err := os.WriteFile(waiter, b, 0o666); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		snap(root, 5, nil)
	}
	if _, err := os.Stat(filepath.Join(dir, "yield.41")); !os.IsNotExist(err) {
		t.Fatalf("a status snapshot wrote the yield request (%v): only the holder's own tick may", err)
	}
	// The control: the demand was there. The holder's own asker, ticking twice on the same lease and the same
	// waiter, asks.
	if _, err := os.Stat(waiter); err != nil {
		t.Fatalf("setup: the waiter record is gone, so the snapshots above saw no demand (%v)", err)
	}
	asker := gpulease.ObserverAt(dir).NewYieldAsker(41)
	for i := 0; i < 2; i++ {
		if _, _, err := asker.Tick(); err != nil {
			t.Fatalf("setup: the holder's tick: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "yield.41")); err != nil {
		t.Fatalf("setup: the same lease and waiter must be asked by the holder's tick, or the snapshots proved nothing (%v)", err)
	}
}

// A tree-orphan has no holder left to ask: its record may still carry the stamp of the wrapper that died, and
// a sidecar that wrapper wrote, and neither is reported as a request waiting for an answer.
func TestATreeOrphanHasNoHolderToAsk(t *testing.T) {
	yieldPolicy(t, "request")
	dir := t.TempDir()
	plantYield(t, dir, 41, 25*time.Minute, gpulease.YieldActionRan)
	info := gpulease.Info{Held: true, Epoch: 41, PID: deadOwnerPID, Class: gpulease.ClassMedia, Epochs: []uint64{41}, Yields: gpulease.YieldsWrapper,
		ExpiresAt: time.Now().Add(-time.Hour), TreeOrphan: true, TreeWhy: "a recorded member of the job's tree is alive"}
	_, head := holdersOf(dir, info, Options{}, time.Now())
	if head == nil || !head.TreeOrphan {
		t.Fatalf("setup: %+v", head)
	}
	if head.YieldFile != "" || head.YieldNote != "" || head.Yield != nil {
		t.Errorf("a tree-orphan reports a request nobody can answer: %+v", head)
	}
}
