package mediaremote

// Third review round for the media-job work (register CT-50): each test here fails without the guard it names.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// ---- S1/S2: a real node, a held render, a spent budget: no withdraw is attempted ---------------------

func TestABudgetExpiryAgainstARealNodeSendsNoDeleteAndSaysTheJobCannotBeRecalled(t *testing.T) {
	withBudget(t, taskRunGraph, time.Second)
	hold := make(chan struct{})
	n := startNode(t, nodeOpts{hold: hold, cfg: func(c *config.Config) { c.FleetMaxConcurrentJobs = 1 }})
	t.Cleanup(func() { close(hold) }) // runs before the node's drain, which would otherwise wait on the render
	cfg := clientCfg(t, n)
	start := time.Now()
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("a budget defer must return at once, took %v", time.Since(start))
	}
	var posted bool
	for _, r := range n.requests() {
		if r.method == http.MethodDelete {
			t.Errorf("a media job cannot be withdrawn: the client must not send %s %s", r.method, r.path)
		}
		if r.method == http.MethodPost {
			posted = true
		}
	}
	if !posted {
		t.Fatal("test premise: the job must have reached the node")
	}
	for _, want := range []string{"remote job media-", "may still be running the job", "cannot be recalled", "ADR 0064"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the defer must say %q: %s", want, res.Reason)
		}
	}
}

// ---- S5: the sweep's threshold outlasts the longest call, and a running fetch keeps its files fresh ---

func TestTheSweepThresholdIsTheLongestBudgetPlusAnHour(t *testing.T) {
	var longest time.Duration
	for _, b := range Budgets {
		longest = max(longest, b)
	}
	if got := staleAfter(); got != longest+time.Hour || got <= 6*time.Hour {
		t.Fatalf("staleAfter %v: want the longest budget (%v) plus 1h, and more than any default call", got, longest)
	}
	withBudget(t, taskVideo, 9*time.Hour)
	if got := staleAfter(); got != 10*time.Hour {
		t.Fatalf("staleAfter %v must follow the budgets: want 10h", got)
	}
}

func TestSweepStaleKeepsAFileInsideTheThresholdAndTakesOneBeyondIt(t *testing.T) {
	dir := t.TempDir()
	const job = "media-0123456789abcdef-"
	writeFile(t, dir, job+"inside.png", nil)
	writeFile(t, dir, job+"beyond.png", nil)
	age := func(name string, d time.Duration) {
		t.Helper()
		when := time.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
	age(job+"inside.png", staleAfter()-time.Minute) // past the old 1 h constant, inside the threshold
	age(job+"beyond.png", staleAfter()+time.Minute)
	sweepStale(dir, time.Now())
	got := dirNames(t, dir)
	if len(got) != 1 || got[0] != job+"inside.png" {
		t.Fatalf("want only the claim inside the threshold left, got %v", got)
	}
}

func TestTouchRefreshesTheClaimsAndFinishedTempsACallHolds(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * time.Hour)
	mk := func(name string) string {
		p := writeFile(t, dir, name, []byte("x"))
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	claim, tmp, other := mk("media-0123456789abcdef-c.png"), mk(".media-fetch-aa.part"), mk("not-ours.png")
	touch([]*staged{{dst: claim, reserved: true, tmp: tmp}, {dst: other}}) // other: neither claimed nor staged
	fresh := func(p string) bool {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(fi.ModTime()) < time.Minute
	}
	if !fresh(claim) || !fresh(tmp) {
		t.Error("a claim and a finished temp the call holds must be touched")
	}
	if fresh(other) {
		t.Error("a file the call does not hold must be left alone")
	}
}

// During a multi-output fetch, the claims are refreshed after every download, so another call's sweep never
// sees them age while this one is working.
func TestAMultiOutputFetchKeepsItsClaimsFresh(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	old := time.Now().Add(-10 * time.Hour)
	var calls int
	var claimAges []time.Duration
	n.tamper = func(name string, b []byte) []byte {
		calls++
		entries, _ := os.ReadDir(cfg.MediaDir)
		for _, e := range entries {
			if e.Name() == name || strings.HasPrefix(e.Name(), ".media-fetch-") {
				continue // a claim is every file the call created empty here
			}
			p := filepath.Join(cfg.MediaDir, e.Name())
			if calls == 1 {
				_ = os.Chtimes(p, old, old) // pretend the first download took very long
			} else if fi, err := e.Info(); err == nil {
				claimAges = append(claimAges, time.Since(fi.ModTime()))
			}
		}
		return b
	}
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res.OK || calls != 2 {
		t.Fatalf("calls %d: %+v", calls, res)
	}
	if len(claimAges) != 2 {
		t.Fatalf("want both claims seen during the second download, got %d", len(claimAges))
	}
	for _, a := range claimAges {
		if a > time.Minute {
			t.Errorf("a claim aged %v during the fetch: it was not touched after the first download", a)
		}
	}
}
