package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Cross-platform lint: the harness ships to Windows AND Linux nodes, and every
// install defect found on the 2026-07-27 fleet deploy was the same mistake — shared
// code that only fits the machine it was written on:
//
//   - the ComfyUI venv python was probed at Windows paths only, so a Linux node could
//     not launch ComfyUI at all;
//   - run-graph hardcoded .venv/Scripts/python.exe with no candidate list;
//   - comfy_dir defaulted to "C:/ComfyUI" on every OS, so a Linux node advertised
//     ComfyUI-backed fleet tasks that fail on arrival.
//
// Each was invisible until someone ran the route on the other OS. These tests make the
// class fail in CI instead. They check SHARED code only — platform-suffixed files
// (_windows.go / _linux.go) and tests are exempt by design.

// TestNoWindowsOnlyInterpreterProbe: a runner that probes a Windows venv interpreter
// must probe the POSIX one too. render/tts.mjs and internal/mediaops both did this
// correctly long before comfy-lifecycle.mjs did; this keeps the next runner honest.
func TestNoWindowsOnlyInterpreterProbe(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("render", "*.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, ".test.mjs") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, "Scripts/python.exe") {
			continue
		}
		if !strings.Contains(src, "bin/python") {
			t.Errorf("%s probes a Windows venv interpreter (Scripts/python.exe) but no POSIX one "+
				"(bin/python) — that is exactly how ComfyUI became unlaunchable on Linux nodes", path)
		}
	}
}

// TestNoUnguardedDriveLetterDefaults: a drive-letter literal is Windows-specific, so a
// shared file may only carry one when it also branches on runtime.GOOS. config.go's
// "C:/ComfyUI" is legal because DefaultComfyDir guards it; a new unguarded one is the
// comfy_dir bug returning under a different field name.
func TestNoUnguardedDriveLetterDefaults(t *testing.T) {
	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Vendored/build dirs carry third-party paths that are not ours to police.
			if name := info.Name(); name == ".git" || name == "bin" || name == "testdata" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") ||
			strings.HasSuffix(name, "_windows.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(raw)
		guarded := strings.Contains(src, "runtime.GOOS")
		for i, line := range strings.Split(src, "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx] // a drive letter in prose is documentation, not behavior
			}
			if !containsDriveLiteral(code) || guarded {
				continue
			}
			offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+" "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("Windows drive-letter literal in shared code with no runtime.GOOS branch:\n  %s\n"+
			"Guard it like config.DefaultComfyDir, or the other OS inherits a path that cannot exist.",
			strings.Join(offenders, "\n  "))
	}
}

// windowsOnlySeedGrandfathered are the tier seeds that still carry a Windows-only
// binary name. They are NOT accepted — they are recorded, with the reason, so a NEW
// one fails immediately while these two are fixed by the tier-schema work (W4 of the
// install-architecture plan: per-OS binary names rendered by the installer). Removing
// an entry here is part of that change; adding one requires the same justification.
var windowsOnlySeedGrandfathered = map[string]string{
	"amd-rdna3":      "sdcpp_bin sd-cli.exe — Juan's tier; needs per-OS binary rendering (W4)",
	"amd-rdna3-dgpu": "sdcpp_bin sd-cli.exe — same seed shape as amd-rdna3 (W4)",
}

// TestTierSeedsCarryNoWindowsOnlyBinary: a tier is a HARDWARE class, not an operating
// system. A seed that names sd-cli.exe cannot be rendered on a Linux box of the same
// tier, which is how "first-class on every node" quietly becomes "first-class on mine".
func TestTierSeedsCarryNoWindowsOnlyBinary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("no profiles parsed — the schema moved and this gate went blind")
	}
	for name, p := range doc.Profiles {
		for key, val := range p.ConfigSeed {
			if !strings.Contains(strings.ToLower(string(val)), ".exe") {
				continue
			}
			if why, known := windowsOnlySeedGrandfathered[name]; known {
				t.Logf("known Windows-only seed %s.%s (%s)", name, key, why)
				continue
			}
			t.Errorf("tier %q seeds %s = %s — a .exe in a tier seed cannot render on a "+
				"non-Windows box of the same hardware tier", name, key, string(val))
		}
	}
}

func containsDriveLiteral(s string) bool {
	for _, drive := range []string{`"C:/`, `"C:\\`, `"D:/`, `"D:\\`} {
		if strings.Contains(s, drive) {
			return true
		}
	}
	return false
}
