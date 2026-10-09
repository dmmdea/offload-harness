package mediaremote

// Second review round for the media-job work (register CT-50): each test here fails without the guard it names.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// ---- R3: an empty media_dir is the current directory ------------------------------------------------

func TestAnEmptyMediaDirIsTheCurrentDirectory(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	cfg.MediaDir = ""
	here := t.TempDir()
	t.Chdir(here)
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res.OK {
		t.Fatalf("a render finished and an empty media_dir lost it: %+v", res)
	}
	// The primary keeps the node's name; the secondary takes the job id as a prefix (media_dir rule).
	if got := dirNames(t, here); len(got) != 2 || got[0] != "graph-a.png" || !strings.HasSuffix(got[1], "-graph-b.mp4") {
		t.Fatalf("the current directory holds %v, want both outputs", got)
	}
	noTempLeft(t, here)
}

// ---- R4: the explicit out is reserved before any other output claims a name -------------------------

func TestAnOutThatSharesANameWithASecondaryNeverDestroysIt(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	dir := t.TempDir()
	// The primary (graph-a.png, "GA") is sent to D/graph-b.mp4: the very name the node gave the secondary.
	out := filepath.Join(dir, "graph-b.mp4")
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out": out, "out_dir": dir}), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if got := readString(t, out); got != "GA" {
		t.Fatalf("out must hold the primary output, got %q", got)
	}
	m := decode(t, res)
	job, _ := m["remote_job_id"].(string)
	secondary := filepath.Join(dir, job+"-graph-b.mp4")
	if got := readString(t, secondary); got != "GB" {
		t.Fatalf("the secondary output was destroyed or misplaced: dir holds %v", dirNames(t, dir))
	}
	if m["image_path"] != out {
		t.Errorf("image_path %v, want out %s", m["image_path"], out)
	}
	var secondaryPath string
	for _, files := range m["outputs"].(map[string]any) {
		for _, f := range files.([]any) {
			if p := f.(map[string]any)["path"].(string); p != out {
				secondaryPath = p
			}
		}
	}
	if secondaryPath != secondary {
		t.Errorf("the secondary's reported path %q must be its own file %q, not out", secondaryPath, secondary)
	}
	noTempLeft(t, dir)
}

// ---- R8: a fetch that never finished leaves nothing the next one cannot sweep -----------------------

func TestAStaleFetchLeftoverIsSweptBeforeTheNextClaim(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	dir := cfg.MediaDir
	// Stale is past the sweep's threshold (the longest call budget plus an hour); "mid" is older than the
	// old one-hour constant but still inside the threshold, so a call that is still running keeps its files.
	old := time.Now().Add(-staleAfter() - time.Minute)
	mid := time.Now().Add(-2 * time.Hour)
	age := func(name string) {
		t.Helper()
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	ageMid := func(name string) {
		t.Helper()
		if err := os.Chtimes(filepath.Join(dir, name), mid, mid); err != nil {
			t.Fatal(err)
		}
	}
	const jobA, jobB = "media-0123456789abcdef-", "media-fedcba9876543210-"
	for name, body := range map[string]string{
		".media-fetch-aaaa.part":  "partial",  // stale temp: swept
		".media-fetch-fresh.part": "partial",  // a download that may still be running: kept
		".media-fetch-mid.part":   "partial",  // 2 h old, inside the threshold (a 6 h call may still hold it): kept
		jobA + "mid.png":          "",         // 2 h old empty claim, inside the threshold: kept
		jobA + "x.png":            "",         // stale empty claim: swept
		jobA + "full.png":         "REAL",     // a fetched file: kept however old
		jobB + "fresh.png":        "",         // an empty claim that is not stale: kept
		"graph-a.png":             "",         // an empty file with a node name: the user's, kept
		"notes.part":              "not ours", // the wrong shape: kept
	} {
		writeFile(t, dir, name, []byte(body))
	}
	for _, name := range []string{".media-fetch-aaaa.part", jobA + "x.png", jobA + "full.png", "graph-a.png", "notes.part"} {
		age(name)
	}
	ageMid(".media-fetch-mid.part")
	ageMid(jobA + "mid.png")
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	have := map[string]bool{}
	for _, name := range dirNames(t, dir) {
		have[name] = true
	}
	for _, gone := range []string{".media-fetch-aaaa.part", jobA + "x.png"} {
		if have[gone] {
			t.Errorf("stale leftover %s was not swept: %v", gone, dirNames(t, dir))
		}
	}
	for _, kept := range []string{".media-fetch-fresh.part", ".media-fetch-mid.part", jobA + "mid.png", jobA + "full.png", jobB + "fresh.png", "graph-a.png", "notes.part"} {
		if !have[kept] {
			t.Errorf("%s must not be swept: %v", kept, dirNames(t, dir))
		}
	}
	if readString(t, filepath.Join(dir, jobA+"full.png")) != "REAL" {
		t.Error("a fetched file's content changed")
	}
}

// ---- R5: a budget that ends the call says what it cannot undo ---------------------------------------

// A media job cannot be withdrawn (ADR 0064), so the call does not try: it names the job and says the node
// may still be running it. The fake answers 405 to a DELETE, as a real node does, and must never see one.
func TestABudgetThatEndsTheCallSaysTheJobCannotBeRecalled(t *testing.T) {
	withBudget(t, taskImage, 300*time.Millisecond)
	for name, state := range map[string]string{"rendering": "running", "queued": "queued"} {
		log := &fakeLog{}
		fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobState: state, log: log})
		cfg := config.Config{MediaDir: t.TempDir(), FleetAuthToken: "tok", DelegateRemotes: []string{fake.URL}}
		start := time.Now()
		res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
		if res.OK || res.DeferClass != core.DeferClassBudget {
			t.Fatalf("%s: %+v", name, res)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: a budget defer must not spend extra time on a request of its own: %v", name, time.Since(start))
		}
		if del := log.deleted(); len(del) != 0 {
			t.Fatalf("%s: a media job is never withdrawn, so no DELETE may be sent: %+v", name, del)
		}
		for _, want := range []string{"remote job media-", "may still be running the job", "cannot be recalled", "ADR 0064"} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("%s: the defer must say %q: %s", name, want, res.Reason)
			}
		}
		if strings.Contains(res.Reason, "withdrew") || strings.Contains(res.Reason, "finished the render") {
			t.Errorf("%s: wrong phase wording: %s", name, res.Reason)
		}
	}
}

// A deadline that passes while the outputs are fetched is not a job still running: the render is finished.
func TestABudgetThatEndsDuringTheFetchSaysTheRenderFinished(t *testing.T) {
	withBudget(t, taskImage, 500*time.Millisecond)
	log := &fakeLog{}
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: `{"image_path":"x.png"}`, mediaHang: true, log: log})
	cfg := config.Config{MediaDir: t.TempDir(), FleetAuthToken: "tok", DelegateRemotes: []string{fake.URL}}
	start := time.Now()
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if time.Since(start) > 10*time.Second || res.OK || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("%v %+v", time.Since(start), res)
	}
	if !strings.Contains(res.Reason, "finished the render") || !strings.Contains(res.Reason, "fetched") || strings.Contains(res.Reason, "may still be running") || strings.Contains(res.Reason, "cannot be recalled") {
		t.Errorf("the fetch-phase expiry must say the render finished and the fetch ran out of time: %s", res.Reason)
	}
	if del := log.deleted(); len(del) != 0 {
		t.Errorf("no DELETE is ever sent: %+v", del)
	}
	noTempLeft(t, cfg.MediaDir)
	if got := dirNames(t, cfg.MediaDir); len(got) != 0 {
		t.Errorf("nothing may be kept after a fetch that ran out of time: %v", got)
	}
}

// ---- R6: failures that alternate with answers are not "consecutive" ----------------------------------

func TestPollFailuresThatAlternateWithAnswersNeverEndTheWait(t *testing.T) {
	media := t.TempDir()
	writeFile(t, media, "x.png", []byte("PNG"))
	log := &fakeLog{}
	// Twelve polls: six failures, each followed by a running answer, then done.
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, alternate: 6, jobData: `{"image_path":"x.png"}`, media: media, log: log})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if !res.OK {
		t.Fatalf("%d failures in all, never %d in a row, is not an outage: %+v", 6, maxPollFailures, res)
	}
	if got := len(log.seen("/fleet/jobs/")); got != 13 {
		t.Errorf("polled %d times, want 13", got)
	}
}

// ---- R7: the Call-level body backstop ---------------------------------------------------------------

// json.Marshal writes <, > and & as six-byte escapes, so a graph that passes the plan's size check (it
// counts the raw bytes) can be over the node's 1 MiB body once it is encoded for the wire.
func TestAGraphThatEscapesPastTheNodesBodyCapIsRefusedBeforeTheNetwork(t *testing.T) {
	log := &fakeLog{}
	fake := fakeNode(t, fakeOpts{tasks: []string{"run-graph"}, jobData: `{"image_path":"x.png"}`, log: log})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	graph := writeFile(t, t.TempDir(), "g.json", []byte(`{"pad":"`+strings.Repeat("<", 200<<10)+`"}`))
	if len(`{"pad":"`)+200<<10+2 > maxGraphBytes {
		t.Fatal("test premise: the raw graph must pass the plan check")
	}
	res := Run(context.Background(), cfg, &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": graph}}, "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "a fleet dispatch carries") {
		t.Fatalf("%+v", res)
	}
	if got := log.seen("/fleet/dispatch"); len(got) != 0 {
		t.Fatalf("the oversized body was sent to the node: %v", got)
	}
}
