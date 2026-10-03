package imagegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// A synthetic card id (a repeated-nibble head is a placeholder to the tree's shape gate).
const instanceCard = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"

// TestComfyLaunchEnvCarriesAPIAndCard: a bound per-card instance hands the runner its
// endpoint and its card by uuid. The two entries exist only when set, so an unbound
// launch keeps its exact two-entry env (TestComfyLaunchEnv). A card pin by uuid replaces
// the legacy index: the runner refuses a launch that carries both, so the index is
// blanked here rather than trusted to the caller.
func TestComfyLaunchEnvCarriesAPIAndCard(t *testing.T) {
	got := ComfyLaunch{DynamicVRAM: "on", ExtraArgs: "--verbose", API: "http://localhost:8189", CardUUID: instanceCard}.Env()
	want := []string{"COMFY_CUDA_DEVICE=", "COMFY_DYNAMIC_VRAM=on", "COMFY_EXTRA_ARGS=--verbose",
		"COMFY_API=http://localhost:8189", "COMFY_CARD_UUID=" + instanceCard}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	// An index set alongside a card is blanked: the uuid wins.
	pinned := ComfyLaunch{CudaDevice: "2", CardUUID: instanceCard}.Env()
	if pinned[0] != "COMFY_CUDA_DEVICE=" {
		t.Fatalf("a card pin must blank the legacy index, got %v", pinned)
	}
	// API alone leaves the legacy pin path exactly as it was.
	apiOnly := ComfyLaunch{CudaDevice: "2", API: "http://localhost:8190"}.Env()
	if want := []string{"COMFY_CUDA_DEVICE=2", "COMFY_DYNAMIC_VRAM=", "COMFY_API=http://localhost:8190"}; !reflect.DeepEqual(apiOnly, want) {
		t.Fatalf("api-only env = %v, want %v", apiOnly, want)
	}
	// Unbound: neither entry.
	for _, e := range (ComfyLaunch{CudaDevice: "1"}).Env() {
		if e == "COMFY_API=" || e == "COMFY_CARD_UUID=" {
			t.Fatalf("an unbound launch must not export %q", e)
		}
	}
}

// dumpScript writes the child's environment as JSON to the --results path when the argv
// has one (the batch runner), else to the first argument (every other runner).
func dumpScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dump.js")
	js := `const a = process.argv.slice(2);
const i = a.indexOf('--results');
require('fs').writeFileSync(i >= 0 ? a[i + 1] : a[0], JSON.stringify(process.env));
`
	if err := os.WriteFile(p, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestComfyLaunchReachesEveryRunnerAndTheFree: for the image, edit and batch runners the
// instance's endpoint and card reach the child process env AND the post-run /free is sent
// to the endpoint of the instance that ran (not the default one).
func TestComfyLaunchReachesEveryRunnerAndTheFree(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; the runner-env test needs the verified toolchain")
	}
	var frees atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/free" {
			frees.Add(1)
		}
	}))
	defer srv.Close()
	launch := ComfyLaunch{CudaDevice: "2", API: srv.URL, CardUUID: instanceCard}
	script := dumpScript(t)
	read := func(path string) map[string]string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]string
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	check := func(name string, env map[string]string, freesBefore int32) {
		t.Helper()
		if env["COMFY_API"] != srv.URL || env["COMFY_CARD_UUID"] != instanceCard {
			t.Errorf("%s: child env api/card = %q / %q", name, env["COMFY_API"], env["COMFY_CARD_UUID"])
		}
		if env["COMFY_CUDA_DEVICE"] != "" {
			t.Errorf("%s: a card-bound launch must not carry an index, got %q", name, env["COMFY_CUDA_DEVICE"])
		}
		if got := frees.Load(); got != freesBefore+1 {
			t.Errorf("%s: /free hit the instance %d time(s), want exactly 1", name, got-freesBefore)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()

	out := filepath.Join(dir, "gen.json")
	if _, err := Generate(ctx, "node", script, dir, out, "p", nil, Model{Launch: launch}, 20*time.Second, nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	check("Generate", read(out), 0)

	before := frees.Load()
	out = filepath.Join(dir, "edit.json")
	if _, err := Edit(ctx, "node", script, dir, out, "in.png", "e", nil, EditModel{Launch: launch}, 20*time.Second); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	check("Edit", read(out), before)

	before = frees.Load()
	results := filepath.Join(dir, "batch.json")
	if err := GenerateBatch(ctx, "node", script, dir, filepath.Join(dir, "jobs.jsonl"), results, Model{Launch: launch}, 20*time.Second); err != nil {
		t.Fatalf("GenerateBatch: %v", err)
	}
	check("GenerateBatch", read(results), before)
}
