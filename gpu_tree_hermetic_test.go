package main

// SAFETY, pinned (plan P10): a real wrapper test must be unable to reach the machine's real lease
// directory, configuration or ComfyUI install. The wrapper reads launch markers from the config's
// comfy_dir, and a config that names none resolves to the machine's real install on Windows
// (config.DefaultComfyDir): the wrapper would read the live markers there, and stop a live
// instance whose marker happened to name the epoch of the scratch lease it holds. The wrapper also
// inherits its parent's environment, where GPU_LOCK, LOCAL_OFFLOAD_STATE_DIR and
// LOCAL_OFFLOAD_CONFIG each redirect the lease or the config to somewhere that is not scratch, and
// the test's own lease Manager honours GPU_LOCK too: with it set, the test's ReleaseByEpoch would
// act on whatever lease that path holds.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestRealWrapperTestsNeverReachTheMachinesLeaseConfigOrComfyInstall(t *testing.T) {
	// A developer shell that has the redirects set.
	elsewhere := t.TempDir()
	for _, name := range machineStateRedirects {
		t.Setenv(name, filepath.Join(elsewhere, name))
	}

	isolateFromMachineState(t)
	cfgPath, m := leaseFixture(t)
	// The scratch root the fixture's own config names: the Manager must resolve inside it (the
	// Manager's own Root() is derived from what it resolved, so it cannot be the yardstick).
	var fixture struct {
		StateDir string `json:"state_dir"`
	}
	fixtureRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixtureRaw, &fixture); err != nil || fixture.StateDir == "" {
		t.Fatalf("the fixture config names no state_dir (%v): %s", err, fixtureRaw)
	}
	if !isUnder(m.Dir(), fixture.StateDir) {
		t.Fatalf("the test's lease Manager resolves %s, outside its scratch state root %s: a redirect in the environment reached it", m.Dir(), fixture.StateDir)
	}

	named := writeTreeConfig(t, cfgPath, m, "")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the config is not JSON: %v\n%s", err, raw)
	}
	comfy, _ := cfg["comfy_dir"].(string)
	if comfy == "" {
		t.Fatalf("the default config of a real-wrapper test names no comfy_dir, so the wrapper resolves it to the machine's real ComfyUI install: %s", raw)
	}
	if named != comfy {
		t.Errorf("writeTreeConfig reports comfy_dir %q but wrote %q", named, comfy)
	}
	if sameDir(comfy, config.DefaultComfyDir()) {
		t.Fatalf("comfy_dir %q is the machine's real ComfyUI install", comfy)
	}
	if !isUnder(comfy, os.TempDir()) {
		t.Fatalf("comfy_dir %q is not a scratch directory (not under %s)", comfy, os.TempDir())
	}

	var sawExtra bool
	for _, kv := range treeEnv("LO_EXTRA=1") {
		name, value, _ := strings.Cut(kv, "=")
		for _, bad := range machineStateRedirects {
			if strings.EqualFold(name, bad) && value != "" {
				t.Errorf("the wrapper inherits %s=%s, which redirects the lease or the config away from scratch", name, value)
			}
		}
		if kv == "LO_EXTRA=1" {
			sawExtra = true
		}
	}
	if !sawExtra {
		t.Error("treeEnv drops the variables the test passes to the wrapper")
	}
}

func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.ToSlash(filepath.Clean(a)), filepath.ToSlash(filepath.Clean(b)))
}

func isUnder(p, dir string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.ToSlash(filepath.Clean(p))), strings.ToLower(filepath.ToSlash(filepath.Clean(dir)))+"/")
}
