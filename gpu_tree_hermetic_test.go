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
	// Scratch means this test's own temp directory: a sibling of the one taken above. (os.TempDir() is not the yardstick: the
	// testing package may place t.TempDir() under GOTMPDIR, which is not TMPDIR.)
	if scratch := filepath.Dir(elsewhere); !isUnder(comfy, scratch) {
		t.Fatalf("comfy_dir %q is not a scratch directory (not under this test's temp directory %s)", comfy, scratch)
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

// The repin verb tests run a real `gpu reserve` too (TestHelperRepinDriver), and the same hazard applies to them: their driver
// inherits the parent's environment, where GPU_LOCK, LOCAL_OFFLOAD_STATE_DIR and LOCAL_OFFLOAD_CONFIG redirect the lease or the
// configuration away from the scratch root, and their fixture config names no comfy_dir, which resolves to the machine's real
// ComfyUI install on Windows, whose live launch markers the wrapper reads and stops at its end when one names the scratch
// lease's epoch (epochs of a fresh scratch lease start at 1). The driver must be unable to reach any of it, whatever the
// developer's shell holds, so it does not depend on the calling test remembering to clear it.
func TestRepinVerbTestsNeverReachTheMachinesLeaseConfigOrComfyInstall(t *testing.T) {
	isolateFromMachineState(t)
	cfg, _ := scopedLeaseFixture(t)
	// A developer shell that has the redirects set, whatever the test did before building its driver.
	elsewhere := t.TempDir()
	for _, name := range machineStateRedirects {
		t.Setenv(name, filepath.Join(elsewhere, name))
	}

	cmd, _ := repinDriverCmd(t, cfg, []string{"--devices", "0"}, []string{"COMFY_CUDA_DEVICE=2"})
	for _, kv := range cmd.Env {
		name, value, _ := strings.Cut(kv, "=")
		for _, bad := range machineStateRedirects {
			if strings.EqualFold(name, bad) && value != "" {
				t.Errorf("the repin driver inherits %s=%s, which redirects the lease or the config away from scratch", name, value)
			}
		}
	}
	var sawPin bool
	for _, kv := range cmd.Env {
		sawPin = sawPin || kv == "COMFY_CUDA_DEVICE=2"
	}
	if !sawPin {
		t.Error("the driver drops the variables the test passes to it")
	}

	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the driver's config is not JSON: %v\n%s", err, raw)
	}
	comfy, _ := parsed["comfy_dir"].(string)
	if comfy == "" {
		t.Fatalf("the driver's config names no comfy_dir, so the wrapper resolves it to the machine's real ComfyUI install: %s", raw)
	}
	if sameDir(comfy, config.DefaultComfyDir()) {
		t.Fatalf("comfy_dir %q is the machine's real ComfyUI install", comfy)
	}
	if scratch := filepath.Dir(elsewhere); !isUnder(comfy, scratch) {
		t.Fatalf("comfy_dir %q is not a scratch directory (not under this test's temp directory %s)", comfy, scratch)
	}
	if parsed["gpu_card_scoped_leases"] != true {
		t.Errorf("the fixture's own settings are kept: %s", raw)
	}
}

// Operator order: no process a test starts may show a window. The real-wrapper tests re-execute the test binary as the wrapper, the
// wrapped command and its grandchild; on Windows a console child of a parent with no console opens its own window unless it
// is created window-less, and its descendants inherit what it got. Every spawn in these files hides its child, named by the
// variable that holds it.
func TestRealWrapperTestSpawnsAreWindowless(t *testing.T) {
	for _, file := range []string{"gpu_tree_test.go", "gpu_reserve_repin_test.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		for i, ln := range lines {
			idx := strings.Index(ln, "exec.Command(")
			if idx < 0 || strings.HasPrefix(strings.TrimSpace(ln), "//") {
				continue
			}
			head := strings.TrimSpace(ln[:idx])
			name := strings.TrimSpace(strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(head, ":="), "="), " "))
			if name == "" {
				t.Errorf("%s:%d: an exec.Command whose child is not held in a variable cannot be hidden: %s", file, i+1, strings.TrimSpace(ln))
				continue
			}
			end := i + 8
			if end > len(lines) {
				end = len(lines)
			}
			if !strings.Contains(strings.Join(lines[i:end], "\n"), "hideWindow("+name+")") {
				t.Errorf("%s:%d: %s is started without hideWindow(%s): it can flash a console window on Windows", file, i+1, name, name)
			}
		}
	}
}
