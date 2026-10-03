package gpugen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

const instanceCard = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"

// TestInstanceEnv: a Spec that names a per-card ComfyUI instance hands the runner its
// endpoint and card; a card pin by uuid also blanks the legacy index (the runner refuses
// both). A Spec that names neither adds nothing, which is every caller today.
func TestInstanceEnv(t *testing.T) {
	if got := instanceEnv(Spec{}); len(got) != 0 {
		t.Fatalf("an unbound spec must add no env, got %v", got)
	}
	if got, want := instanceEnv(Spec{ComfyAPI: "http://localhost:8189"}), []string{"COMFY_API=http://localhost:8189"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("api-only env = %v, want %v", got, want)
	}
	got := instanceEnv(Spec{ComfyAPI: "http://localhost:8189", CardUUID: instanceCard})
	want := []string{"COMFY_API=http://localhost:8189", "COMFY_CARD_UUID=" + instanceCard, "COMFY_CUDA_DEVICE="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

// TestGenerateChildEnvCarriesInstance: the entries reach the child process, in the
// inheriting mode and in the exact-env (allowlist) mode alike, and a later entry in the
// spec's own Env cannot shadow them. The post-run /free goes to the instance's endpoint.
func TestGenerateChildEnvCarriesInstance(t *testing.T) {
	requireNode(t)
	var frees atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/free" {
			frees.Add(1)
		}
	}))
	defer srv.Close()
	dump := `require('fs').writeFileSync(process.argv[1], JSON.stringify(process.env))`
	keep := []string{"COMFY_CUDA_DEVICE=2", "COMFY_API=http://stale.invalid:1"}
	for _, k := range []string{"PATH", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			keep = append(keep, k+"="+v)
		}
	}
	for _, exact := range []bool{false, true} {
		out := filepath.Join(t.TempDir(), "env.json")
		if _, err := Generate(context.Background(), Spec{Exe: "node", Script: "-e", Args: []string{dump, out},
			Env: keep, EnvExact: exact, Out: out, Timeout: 10 * time.Second, ComfyAPI: srv.URL, CardUUID: instanceCard}); err != nil {
			t.Fatalf("Generate(exact=%v): %v", exact, err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var env map[string]string
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		if env["COMFY_API"] != srv.URL || env["COMFY_CARD_UUID"] != instanceCard || env["COMFY_CUDA_DEVICE"] != "" {
			t.Fatalf("exact=%v: child env api/card/index = %q / %q / %q", exact, env["COMFY_API"], env["COMFY_CARD_UUID"], env["COMFY_CUDA_DEVICE"])
		}
	}
	if got := frees.Load(); got != 2 {
		t.Fatalf("the post-run /free hit the instance endpoint %d time(s), want 2 (one per run)", got)
	}
}

// TestGenerateUnboundSpecAddsNoInstanceEnv: with neither field set the child's env is
// exactly what the caller built (EnvExact makes that observable).
func TestGenerateUnboundSpecAddsNoInstanceEnv(t *testing.T) {
	requireNode(t)
	dump := `require('fs').writeFileSync(process.argv[1], JSON.stringify(process.env))`
	keep := []string{}
	for _, k := range []string{"PATH", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			keep = append(keep, k+"="+v)
		}
	}
	out := filepath.Join(t.TempDir(), "env.json")
	if _, err := Generate(context.Background(), Spec{Exe: "node", Script: "-e", Args: []string{dump, out},
		Env: keep, EnvExact: true, Out: out, Timeout: 10 * time.Second, SkipFreeComfy: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	var env map[string]string
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"COMFY_API", "COMFY_CARD_UUID", "COMFY_CUDA_DEVICE"} {
		if _, ok := env[k]; ok {
			t.Fatalf("an unbound spec exported %s", k)
		}
	}
}
