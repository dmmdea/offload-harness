package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Findings of the review of `install vllm-seat` for windows-wsl seats. They use the synthetic
// distro and the helpers of install_vllmseat_wsl_test.go.

// The seat env is read by bash in the distro (`. "$CFG"`): a CR before each newline is part
// of every value. envLines (above) trims it, so the tests there could not see it.
func TestInstallVLLMSeatWritesTheSeatEnvWithLFLineEndings(t *testing.T) {
	r := newWSLBox(t, "blackwell-3x16").run(t, nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	raw, err := os.ReadFile(filepath.Join(r.out, "qwen3.8-27b-vllm.env"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "\r"); n != 0 {
		t.Errorf("the rendered seat env carries %d CR byte(s); bash inside the distro would read SEAT_PORT as \"18797\\r\"", n)
	}
}

// --venv and --model-path were checked by the prerequisite probe and written nowhere:
// seat_fg.sh then ran <seat dir>/vllm-env and the hard-coded repo id, so the probe vouched
// for a venv and weights the seat never used. The env now carries both.
func TestInstallVLLMSeatRendersTheVenvAndTheModelItChecked(t *testing.T) {
	tier := "blackwell-3x16"
	repo := committedLaneSeat(t, tier).ModelRepo
	b := newWSLBox(t, tier)
	putScript(t, filepath.ToSlash(filepath.Join(b.root, "opt", "other-venv", "bin", "vllm")))

	r := b.run(t, map[string]string{"-venv": "/opt/other-venv", "-wsl-seat-dir": "/srv/seat"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	env := envLines(readRendered(t, r.out, "qwen3.8-27b-vllm.env"))
	if got := env["SEAT_VENV"]; got != "/opt/other-venv" {
		t.Errorf("SEAT_VENV = %q, want the venv the check looked in, /opt/other-venv", got)
	}
	if got, want := env["SEAT_MODEL"], "/hf/"+repo+"/snapshots/abc"; got != want {
		t.Errorf("SEAT_MODEL = %q, want the snapshot the check resolved, %q", got, want)
	}

	// A second snapshot makes the repo ambiguous; pinning one with --model-path is the way out,
	// and the pinned path (not whichever sorts first) is what the seat is told to serve.
	snap := filepath.Join(b.root, "hf", filepath.FromSlash(repo), "snapshots", "zzz")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(r.out); err != nil { // the refusal below must be judged against an empty directory
		t.Fatal(err)
	}
	mustRefuse(t, b.run(t, nil), "two snapshots and no --model-path", "snapshots")
	pin := "/hf/" + repo + "/snapshots/zzz"
	r = b.run(t, map[string]string{"-model-path": pin})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := envLines(readRendered(t, r.out, "qwen3.8-27b-vllm.env"))["SEAT_MODEL"]; got != pin {
		t.Errorf("SEAT_MODEL = %q, want the pinned snapshot %q", got, pin)
	}
}

// The model path is what the engine opens, so it is a distro path like every other path in
// the env. A directory that happens to exist on the Windows side would pass the probe by
// accident and be written into an env bash cannot open.
func TestInstallVLLMSeatNeedsTheModelPathAsADistroPath(t *testing.T) {
	b := newWSLBox(t, "blackwell-3x16")
	mustRefuse(t, b.run(t, map[string]string{"-model-path": "C:/models/qwen/snapshots/abc"}), "a Windows --model-path", "--model-path", "INSIDE the distro")
}

// Each path flag's refusal gives an example of ITS OWN path. The one text used to end every
// time with "(for example /hf)", which sent an operator who left out --venv to pass /hf.
func TestInstallVLLMSeatGivesEachPathFlagItsOwnExample(t *testing.T) {
	b := newWSLBox(t, "blackwell-3x16")
	t.Setenv("HF_HOME", "")
	for _, tc := range []struct {
		over       map[string]string
		flag, want string
	}{
		{map[string]string{"-venv": ""}, "--venv", "/opt/seat/vllm-env"},
		{map[string]string{"-hf-home": ""}, "--hf-home", "/hf"},
		{map[string]string{"-wsl-seat-dir": "C:/seat"}, "--wsl-seat-dir", "/opt/seat"},
		{map[string]string{"-lmcache-overlay": "C:/overlay"}, "--lmcache-overlay", "/opt/seat/lmcache-overlay"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			r := b.run(t, tc.over)
			mustRefuse(t, r, "a missing or Windows "+tc.flag, tc.flag)
			if !strings.Contains(r.err.Error(), "for example "+tc.want+")") {
				t.Errorf("the refusal must give %s as the example for %s, got %v", tc.want, tc.flag, r.err)
			}
			if tc.flag != "--hf-home" && strings.Contains(r.err.Error(), "for example /hf)") {
				t.Errorf("the refusal of %s still gives the HF cache's example: %v", tc.flag, r.err)
			}
		})
	}
}

// A prefix that cannot be read looked exactly like a missing venv: the operator was told to
// build the venv and fetch the weights, and the hint to check --host-prefix appeared only when
// no prefix had been given at all.
func TestInstallVLLMSeatPointsAtTheHostPrefixWhenTheProbeWentThroughIt(t *testing.T) {
	b := newWSLBox(t, "blackwell-3x16")

	typo := filepath.ToSlash(filepath.Join(b.home, "no-such-distro"))
	r := b.run(t, map[string]string{"-host-prefix": typo})
	mustRefuse(t, r, "a --host-prefix that cannot be read", "--host-prefix", typo)
	if strings.Contains(r.err.Error(), "Build the vLLM venv") {
		t.Errorf("an unreadable prefix is not a missing venv, yet the refusal sends the operator to build one: %v", r.err)
	}

	// A readable prefix whose distro lacks the venv IS the venv's fault, and the refusal still
	// says to check the prefix, because it is the one thing the operator typed that the probe
	// trusted.
	if err := os.Remove(filepath.Join(b.root, "opt", "seat", "vllm-env", "bin", "vllm")); err != nil {
		t.Fatal(err)
	}
	r = b.run(t, nil)
	mustRefuse(t, r, "a distro without the venv", "--host-prefix", "vllm entry point")
}

// --host-prefix reads one distro's filesystem and --distro names the one the tasks run in.
// When the prefix is a \\wsl.localhost\<name> (or \\wsl$\<name>) share, the two names must
// agree: a prefix for another distro vouches for a venv and weights the seat will not find.
func TestInstallVLLMSeatRefusesAHostPrefixForAnotherDistro(t *testing.T) {
	b := newWSLBox(t, "blackwell-3x16")
	for _, prefix := range []string{`//wsl.localhost/other`, `\\wsl.localhost\other`, `\\wsl$\other`, `//wsl.localhost/other/`} {
		t.Run(prefix, func(t *testing.T) {
			mustRefuse(t, b.run(t, map[string]string{"-distro": "distro", "-host-prefix": prefix}),
				"a --host-prefix naming another distro", "--host-prefix", "--distro", "other")
		})
	}

	// The same distro, under any spelling the share allows, is not a mismatch; neither is a
	// prefix that is a plain directory (a test box, a mounted copy), which names no distro.
	seat := committedLaneSeat(t, "blackwell-3x16")
	for _, prefix := range []string{`//wsl.localhost/distro`, `\\WSL.LOCALHOST\Distro`, `\\wsl$\distro\`, `/mnt/any/directory`} {
		f := vllmRuntimeFlags{distro: "distro", venv: "/opt/seat/vllm-env", hfHome: "/hf", hostPrefix: prefix,
			lmcacheOverlay: wslTestOverlay}
		if err := f.check("blackwell-3x16", seat); err != nil {
			t.Errorf("--host-prefix %s for --distro distro was refused: %v", prefix, err)
		}
	}
}

// --cache-mount-src/--cache-mount-opts are read only for a seat that has a cache server. On a
// storeless seat the tokens render empty, and the operator who believes the share is mounted
// gets a same-box seat with no word that the flags were discarded.
func TestInstallVLLMSeatRefusesMountFlagsForASeatWithNoCacheServer(t *testing.T) {
	b := newWSLBox(t, "blackwell-3x16")
	cfg := writeConfig(t, `{"vllm_seats":["qwen3.8-27b-vllm"],"kv_cache_server":[{"seat":"qwen3.8-27b-vllm","storeless":true,"reason":"same-box tier only"}]}`)
	for flag, val := range map[string]string{"-cache-mount-src": "//store.example/kvcache", "-cache-mount-opts": "vers=3.1.1"} {
		t.Run(flag, func(t *testing.T) {
			mustRefuse(t, b.run(t, map[string]string{"-config": cfg, flag: val}),
				"a mount flag for a storeless seat", "-"+flag, "cache_server")
		})
	}
	if r := b.run(t, map[string]string{"-config": cfg}); r.err != nil {
		t.Errorf("a storeless seat with no mount flags must render: %v", r.err)
	}
}

// A seat that starts through systemd has no LMCache wiring in its unit, so an fp8 KV seat
// bound to a store cannot be rendered by this command at all (Artifacts refused it before
// the flag existed, and no flag can clear it now). The refusal says why, instead of asking for
// a flag that is then refused as stray.
func TestInstallVLLMSeatSaysWhyAnFp8StoreSeatCannotRenderThroughSystemd(t *testing.T) {
	cfg := writeConfig(t, `{"vllm_seats":["qwen38-27b-gsq-vllm"],"kv_cache_server":[{"seat":"qwen38-27b-gsq-vllm","enabled":true,"store":"fs_native","address":"/mnt/kvcache/lmcache-seat-gsq","chunk_size":784,"key_prefix":"seat-gsq-gen1"}]}`)
	r := runSeatCommand(t, "blackwell-16", "-config", cfg)
	mustRefuse(t, r, "an fp8 seat bound to a store, rendered for systemd", "windows-wsl", "4253")
	if r.err != nil && strings.Contains(r.err.Error(), "needs --lmcache-overlay") {
		t.Errorf("the refusal asks for a flag that this seat refuses: %v", r.err)
	}
}
