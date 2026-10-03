package mediaremote

// Review round for the media-job work (register CT-50): each test here fails without the guard it names.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

func graphJob(t *testing.T, params map[string]any) core.Request {
	t.Helper()
	p := map[string]any{"graph_path": writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{"class_type":"X"}}`))}
	for k, v := range params {
		p[k] = v
	}
	return core.Request{Task: core.TaskRunGraph, Params: p}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- F5: fetched files never overwrite an existing local file --------------------------------------

func TestAFetchedOutputNeverReplacesAFileThatIsAlreadyThere(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	// The node's ComfyUI counter names collide with files this machine already holds.
	mine := map[string]string{"graph-a.png": "MINE-A", "graph-b.mp4": "MINE-B"}
	for name, body := range mine {
		writeFile(t, cfg.MediaDir, name, []byte(body))
	}
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	for name, body := range mine {
		if got := readString(t, filepath.Join(cfg.MediaDir, name)); got != body {
			t.Errorf("%s was replaced by a download: %q", name, got)
		}
	}
	m := decode(t, res)
	primary, _ := m["image_path"].(string)
	job, _ := m["remote_job_id"].(string)
	if readString(t, primary) != "GA" || primary == filepath.Join(cfg.MediaDir, "graph-a.png") || filepath.Dir(primary) != cfg.MediaDir {
		t.Errorf("the primary output must land beside, not over, the existing file: %s", primary)
	}
	secondary := filepath.Join(cfg.MediaDir, job+"-graph-b.mp4")
	if readString(t, secondary) != "GB" {
		t.Errorf("the secondary output must carry the job id: %v", dirNames(t, cfg.MediaDir))
	}
	noTempLeft(t, cfg.MediaDir)

	// Two jobs in a row with the same node file names: neither replaces the other's files.
	res2 := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res2.OK {
		t.Fatalf("%+v", res2)
	}
	if p2 := decode(t, res2)["image_path"].(string); p2 == primary || readString(t, primary) != "GA" || readString(t, p2) != "GA" {
		t.Errorf("a second job must not replace the first's primary: %s vs %s", p2, primary)
	}
	for _, name := range dirNames(t, cfg.MediaDir) {
		if strings.HasPrefix(name, ".") {
			t.Errorf("a staging file was left: %s", name)
		}
	}

	// Without a collision the primary keeps the node's name (what callers already expect).
	fresh := clientCfg(t, n)
	if p := decode(t, Run(context.Background(), fresh, &recordingRunner{}, graphJob(t, nil), "remote", nil))["image_path"]; p != filepath.Join(fresh.MediaDir, "graph-a.png") {
		t.Errorf("an unclaimed primary keeps the node's file name, got %v", p)
	}
}

// Only the caller's explicit out may be replaced.
func TestOnlyTheCallersOutMayBeReplaced(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	out := writeFile(t, t.TempDir(), "main.png", []byte("OLD"))
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out": out}), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if readString(t, out) != "GA" {
		t.Fatalf("an explicit out is the one file a download replaces: %q", readString(t, out))
	}
}

// ---- F6: a failing placement leaves nothing half-done and says what landed -------------------------

func TestAFailedPlacementRemovesEveryTempAndNamesWhatLanded(t *testing.T) {
	n := startNode(t, nodeOpts{})
	prev := renameFile
	t.Cleanup(func() { renameFile = prev })

	for _, failAt := range []int32{1, 2} {
		cfg := clientCfg(t, n)
		outDir := t.TempDir()
		out := writeFile(t, outDir, "main.png", []byte("MINE"))
		var calls atomic.Int32
		renameFile = func(from, to string) error {
			if calls.Add(1) == failAt {
				return errors.New("disk said no")
			}
			return prev(from, to)
		}
		res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out": out}), "remote", nil)
		if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "disk said no") {
			t.Fatalf("failAt %d: %+v", failAt, res)
		}
		if got := readString(t, out); got != "MINE" {
			t.Errorf("failAt %d: the caller's out was replaced although the placement failed: %q", failAt, got)
		}
		if left := dirNames(t, outDir); len(left) != 1 {
			t.Errorf("failAt %d: the out directory holds %v, want only the caller's file", failAt, left)
		}
		media := dirNames(t, cfg.MediaDir)
		if failAt == 1 {
			// Nothing landed: every claim and every temp is gone, and the error says so.
			if len(media) != 0 || !strings.Contains(res.Reason, "files already in place: none") {
				t.Errorf("failAt 1: media_dir holds %v, reason %q", media, res.Reason)
			}
			continue
		}
		// The secondary output landed before the caller's out failed; the error names it, nothing else is left.
		if len(media) != 1 || !strings.HasSuffix(media[0], "-graph-b.mp4") || !strings.Contains(res.Reason, media[0]) {
			t.Errorf("failAt 2: media_dir holds %v, reason %q", media, res.Reason)
		}
	}
}

// ---- F7: run_graph out_dir ------------------------------------------------------------------------

func TestRunGraphOutDirReceivesTheFetchedOutputs(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	outDir := filepath.Join(t.TempDir(), "renders", "today")
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out_dir": outDir}), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if got := dirNames(t, outDir); len(got) != 2 || got[0] != "graph-a.png" || got[1] != "graph-b.mp4" {
		t.Fatalf("out_dir holds %v, want both outputs under the node's names", got)
	}
	if got := dirNames(t, cfg.MediaDir); len(got) != 0 {
		t.Errorf("with an out_dir nothing goes to media_dir: %v", got)
	}
	m := decode(t, res)
	if m["image_path"] != filepath.Join(outDir, "graph-a.png") {
		t.Errorf("image_path %v", m["image_path"])
	}
	for _, files := range m["outputs"].(map[string]any) {
		for _, f := range files.([]any) {
			if p := f.(map[string]any)["path"].(string); filepath.Dir(p) != outDir {
				t.Errorf("an outputs path is not under out_dir: %s", p)
			}
		}
	}
	var wire struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(n.posts()[0].body, &wire)
	if _, has := wire.Payload["out_dir"]; has {
		t.Error("out_dir must never travel to the node")
	}
	// Existing files in out_dir are not replaced either.
	res = Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out_dir": outDir}), "remote", nil)
	if !res.OK || len(dirNames(t, outDir)) != 4 || readString(t, filepath.Join(outDir, "graph-a.png")) != "GA" {
		t.Fatalf("a second job into the same out_dir: %v %+v", dirNames(t, outDir), res)
	}
	// out and out_dir together: the primary goes to out, the rest to out_dir.
	out := filepath.Join(t.TempDir(), "main.png")
	out2 := filepath.Join(t.TempDir(), "again")
	res = Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out_dir": out2, "out": out}), "remote", nil)
	if !res.OK || readString(t, out) != "GA" || len(dirNames(t, out2)) != 1 || dirNames(t, out2)[0] != "graph-b.mp4" {
		t.Fatalf("out + out_dir: out2 holds %v, %+v", dirNames(t, out2), res)
	}
}

// An out_dir this machine cannot create is refused by name before the network is touched.
func TestAnOutDirThatCannotBeCreatedIsRefusedBeforeTheNetwork(t *testing.T) {
	n := startNode(t, nodeOpts{})
	cfg := clientCfg(t, n)
	blocker := writeFile(t, t.TempDir(), "afile", []byte("x"))
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out_dir": filepath.Join(blocker, "sub")}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "out_dir") {
		t.Fatalf("%+v", res)
	}
	if len(n.requests()) != 0 {
		t.Fatalf("the node was contacted: %+v", n.requests())
	}
}

// ---- F8, F9: errors keep their real cause and class ------------------------------------------------

func TestAnUnencodableParameterIsAContractDeferNamingTheCause(t *testing.T) {
	n := startNode(t, nodeOpts{})
	res := Run(context.Background(), clientCfg(t, n), &recordingRunner{}, video(map[string]any{"reserve_vram": math.Inf(1)}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Reason, "+Inf") || strings.Contains(res.Reason, "prompt required") {
		t.Fatalf("the defer must name the real cause: %s", res.Reason)
	}
	if len(n.requests()) != 0 {
		t.Fatal("the network was touched")
	}
}

func TestBundlingFailuresAreInfrastructureAndABadInputFileIsContract(t *testing.T) {
	cfg := config.Config{}
	// A file that cannot be opened is the caller's.
	_, _, err := buildBundle(cfg, []input{{"still", filepath.Join(t.TempDir(), "gone.png")}})
	var ce *contractError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "still") {
		t.Fatalf("an unreadable input file: want a contract error naming the field, got %T %v", err, err)
	}
	still := writeFile(t, t.TempDir(), "s.png", png)
	inputs := []input{{"still", still}}

	// This machine's temp directory failing is not.
	bad := filepath.Join(t.TempDir(), "no", "such", "dir")
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, bad)
	}
	_, _, err = buildBundle(cfg, inputs)
	var pe *placementError
	if !errors.As(err, &pe) || pe.class != core.DeferClassInfrastructure {
		t.Fatalf("a temp directory that cannot be made: want infrastructure, got %T %v", err, err)
	}
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, t.TempDir())
	}

	// So is the packer failing on what was just copied.
	prev := packFn
	t.Cleanup(func() { packFn = prev })
	packFn = func(string, composebundle.Limits) ([]byte, error) { return nil, errors.New("read error on the copy") }
	_, _, err = buildBundle(cfg, inputs)
	if !errors.As(err, &pe) || pe.class != core.DeferClassInfrastructure || !strings.Contains(err.Error(), "read error on the copy") {
		t.Fatalf("a packer failure: want infrastructure naming the cause, got %T %v", err, err)
	}
	if errors.As(err, &ce) {
		t.Fatal("a local I/O failure is never the caller's contract")
	}
}

// ---- F10, F20: budgets ----------------------------------------------------------------------------

func withBudget(t *testing.T, task string, d time.Duration) {
	t.Helper()
	prev := Budgets[task]
	Budgets[task] = d
	t.Cleanup(func() { Budgets[task] = prev })
}

func neverFinishes(t *testing.T) (*fakeLog, config.Config) {
	t.Helper()
	log := &fakeLog{}
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobState: "running", log: log})
	return log, config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
}

func imageReq() core.Request { return core.Request{Task: core.TaskGenerateImage, Input: "p"} }

// With no deadline of its own the call is bounded by the task's budget, and its expiry is a BUDGET defer
// that names the node and the remote job (which may still hold the card).
func TestTheTaskBudgetEndsACallThatSetNoDeadlineAsABudgetDefer(t *testing.T) {
	withBudget(t, taskImage, 300*time.Millisecond)
	log, cfg := neverFinishes(t)
	start := time.Now()
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if time.Since(start) > 10*time.Second {
		t.Fatal("the budget did not bound the call")
	}
	if res.OK || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("an expired budget is a budget defer, got class %q: %+v", res.DeferClass, res)
	}
	for _, want := range []string{"node fake-", "remote job media-", "may still be running"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the defer must name %q: %s", want, res.Reason)
		}
	}
	if len(log.seen("/fleet/jobs/")) < 2 {
		t.Error("the call must have polled the job while it waited")
	}
}

// A caller's shorter deadline wins over the budget; a caller's longer one is not cut down to it.
func TestACallersDeadlineIsHonouredInBothDirections(t *testing.T) {
	withBudget(t, taskImage, time.Hour)
	_, cfg := neverFinishes(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := Run(ctx, cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if time.Since(start) > 10*time.Second || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("a caller's 300 ms deadline must end the call as a budget defer within seconds (class %q, %v): %+v", res.DeferClass, time.Since(start), res)
	}

	// The reverse: the task budget is 50 ms but the caller allowed an hour and the node finishes in 400 ms.
	withBudget(t, taskImage, 50*time.Millisecond)
	media := t.TempDir()
	writeFile(t, media, "x.png", []byte("PNG"))
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, doneAfter: 400 * time.Millisecond, jobData: `{"image_path":"x.png"}`, media: media})
	cfg2 := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if res := Run(ctx2, cfg2, &recordingRunner{}, imageReq(), "remote", nil); !res.OK {
		t.Fatalf("a caller's longer deadline must not be cut to the task budget: %+v", res)
	}
	// And with no deadline at all the 50 ms budget does apply to the same slow node.
	if res := Run(context.Background(), cfg2, &recordingRunner{}, imageReq(), "remote", nil); res.OK || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("the budget must apply when the caller set none: %+v", res)
	}
}

// ---- F21: a node that lost the job, or keeps failing, ends the wait --------------------------------

func TestAPollThatIs404EndsTheWaitAtOnce(t *testing.T) {
	log := &fakeLog{}
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, pollStatus: 404, log: log})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "denies holding it") {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Reason, "media-") {
		t.Errorf("the defer must name the job: %s", res.Reason)
	}
	if got := len(log.seen("/fleet/jobs/")); got != 1 {
		t.Fatalf("a 404 must end the wait on the first poll, polled %d times", got)
	}
}

func TestConsecutivePollFailuresEndTheWaitAfterFiveAndAResetByASuccess(t *testing.T) {
	log := &fakeLog{}
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, pollStatus: 500, log: log})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "5 consecutive poll failures") {
		t.Fatalf("%+v", res)
	}
	if got := len(log.seen("/fleet/jobs/")); got != maxPollFailures {
		t.Fatalf("polled %d times, want exactly %d", got, maxPollFailures)
	}

	// Four failures and then a good answer is not an outage.
	media := t.TempDir()
	writeFile(t, media, "x.png", []byte("PNG"))
	log2 := &fakeLog{}
	flaky := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, pollFailures: maxPollFailures - 1, jobData: `{"image_path":"x.png"}`, media: media, log: log2})
	cfg2 := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{flaky.URL}}
	if res := Run(context.Background(), cfg2, &recordingRunner{}, imageReq(), "remote", nil); !res.OK {
		t.Fatalf("four failed polls then a done job must succeed: %+v", res)
	}
}

// ---- F25: results that are only partly verified, empty, refused, or cut short ----------------------

func TestAPartlyPublishedResultIsMarkedUnverifiedButStillCheckedWhereItCan(t *testing.T) {
	media := t.TempDir()
	writeFile(t, media, "a.png", []byte("AAA"))
	writeFile(t, media, "b.mp4", []byte("BBB"))
	sum := sha256.Sum256([]byte("AAA"))
	data := fmt.Sprintf(`{"image_path":"/n/a.png","outputs":{"1":[{"path":"/n/a.png"}],"2":[{"path":"/n/b.mp4"}]},"artifacts":[{"name":"a.png","bytes":3,"sha256":%q}]}`, hex.EncodeToString(sum[:]))
	fake := fakeNode(t, fakeOpts{tasks: []string{"run-graph"}, jobData: data, media: media})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, nil), "remote", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	m := decode(t, res)
	if m["unverified"] != true {
		t.Fatalf("one output has no published hash: the result must say unverified: %v", m)
	}
	if got := readString(t, m["image_path"].(string)); got != "AAA" {
		t.Errorf("the verified output holds %q", got)
	}
	// Every output arrived, the unverified one too.
	if names := dirNames(t, cfg.MediaDir); len(names) != 2 {
		t.Errorf("media_dir holds %v, want both outputs", names)
	}
	// The one that IS published is still checked: a wrong hash for it defers.
	bad := fmt.Sprintf(`{"image_path":"/n/a.png","outputs":{"2":[{"path":"/n/b.mp4"}]},"artifacts":[{"name":"a.png","bytes":3,"sha256":%q}]}`, strings.Repeat("0", 64))
	fake2 := fakeNode(t, fakeOpts{tasks: []string{"run-graph"}, jobData: bad, media: media})
	cfg2 := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake2.URL}}
	if res := Run(context.Background(), cfg2, &recordingRunner{}, graphJob(t, nil), "remote", nil); res.OK || !strings.Contains(res.Reason, "sha256") {
		t.Fatalf("a published hash that does not match must defer: %+v", res)
	}
	if names := dirNames(t, cfg2.MediaDir); len(names) != 0 {
		t.Errorf("a failed verification left %v", names)
	}
}

func TestADoneJobThatNamesNoOutputIsAnInfrastructureDefer(t *testing.T) {
	for name, data := range map[string]string{"no path": `{"seed":5,"kind":"voice"}`, "empty path": `{"image_path":""}`, "empty outputs": `{"outputs":{"9":[]}}`} {
		fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: data, media: t.TempDir()})
		cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
		res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
		if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "names no output file") || !strings.Contains(res.Reason, "node fake-") {
			t.Errorf("%s: %+v", name, res)
		}
	}
}

func TestAnUnauthorizedNodeIsAConfigDeferOnEveryStep(t *testing.T) {
	// The dispatch.
	fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, status: 401})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	if res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil); res.DeferClass != core.DeferClassConfig {
		t.Errorf("a 401 on dispatch: %+v", res)
	}
	// The poll.
	log := &fakeLog{}
	poll := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, pollStatus: 401, log: log})
	cfg = config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{poll.URL}}
	res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
	if res.DeferClass != core.DeferClassConfig || !strings.Contains(res.Reason, "fleet_auth_token") || len(log.seen("/fleet/jobs/")) != 1 {
		t.Errorf("a 401 on the poll must end the wait as a config defer: %+v (polls %d)", res, len(log.seen("/fleet/jobs/")))
	}
	// The fetch.
	fetch := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: `{"image_path":"/n/x.png"}`, media: t.TempDir(), mediaStatus: 401})
	cfg = config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fetch.URL}}
	if res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil); res.DeferClass != core.DeferClassConfig || !strings.Contains(res.Reason, "rendered the job") {
		t.Errorf("a 401 on the fetch: %+v", res)
	}
}

// A second download that fails removes the first one's temp and lands nothing.
func TestAFailedSecondDownloadLeavesNoPartFileAndLandsNothing(t *testing.T) {
	media := t.TempDir()
	writeFile(t, media, "a.png", []byte("AAA"))
	// b.mp4 is named by the node but not served: the second GET is a 404.
	data := `{"image_path":"/n/a.png","outputs":{"2":[{"path":"/n/b.mp4"}]}}`
	fake := fakeNode(t, fakeOpts{tasks: []string{"run-graph"}, jobData: data, media: media})
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
	outDir := t.TempDir()
	out := writeFile(t, outDir, "main.png", []byte("MINE"))
	res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out": out}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "b.mp4") {
		t.Fatalf("%+v", res)
	}
	if got := dirNames(t, cfg.MediaDir); len(got) != 0 {
		t.Errorf("media_dir holds %v after a failed fetch", got)
	}
	if got := dirNames(t, outDir); len(got) != 1 || readString(t, out) != "MINE" {
		t.Errorf("out dir holds %v (out %q): the first download's temp or the file itself was left", got, readString(t, out))
	}
}

// ---- F26: a hostile result path is pinned for real ------------------------------------------------

func TestAHostileResultPathCollapsesToAPlainNameInsideMediaDir(t *testing.T) {
	serve := t.TempDir()
	writeFile(t, serve, "passwd", []byte("NODE-FILE"))
	for name, imagePath := range map[string]string{
		"unix traversal":    "../../../etc/passwd",
		"windows traversal": `..\..\..\Windows\passwd`,
		"absolute":          "/etc/passwd",
		"drive absolute":    `C:\Windows\passwd`,
	} {
		t.Run(name, func(t *testing.T) {
			log := &fakeLog{}
			raw, _ := json.Marshal(map[string]any{"image_path": imagePath})
			fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: string(raw), media: serve, log: log})
			parent := t.TempDir()
			cfg := config.Config{MediaDir: filepath.Join(parent, "media"), DelegateRemotes: []string{fake.URL}}
			res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
			if !res.OK {
				t.Fatalf("%+v", res)
			}
			// What went on the wire is the bare name, never the path the node reported.
			if got := log.seen("/fleet/media/"); len(got) != 1 || got[0] != "/fleet/media/passwd" {
				t.Fatalf("media requests %v, want exactly /fleet/media/passwd", got)
			}
			// And what landed is a plain file directly inside media_dir.
			p := decode(t, res)["image_path"].(string)
			if filepath.Dir(p) != cfg.MediaDir || readString(t, p) != "NODE-FILE" {
				t.Fatalf("landed at %s", p)
			}
			if got := dirNames(t, parent); len(got) != 1 || got[0] != "media" {
				t.Errorf("something was written outside media_dir: %v", got)
			}
		})
	}
	// Names that are not plain names are refused before a single byte is fetched.
	for _, bad := range []string{"..", ".", "/", "dir/..", "C:", "a:b", "x.png:stream"} {
		log := &fakeLog{}
		raw, _ := json.Marshal(map[string]any{"image_path": bad})
		fake := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, jobData: string(raw), media: serve, log: log})
		cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{fake.URL}}
		res := Run(context.Background(), cfg, &recordingRunner{}, imageReq(), "remote", nil)
		if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "not a plain file name") {
			t.Errorf("%q: %+v", bad, res)
		}
		if got := log.seen("/fleet/media/"); len(got) != 0 {
			t.Errorf("%q: the client fetched %v for a name it refuses", bad, got)
		}
	}
}

// ---- F18, F15: the local-lane verdict --------------------------------------------------------------

func stubLocalRoutes(t *testing.T, fn func(config.Config) []mediacap.Route) {
	t.Helper()
	prev := routesFn
	routesFn = fn
	localRoutes.Reset()
	t.Cleanup(func() { routesFn = prev; localRoutes.Reset() })
}

func configured(names ...string) func(config.Config) []mediacap.Route {
	return func(config.Config) []mediacap.Route {
		var out []mediacap.Route
		for _, n := range names {
			out = append(out, mediacap.Route{Name: n, State: mediacap.Configured})
		}
		return out
	}
}

func TestLocalConfiguredMapsEachTaskToItsOwnRoute(t *testing.T) {
	overlay := func(raw string) config.FamilyOverlay {
		var o config.FamilyOverlay
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	cfg := config.Config{
		ImageGenFamily: "sdxl-house",
		ImageGenFamilies: map[string]config.FamilyOverlay{
			"qi21": overlay(`{"license":"Qwen Research License","commercial_use":false,"imagegen_family":"qwen-image-2.1"}`),
		},
	}
	image := core.Request{Task: core.TaskGenerateImage}
	named := core.Request{Task: core.TaskGenerateImage, Params: map[string]any{"family": "qi21"}}
	defaultByName := core.Request{Task: core.TaskGenerateImage, Params: map[string]any{"family": "sdxl-house"}}
	unknown := core.Request{Task: core.TaskGenerateImage, Params: map[string]any{"family": "nope"}}
	animate := core.Request{Task: core.TaskAnimateCharacter}
	music := core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"kind": "music"}}
	voice := core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"voice": "generalist"}}
	anyAudio := core.Request{Task: core.TaskGenerateAudio}

	for _, tc := range []struct {
		name  string
		have  []string
		req   core.Request
		local bool
	}{
		{"default image with its route", []string{"generate_image"}, image, true},
		{"default image without it", []string{"generate_video", "animate_character"}, image, false},
		{"named family with its own route", []string{"generate_image:qi21"}, named, true},
		{"named family on the default route only", []string{"generate_image"}, named, false},
		{"the default family named explicitly", []string{"generate_image"}, defaultByName, true},
		{"an unknown family is never local", []string{"generate_image", "generate_image:nope"}, unknown, false},
		{"animate with its route", []string{"animate_character"}, animate, true},
		{"animate with only the video route", []string{"generate_video"}, animate, false},
		{"video with its route", []string{"generate_video"}, video(nil), true},
		{"video with only the animate route", []string{"animate_character"}, video(nil), false},
		{"music with its route", []string{"generate_audio:music"}, music, true},
		{"music with only voice", []string{"generate_audio:voice"}, music, false},
		{"voice with its route", []string{"generate_audio:voice"}, voice, true},
		{"generalist voice with only the endpoint", []string{"generate_audio:voice:endpoint"}, voice, false},
		{"any audio with the endpoint", []string{"generate_audio:voice:endpoint"}, anyAudio, true},
		{"any audio with only music", []string{"generate_audio:music"}, anyAudio, false},
		{"run_graph with its route", []string{"run_graph"}, core.Request{Task: core.TaskRunGraph}, true},
	} {
		stubLocalRoutes(t, configured(tc.have...))
		if got := LocalConfigured(cfg, tc.req); got != tc.local {
			t.Errorf("%s: LocalConfigured = %v, want %v", tc.name, got, tc.local)
		}
	}
}

// The verdict is read from the disk at most once a minute: auto no longer walks the files on every call.
func TestLocalConfiguredIsCachedForAtMostSixtySeconds(t *testing.T) {
	var calls atomic.Int32
	stubLocalRoutes(t, func(config.Config) []mediacap.Route {
		calls.Add(1)
		return []mediacap.Route{{Name: "run_graph", State: mediacap.Configured}}
	})
	prev := localClock
	t.Cleanup(func() { localClock = prev })
	now := time.Now()
	localClock = func() time.Time { return now }
	cfg := config.Config{RunGraphScript: "x.mjs"}
	req := core.Request{Task: core.TaskRunGraph}
	for i := 0; i < 5; i++ {
		if !LocalConfigured(cfg, req) {
			t.Fatal("the lane is configured")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d derivations for five calls inside a minute, want 1", calls.Load())
	}
	now = now.Add(59 * time.Second)
	LocalConfigured(cfg, req)
	if calls.Load() != 1 {
		t.Fatal("a call at 59 s must hit the cache")
	}
	now = now.Add(2 * time.Second)
	LocalConfigured(cfg, req)
	if calls.Load() != 2 {
		t.Fatal("a call past 60 s must read the files again")
	}
}

// ---- F22, F23: every parameter reaches the node under the field name ITS builder reads -------------

// nodeCfg is the config a node builds with: every script bound.
func nodeCfg(t *testing.T) config.Config {
	t.Helper()
	t.Cleanup(fleetnode.SetMediaRoutesSourceForTest(bindOnly))
	return config.Config{
		MediaDir: t.TempDir(), FleetAuthToken: "tok", FleetMediaInputs: true,
		ImageGenScript: "render/comfy-generate.mjs", VideoGenScript: "render/comfy-video.mjs",
		AnimateGenScript: "render/comfy-animate.mjs", VoiceGenScript: "render/tts.mjs",
		MusicGenScript: "render/comfy-music.mjs", RunGraphScript: "render/comfy-run-graph.mjs",
	}
}

// roundTrip plans req, puts the input files where the node's media-job door would (a node path in the field),
// encodes the payload as the wire carries it and hands it to the node's own builder.
func roundTrip(t *testing.T, req core.Request) core.Request {
	t.Helper()
	pl, err := plan(req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, in := range pl.inputs {
		pl.payload[in.field] = "/node/inputs/" + in.field
	}
	raw, err := json.Marshal(pl.payload)
	if err != nil {
		t.Fatal(err)
	}
	built, cleanup, err := fleetnode.BuildRequest(context.Background(), nodeCfg(t), true, pl.fleetTask, raw)
	if err != nil {
		t.Fatalf("the node's builder refused the payload %s: %v", raw, err)
	}
	t.Cleanup(cleanup)
	if built.Task != req.Task {
		t.Fatalf("the node builds %q for %q", built.Task, req.Task)
	}
	return built
}

func TestEveryMCPParameterReachesTheNodeBuilderUnderItsFieldName(t *testing.T) {
	dir := t.TempDir()
	still := writeFile(t, dir, "s.png", png)
	ref := writeFile(t, dir, "r.png", png)
	driver := writeFile(t, dir, "d.mp4", mp4)
	clone := writeFile(t, dir, "c.wav", wav)

	t.Run("image", func(t *testing.T) {
		built := roundTrip(t, core.Request{Task: core.TaskGenerateImage, Input: "a door", Params: map[string]any{
			"negative": "text", "family": "qi21", "transparent": true, "width": 512, "height": 768, "steps": 20, "seed": 3, "out": "/ignored.png"}})
		want := map[string]any{"negative": "text", "family": "qi21", "transparent": true, "width": 512, "height": 768, "steps": 20, "seed": 3}
		if built.Input != "a door" || !reflect.DeepEqual(built.Params, want) {
			t.Fatalf("input %q params %#v, want %#v", built.Input, built.Params, want)
		}
	})
	t.Run("video", func(t *testing.T) {
		built := roundTrip(t, core.Request{Task: core.TaskGenerateVideo, Input: "a slow pan", Params: map[string]any{
			"still": still, "model": "ltx25", "negative": "blur", "fast": true, "hero": true, "upscale": true,
			"frames": 33, "width": 640, "height": 360, "steps": 8, "seed": 7, "reserve_vram": "1.5"}})
		want := map[string]any{"still": "/node/inputs/still", "model": "ltx25", "negative": "blur", "fast": true, "hero": true, "upscale": true,
			"frames": 33, "width": 640, "height": 360, "steps": 8, "seed": 7, "reserve_vram": "1.5"}
		if built.Input != "a slow pan" || !reflect.DeepEqual(built.Params, want) {
			t.Fatalf("params %#v, want %#v", built.Params, want)
		}
	})
	t.Run("animate", func(t *testing.T) {
		built := roundTrip(t, core.Request{Task: core.TaskAnimateCharacter, Input: "a fox", Image: ref, Video: driver, Params: map[string]any{
			"motion_prompt": "a slow dance", "negative": "blur", "width": 482, "height": 854, "frames": 81, "steps": 10, "seed": 5,
			"pose_strength": "0.8", "ref_strength": "0.9", "reserve_vram": 2.0}})
		want := map[string]any{"ref": "/node/inputs/ref", "driver": "/node/inputs/driver", "motion_prompt": "a slow dance", "negative": "blur",
			"width": 482, "height": 854, "frames": 81, "steps": 10, "seed": 5, "pose_strength": "0.8", "ref_strength": "0.9", "reserve_vram": "2"}
		if built.Input != "a fox" || !reflect.DeepEqual(built.Params, want) {
			t.Fatalf("params %#v, want %#v", built.Params, want)
		}
	})
	t.Run("audio", func(t *testing.T) {
		built := roundTrip(t, core.Request{Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{
			"kind": "music", "voice": "generalist", "clone": clone, "lang": "es", "seconds": 30, "seed": 4, "reserve_vram": "1"}})
		want := map[string]any{"kind": "music", "voice": "generalist", "clone": "/node/inputs/clone", "lang": "es", "seconds": 30, "seed": 4, "reserve_vram": "1"}
		if built.Input != "hola" || !reflect.DeepEqual(built.Params, want) {
			t.Fatalf("params %#v, want %#v", built.Params, want)
		}
	})
	t.Run("run-graph", func(t *testing.T) {
		built := roundTrip(t, core.Request{Task: core.TaskRunGraph, Params: map[string]any{
			"graph_path":    writeFile(t, dir, "g.json", []byte(`{"1":{"class_type":"KSampler"}}`)),
			"manifest_path": writeFile(t, dir, "m.json", []byte(`{"models":[]}`)),
			"reserve_vram":  "0.5", "model_family": "wan2.2", "out_dir": "/never/travels"}})
		if built.Params["reserve_vram"] != "0.5" || built.Params["model_family"] != "wan2.2" {
			t.Fatalf("params %#v", built.Params)
		}
		if _, has := built.Params["out_dir"]; has {
			t.Error("out_dir reached the node's builder")
		}
		if g := readString(t, built.Params["graph_path"].(string)); g != `{"1":{"class_type":"KSampler"}}` {
			t.Errorf("the node's graph file holds %q", g)
		}
		if m := readString(t, built.Params["manifest_path"].(string)); m != `{"models":[]}` {
			t.Errorf("the node's manifest file holds %q", m)
		}
	})
}

// The animate task is judged on the animate_character route, not on video's (and the audio kinds on theirs).
func TestNodeRoutesPerTaskAndKind(t *testing.T) {
	for name, tc := range map[string]struct {
		req  core.Request
		want []string
	}{
		"video":               {video(nil), []string{"generate_video"}},
		"animate":             {core.Request{Task: core.TaskAnimateCharacter}, []string{"animate_character"}},
		"run-graph":           {core.Request{Task: core.TaskRunGraph}, []string{"run_graph"}},
		"music":               {core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"kind": "Music"}}, []string{"generate_audio:music"}},
		"endpoint voice":      {core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"voice": "endpoint"}}, []string{"generate_audio:voice:endpoint"}},
		"generalist voice":    {core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"voice": "generalist"}}, []string{"generate_audio:voice"}},
		"finetuned voice":     {core.Request{Task: core.TaskGenerateAudio, Params: map[string]any{"voice": "finetuned"}}, []string{"generate_audio:voice"}},
		"unspecified voice":   {core.Request{Task: core.TaskGenerateAudio}, []string{"generate_audio:voice", "generate_audio:voice:endpoint"}},
		"image (node judges)": {core.Request{Task: core.TaskGenerateImage}, nil},
	} {
		if got := nodeRoutes(tc.req); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: nodeRoutes = %v, want %v", name, got, tc.want)
		}
	}
	for task, want := range map[core.TaskType]string{
		core.TaskGenerateImage: taskImage, core.TaskGenerateVideo: taskVideo, core.TaskAnimateCharacter: taskAnimate,
		core.TaskGenerateAudio: taskAudio, core.TaskRunGraph: taskRunGraph,
	} {
		if got, ok := fleetTaskOf(task); !ok || got != want {
			t.Errorf("fleetTaskOf(%s) = %q, %v; want %q", task, got, ok, want)
		}
	}
}

// End to end: a node that reports animate_character BOUND-BUT-MISSING and generate_video CONFIGURED is not a
// node for an animation (mapping animate to video's route would pick it).
func TestAnimationGoesOnlyToANodeWhoseAnimateRouteIsConfigured(t *testing.T) {
	both := func(video, animate string) string {
		return fmt.Sprintf(`[{"route":"generate_video","engine":"comfyui","state":%q},{"route":"animate_character","engine":"comfyui","state":%q}]`, video, animate)
	}
	wrong := fakeNode(t, fakeOpts{tasks: []string{"animate", "media-job"}, routes: both("CONFIGURED", "BOUND-BUT-MISSING")})
	right := fakeNode(t, fakeOpts{tasks: []string{"animate", "media-job"}, routes: both("BOUND-BUT-MISSING", "CONFIGURED")})
	ctx := context.Background()
	routes := nodeRoutes(core.Request{Task: core.TaskAnimateCharacter})
	if _, _, err := pickNode(ctx, config.Config{}, []string{wrong.URL}, taskAnimate, taskMediaJob, routes); err == nil || !strings.Contains(err.Error(), "animate_character") || !strings.Contains(err.Error(), "BOUND-BUT-MISSING") {
		t.Fatalf("a node whose animate route is missing must be refused naming it: %v", err)
	}
	if base, _, err := pickNode(ctx, config.Config{}, []string{wrong.URL, right.URL}, taskAnimate, taskMediaJob, routes); err != nil || base != right.URL {
		t.Fatalf("picked %q, %v; want the node with a CONFIGURED animate route", base, err)
	}
}

// ---- F13: graph and manifest together ---------------------------------------------------------------

func TestAGraphAndManifestThatFitSeparatelyButNotTogetherAreRefusedByName(t *testing.T) {
	n := startNode(t, nodeOpts{})
	pad := func(n int) []byte { return []byte(`{"pad":"` + strings.Repeat("x", n) + `"}`) }
	dir := t.TempDir()
	graph := writeFile(t, dir, "g.json", pad(500<<10))
	manifest := writeFile(t, dir, "m.json", pad(500<<10))
	res := Run(context.Background(), clientCfg(t, n), &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": graph, "manifest_path": manifest}}, "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "graph") || !strings.Contains(res.Reason, "manifest") || !strings.Contains(res.Reason, "together") {
		t.Fatalf("%+v", res)
	}
	if len(n.requests()) != 0 {
		t.Fatal("the node was contacted for a request it would refuse")
	}
	// Each alone, at the same size, is fine.
	if res := Run(context.Background(), clientCfg(t, n), &recordingRunner{}, core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": graph}}, "remote", nil); !res.OK {
		t.Fatalf("a 500 KiB graph alone must travel: %+v", res)
	}
}

// ---- F16: a fleet with no eligible node costs one probe, not a bundle -------------------------------

func TestNoEligibleNodeMeansNothingIsPacked(t *testing.T) {
	noDoor := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}}) // serves video-gen, but has no media-job door
	cfg := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{noDoor.URL}}
	var packs atomic.Int32
	prev := packFn
	t.Cleanup(func() { packFn = prev })
	packFn = func(dir string, lim composebundle.Limits) ([]byte, error) {
		packs.Add(1)
		return prev(dir, lim)
	}
	still := writeFile(t, t.TempDir(), "s.png", png)
	res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"still": still}), "remote", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity || !strings.Contains(res.Reason, "does not advertise media-job") {
		t.Fatalf("%+v", res)
	}
	if packs.Load() != 0 {
		t.Fatalf("the bundle was packed %d time(s) before a node was chosen", packs.Load())
	}
	// With an eligible node it is packed exactly once.
	n := startNode(t, nodeOpts{mediaInputs: true})
	if res := Run(context.Background(), clientCfg(t, n), &recordingRunner{}, video(map[string]any{"still": still}), "remote", nil); !res.OK || packs.Load() != 1 {
		t.Fatalf("packs %d, %+v", packs.Load(), res)
	}
}
