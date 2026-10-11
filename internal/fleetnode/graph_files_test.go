package fleetnode

// The node's side of a remote graph (P0 plan, S4; ADR 0082 amendment). A run-graph dispatch names, inside its graph, the
// weight files its loaders open and the input files its input loaders read (an image, a video, an audio clip). A node that
// holds them all runs the graph; a node that lacks one used to take its lease, cold-start ComfyUI and defer after, so the
// delegator learned it a render later. Admission now reads the graph first and answers 503 (the re-placeable "this node
// cannot run this now") naming what is missing, before any job, temp file or lease exists.

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// graphNode is a run-graph node whose ComfyUI tree holds the given files (relative to the tree: models/<class>/..., input/...).
func graphNode(t *testing.T, files ...string) config.Config {
	t.Helper()
	comfy := t.TempDir()
	for _, rel := range files {
		p := filepath.Join(comfy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fullCfg()
	cfg.ComfyDir = comfy
	return cfg
}

func graphDispatch(id, graph, manifest string) string {
	m := ""
	if manifest != "" {
		m = `,"manifest":` + manifest
	}
	return `{"job_id":"` + id + `","task_type":"run-graph","payload":{"graph":` + graph + m + `}}`
}

const (
	wanGraph = `{
		"3": {"class_type":"UNETLoader","inputs":{"unet_name":"wan/high.safetensors"}},
		"4": {"class_type":"CLIPLoader","inputs":{"clip_name":"umt5.safetensors"}},
		"5": {"class_type":"VAELoader", "inputs":{"vae_name":"wan_vae.safetensors"}}
	}`
	refGraph = `{
		"1": {"class_type":"LoadImage","inputs":{"image":"ref.png"}},
		"3": {"class_type":"UNETLoader","inputs":{"unet_name":"wan/high.safetensors"}}
	}`
)

// leftoverGraphFiles lists the temp files a refused dispatch must not leave behind. The test points the process's
// temp directory at its own, so any file there came from the call.
func isolatedTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, dir)
	}
	return dir
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// THE ACK CHECK. A graph that names a weight file the node does not hold is refused with 503, and the refusal names the
// file (and only the files that are missing). No job exists, nothing runs, and the temp files the graph would have been
// written to are never created.
func TestRunGraphMissingLoaderFileIs503NamingIt(t *testing.T) {
	cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors", "models/text_encoders/umt5.safetensors")
	tmp := isolatedTemp(t)
	runner := &fakeRunner{}
	s, jobs := newTestServer(t, cfg, runner, nil)

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-missing", wanGraph, ""), nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "wan_vae.safetensors")
	body := rec.Body.String()
	for _, held := range []string{"wan/high.safetensors", "umt5.safetensors"} {
		if strings.Contains(body, held) {
			t.Errorf("the refusal names a file the node holds (%s): %s", held, body)
		}
	}
	if !strings.Contains(body, "run-graph") {
		t.Errorf("the refusal must say which task it is about: %s", body)
	}
	if queued, running := jobs.Counts(); queued+running != 0 || len(runner.requests()) != 0 {
		t.Errorf("a refused dispatch creates no job and runs nothing")
	}
	if rec := do(t, s, http.MethodGet, "/fleet/jobs/g-missing", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the refused job must not exist, got %d", rec.Code)
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Errorf("a refused dispatch left temp files behind: %v", left)
	}
}

// The same graph on a node that holds every file is admitted and run, so the check never refuses a node that could run it.
func TestRunGraphWithEveryFileHeldIsAdmitted(t *testing.T) {
	cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors", "models/text_encoders/umt5.safetensors", "models/vae/wan_vae.safetensors")
	runner := &fakeRunner{}
	s, _ := newTestServer(t, cfg, runner, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-held", wanGraph, ""), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a node that holds every file must accept the graph, got %d: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(runner.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(runner.requests()) != 1 {
		t.Fatalf("the admitted graph must run once, got %d", len(runner.requests()))
	}
}

// The file is looked for the way ComfyUI opens it: under any class directory of any models root, so a node that keeps a
// LoRA under loras and a UNet under diffusion_models, or on a second volume through extra_model_paths.yaml, holds both.
func TestRunGraphFilesAreFoundUnderEveryClassDirectoryAndRoot(t *testing.T) {
	cfg := graphNode(t, "models/unet/wan/high.safetensors", "models/clip/umt5.safetensors")
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "vae"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "vae", "wan_vae.safetensors"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := "second:\n  base_path: " + filepath.ToSlash(other) + "\n  vae: vae\n"
	if err := os.WriteFile(filepath.Join(cfg.ComfyDir, "extra_model_paths.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := BuildRequest(t.Context(), cfg, true, "run-graph", []byte(`{"graph":`+wanGraph+`}`))
	defer cleanup()
	if err != nil {
		t.Fatalf("every file is held (legacy class aliases and a second root): %v", err)
	}
}

// A class directory that is a link to a directory elsewhere (a models tree on a second volume, linked into place) is a class
// directory: the files behind it are held. A listing reports the link, not what it points at, and a node that dropped it
// refused graphs whose files it had.
func TestRunGraphFilesBehindALinkedClassDirectoryAreHeld(t *testing.T) {
	for kind, link := range map[string]func(link, target string) error{
		"symlink": os.Symlink,
		"junction": func(link, target string) error {
			if runtime.GOOS != "windows" {
				return os.ErrInvalid
			}
			return exec.Command("cmd", "/c", "mklink", "/J", link, target).Run()
		},
	} {
		t.Run(kind, func(t *testing.T) {
			cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors", "models/text_encoders/umt5.safetensors")
			store := t.TempDir()
			if err := os.WriteFile(filepath.Join(store, "wan_vae.safetensors"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := link(filepath.Join(cfg.ComfyDir, "models", "vae"), store); err != nil {
				t.Skipf("this host cannot make a %s here: %v", kind, err)
			}
			_, cleanup, err := BuildRequest(t.Context(), cfg, true, "run-graph", []byte(`{"graph":`+wanGraph+`}`))
			defer cleanup()
			if err != nil {
				t.Fatalf("the VAE is behind a %s and the node holds it: %v", kind, err)
			}
		})
	}
}

// A loader that names its file under a key other than *_name is read all the same: the scan goes by what the value is (a
// weight file name), because every custom node picks its own key. A node without the file refuses it at the door instead of
// cold-starting ComfyUI to defer, and a node that holds it runs the graph.
func TestRunGraphReadsAFileUnderAnyInputKeyAndAnInputLoaderOfAnyKind(t *testing.T) {
	const patchGraph = `{"1":{"class_type":"ModelPatchLoader","inputs":{"name":"qwen_inpaint_patch.safetensors"}},
		"2":{"class_type":"LoadVideo","inputs":{"file":"driver.mp4"}}}`
	cfg := graphNode(t)
	tmp := isolatedTemp(t)
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-patch", patchGraph, ""), nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "qwen_inpaint_patch.safetensors")
	if !strings.Contains(rec.Body.String(), "driver.mp4") {
		t.Errorf("a LoadVideo file the node lacks is named too: %s", rec.Body.String())
	}
	if queued, running := jobs.Counts(); queued+running != 0 {
		t.Errorf("a refused dispatch creates no job")
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Errorf("a refused dispatch left temp files behind: %v", left)
	}
	held := graphNode(t, "models/model_patches/qwen_inpaint_patch.safetensors", "input/driver.mp4")
	s2, _ := newTestServer(t, held, &fakeRunner{}, nil)
	if rec := do(t, s2, http.MethodPost, "/fleet/dispatch", graphDispatch("g-patch-ok", patchGraph, ""), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("a node that holds both must accept, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An input file does not travel with a graph (the dispatch door carries JSON only), so a LoadImage the node does not hold
// is refused the same way, and the refusal says why the delegator cannot just send it.
func TestRunGraphMissingInputImageIs503(t *testing.T) {
	cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors")
	tmp := isolatedTemp(t)
	runner := &fakeRunner{}
	s, jobs := newTestServer(t, cfg, runner, nil)

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-img", refGraph, ""), nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "ref.png")
	if !strings.Contains(rec.Body.String(), "do not travel") {
		t.Errorf("the refusal must say input files do not travel with a graph: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "high.safetensors") {
		t.Errorf("the weight file is held and must not be named: %s", rec.Body.String())
	}
	if queued, running := jobs.Counts(); queued+running != 0 || len(runner.requests()) != 0 {
		t.Errorf("a refused dispatch creates no job and runs nothing")
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Errorf("a refused dispatch left temp files behind: %v", left)
	}

	// The node that holds the image under its input directory takes it.
	cfg2 := graphNode(t, "models/diffusion_models/wan/high.safetensors", "input/ref.png")
	s2, _ := newTestServer(t, cfg2, &fakeRunner{}, nil)
	if rec := do(t, s2, http.MethodPost, "/fleet/dispatch", graphDispatch("g-img-ok", refGraph, ""), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("a node that holds the input image must accept, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Missing weights and a missing image are one refusal that names both; a long list is cut with a count.
func TestRunGraphRefusalNamesEverythingMissingAndCutsALongList(t *testing.T) {
	cfg := graphNode(t)
	_, cleanup, err := BuildRequest(t.Context(), cfg, true, "run-graph", []byte(`{"graph":`+refGraph+`}`))
	defer cleanup()
	var nre *routeNotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("want the re-placeable route-not-ready refusal, got %v", err)
	}
	for _, want := range []string{"wan/high.safetensors", "ref.png"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %v", want, err)
		}
	}

	// A graph of n lora loaders, none of which this node holds.
	loras := func(n int) []byte {
		var sb strings.Builder
		sb.WriteString(`{"graph":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`"` + string(rune('a'+i)) + `":{"class_type":"LoraLoader","inputs":{"lora_name":"l` + string(rune('a'+i)) + `.safetensors"}}`)
		}
		sb.WriteString("}}")
		return []byte(sb.String())
	}
	// Up to six missing files are listed and counted exactly...
	_, cleanup2, err := BuildRequest(t.Context(), cfg, true, "run-graph", loras(6))
	defer cleanup2()
	if err == nil || !strings.Contains(err.Error(), "6 files") || !strings.Contains(err.Error(), "lf.safetensors") || strings.Contains(err.Error(), "more") {
		t.Fatalf("six missing files are all named and counted: %v", err)
	}
	// ...and past six the node names the first six and says "more": it stops looking at the seventh (the door is open to any
	// peer and a graph can name thousands of files), so it does not know how many there are.
	_, cleanup3, err := BuildRequest(t.Context(), cfg, true, "run-graph", loras(9))
	defer cleanup3()
	if err == nil || !strings.Contains(err.Error(), "la.safetensors") || !strings.Contains(err.Error(), "lf.safetensors") ||
		!strings.Contains(err.Error(), "7 or more files") || !strings.Contains(err.Error(), "lf.safetensors and more") ||
		strings.Contains(err.Error(), "lg.safetensors") || strings.Contains(err.Error(), "li.safetensors") || strings.Contains(err.Error(), "and 3 more") {
		t.Fatalf("a refusal lists the first six and says there are more, with no count it did not take: %v", err)
	}
}

// A model the graph's manifest tells the node to fetch (a models[] entry with a source_url) is not missing: the satisfier
// downloads it before it starts ComfyUI. An entry with no source_url is not provisioned by anyone and stays refused.
func TestRunGraphAFileTheManifestFetchesIsNotMissing(t *testing.T) {
	cfg := graphNode(t, "models/text_encoders/umt5.safetensors", "models/vae/wan_vae.safetensors")
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	fetched := `{"models":[{"path":"models/diffusion_models/wan/high.safetensors","source_url":"https://example.invalid/high.safetensors"}]}`
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-man-ok", wanGraph, fetched), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("a file the manifest fetches must not be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	noSource := `{"models":[{"path":"models/diffusion_models/wan/high.safetensors"}]}`
	wantErrorShape(t, do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-man-no", wanGraph, noSource), nil),
		http.StatusServiceUnavailable, "wan/high.safetensors")
}

// A name is data from a tailnet peer: one that climbs out of the model roots or names an absolute path is looked up
// nowhere, so it reads as missing (and is named) even when the file it points at exists.
func TestRunGraphNamesThatLeaveTheModelRootsAreMissing(t *testing.T) {
	cfg := graphNode(t, "models/vae/ok.safetensors")
	outside := filepath.Join(filepath.Dir(cfg.ComfyDir), "outside.safetensors")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })
	for _, name := range []string{"../../outside.safetensors", filepath.ToSlash(outside)} {
		graph := `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"` + name + `"}}}`
		_, cleanup, err := BuildRequest(t.Context(), cfg, true, "run-graph", []byte(`{"graph":`+graph+`}`))
		cleanup()
		var nre *routeNotReadyError
		if !errors.As(err, &nre) {
			t.Errorf("%q: want a route-not-ready refusal, got %v", name, err)
		}
	}
}

// What the check does not read is unchanged: a graph that names no file is admitted by a node with no models root at all,
// and a payload that is not a valid graph keeps its 400.
func TestRunGraphThatNamesNoFileIsAdmittedAsBefore(t *testing.T) {
	cfg := fullCfg()
	cfg.ComfyDir = ""
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-plain", `{"1":{"class_type":"KSampler","inputs":{"seed":1,"model":["2",0]}}}`, ""), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("a graph that names no file must be admitted, got %d: %s", rec.Code, rec.Body.String())
	}
	body := `{"job_id":"g-bad","task_type":"run-graph","payload":{"graph":[1]}}`
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("a graph that is not an object keeps its 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The refusal says WHICH kind of 503 it is. A graph that names a file the node does not hold is "not this request": the node is
// not full and its route is fine, so the body carries refusal "not_capable" and a delegator marks the node not-capable for that
// ticket alone, with no pause on the node. A node whose run-graph route is not CONFIGURED is the node's own state and says no
// such thing, and neither does a full queue (media_ready_test.go, server_test.go): those delegators do leave alone for a while.
func TestAGraphFileRefusalCarriesTheNotCapableWord(t *testing.T) {
	cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors")
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	for name, body := range map[string]string{
		"a weight file": graphDispatch("g-w", wanGraph, ""),
		"an input file": graphDispatch("g-i", refGraph, ""),
	} {
		rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body.String())
		}
		if got := decodeMap(t, rec)["refusal"]; got != RefusalNotCapable {
			t.Errorf("%s: refusal = %v, want %q (body %s)", name, got, RefusalNotCapable, rec.Body.String())
		}
	}
}

func TestARouteThatIsNotReadyIsNotSaidToBeNotCapable(t *testing.T) {
	useRealMediaRoutes(t)
	script := filepath.Join(t.TempDir(), "comfy-run-graph.mjs")
	touchFile(t, script)
	cfg := graphNode(t, "models/diffusion_models/wan/high.safetensors")
	cfg.RunGraphScript = script
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	ResetMediaRoutesCache()
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", graphDispatch("g-r", wanGraph, ""), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a bound run-graph whose route is not ready: status %d: %s", rec.Code, rec.Body.String())
	}
	if got, has := decodeMap(t, rec)["refusal"]; has {
		t.Errorf("a route that is not CONFIGURED is the node's state, not a refusal of this graph: refusal = %v", got)
	}
}
