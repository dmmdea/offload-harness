package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// `install vllm-seat` rendered the linux-systemd artifact set for EVERY tier, whatever the
// tier's launch: for the two Blackwell workstation tiers (launch windows-wsl) it read a template
// directory the seat does not use, and the box's seat env was left to be maintained by hand. The
// docs promised `install vllm-seat --config ... renders into seat.env`; no shipped command
// could. These tests run the real command over the committed tier table with a synthetic box.

const wslTestOverlay = "/opt/seat/lmcache-overlay"

// committedLaneSeat is a tier's vllm_seat as the command reads it.
func committedLaneSeat(t *testing.T, tier string) vllmseat.Spec {
	t.Helper()
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	p, ok := doc.Profiles[tier]
	if !ok || p.VLLMSeat == nil {
		t.Fatalf("tier %s declares no vllm_seat", tier)
	}
	return *p.VLLMSeat
}

// wslRun is one invocation of `install vllm-seat` and what came out of it.
type wslRun struct {
	out    string // the --out directory
	stdout string
	err    error
}

// runSeatCommand runs the command for a tier on a synthetic box whose venv and weights are
// real directories under a temp dir (so the prerequisite check passes without --force and
// without a host prefix) and returns what it wrote and printed. extra flags are appended.
func runSeatCommand(t *testing.T, tier string, extra ...string) wslRun {
	t.Helper()
	seat := committedLaneSeat(t, tier)
	box := newFakeBox(t).withVenv(t).withWeights(t, seat.ModelRepo)
	r := wslRun{out: box.home + "/rendered"}
	args := append([]string{"-profile", tier, "-root", ".", "-home", box.home, "-out", r.out,
		"-user", "svcuser", "-proxy-host", "127.0.0.1", "-venv", box.venv, "-hf-home", box.hf}, extra...)
	captureStderr(t, func() {
		r.stdout = captureStdout(t, func() { r.err = runInstallVLLMSeat(args) })
	})
	return r
}

// renderedFiles lists what the command wrote, relative to --out, slash form, sorted.
func renderedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readRendered(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// envLines are the assignments of a rendered seat env.
func envLines(env string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(env, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') && v[n-1] == v[0] {
				v = v[1 : n-1]
			}
			out[k] = v
		}
	}
	return out
}

// The templates directory follows the seat's launch (Spec.TemplatesDir), which had no caller
// outside tests. A windows-wsl seat writes its env file and the four Windows scripts and no
// systemd unit; the 3-card tier's shipped chat template rides along under templates/.
func TestInstallVLLMSeatChoosesTemplatesDirByLaunch(t *testing.T) {
	for tier, want := range map[string][]string{
		"blackwell-2x16": {"hidden.vbs", "qwen3.8-27b-vllm.env", "register-seat-tasks.ps1", "seat-cmd.ps1", "seat-cmdstop.ps1"},
		"blackwell-3x16": {"hidden.vbs", "qwen3.8-27b-vllm.env", "register-seat-tasks.ps1", "seat-cmd.ps1", "seat-cmdstop.ps1",
			"templates/qwen3-fold-system.jinja"},
	} {
		t.Run(tier, func(t *testing.T) {
			r := runSeatCommand(t, tier, "-distro", "distro", "-lmcache-overlay", wslTestOverlay)
			if r.err != nil {
				t.Fatalf("install vllm-seat --profile %s: %v", tier, r.err)
			}
			got := renderedFiles(t, r.out)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("wrote %v, want %v (a windows-wsl seat has no systemd unit, wrapper script or polkit rule)", got, want)
			}
		})
	}
}

// A windows-wsl render without --distro would name no distribution in the scheduled tasks. The
// refusal names the FLAG the operator can pass, not the Runtime field Artifacts names, and it
// comes before anything is written.
func TestInstallVLLMSeatRefusesWSLLaunchWithoutDistro(t *testing.T) {
	r := runSeatCommand(t, "blackwell-3x16", "-lmcache-overlay", wslTestOverlay)
	if r.err == nil {
		t.Fatalf("a windows-wsl render with no --distro was accepted; wrote %v", renderedFiles(t, r.out))
	}
	for _, want := range []string{"--distro", "windows-wsl"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("the refusal must name %q, got %v", want, r.err)
		}
	}
	if got := renderedFiles(t, r.out); len(got) != 0 {
		t.Errorf("a refused render wrote %v", got)
	}
}

// fp8 KV pages restore corrupt through stock LMCache and nothing reports it, so the pair
// (fp8 + a cache server) needs the patched overlay. Artifacts already refuses it; the command
// refuses it first, naming --lmcache-overlay. A seat bound storeless has no store round trip
// to corrupt and needs none.
func TestInstallVLLMSeatRefusesAnFp8CacheSeatWithoutAnOverlay(t *testing.T) {
	r := runSeatCommand(t, "blackwell-3x16", "-distro", "distro")
	if r.err == nil {
		t.Fatalf("fp8 KV with a cache server and no --lmcache-overlay was accepted; wrote %v", renderedFiles(t, r.out))
	}
	for _, want := range []string{"--lmcache-overlay", "4253"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("the refusal must name %q, got %v", want, r.err)
		}
	}
	if got := renderedFiles(t, r.out); len(got) != 0 {
		t.Errorf("a refused render wrote %v", got)
	}

	cfg := writeConfig(t, `{"vllm_seats":["qwen3.8-27b-vllm"],"kv_cache_server":[{"seat":"qwen3.8-27b-vllm","storeless":true,"reason":"same-box tier only"}]}`)
	r = runSeatCommand(t, "blackwell-3x16", "-distro", "distro", "-config", cfg)
	if r.err != nil {
		t.Errorf("a seat bound storeless has no L2 round trip and needs no overlay: %v", r.err)
	}
}

// The deployment flags reach the files: the overlay is the env's PYTHONPATH, the distro seat
// directory is where the chat template and the env are named, and the cache export is what
// the wrapper mounts. None of them may be hardened into the tier.
func TestInstallVLLMSeatAcceptsOverlayAndSeatDirFlags(t *testing.T) {
	r := runSeatCommand(t, "blackwell-3x16", "-distro", "distro", "-wsl-seat-dir", "/srv/seat",
		"-lmcache-overlay", "/srv/lmcache", "-cache-mount-src", "//store.example/kvcache", "-cache-mount-opts", "vers=3.1.1")
	if r.err != nil {
		t.Fatal(r.err)
	}
	env := envLines(readRendered(t, r.out, "qwen3.8-27b-vllm.env"))
	for k, want := range map[string]string{
		"SEAT_LMCACHE_PYTHONPATH": "/srv/lmcache",
		"SEAT_L2_MOUNT_SRC":       "//store.example/kvcache",
		"SEAT_L2_MOUNT_OPTS":      "vers=3.1.1",
		"SEAT_L2_STATUS_FILE":     "/srv/seat/seat-l2-qwen3.8-27b-vllm.status",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if !strings.HasSuffix(env["SEAT_EXTRA_ARGS"], "--chat-template /srv/seat/templates/qwen3-fold-system.jinja --enable-prompt-tokens-details") {
		t.Errorf("the chat template must be named at the distro seat directory the flag gave: %q", env["SEAT_EXTRA_ARGS"])
	}
	tasks := readRendered(t, r.out, "register-seat-tasks.ps1")
	for _, want := range []string{"-d distro", "/srv/seat/seat_fg.sh", "/srv/seat/qwen3.8-27b-vllm.env"} {
		if !strings.Contains(tasks, want) {
			t.Errorf("register-seat-tasks.ps1 lacks %q", want)
		}
	}
}

// With no --wsl-seat-dir the distro directory is the documented default.
func TestInstallVLLMSeatDefaultsTheDistroSeatDir(t *testing.T) {
	r := runSeatCommand(t, "blackwell-2x16", "-distro", "distro", "-lmcache-overlay", wslTestOverlay)
	if r.err != nil {
		t.Fatal(r.err)
	}
	env := envLines(readRendered(t, r.out, "qwen3.8-27b-vllm.env"))
	if got := env["SEAT_L2_STATUS_FILE"]; got != "/opt/seat/seat-l2-qwen3.8-27b-vllm.status" {
		t.Errorf("SEAT_L2_STATUS_FILE = %q, want it under the /opt/seat default", got)
	}
}

// The next steps differ by launch, and the linux text (sudo, systemctl, polkit) is wrong for a
// box that starts its engine through a scheduled task. The linux path is untouched.
func TestInstallVLLMSeatSaysWhatToDoNextForAWSLSeat(t *testing.T) {
	r := runSeatCommand(t, "blackwell-3x16", "-distro", "distro", "-lmcache-overlay", wslTestOverlay)
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{"register-seat-tasks.ps1", "distro", "/opt/seat", "seat_fg.sh", "seat_stop.sh", "templates"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the next steps do not mention %q:\n%s", want, r.stdout)
		}
	}
	for _, bad := range []string{"systemctl", "polkit", "sudo"} {
		if strings.Contains(r.stdout, bad) {
			t.Errorf("a windows-wsl seat has no %s step, yet the output says it:\n%s", bad, r.stdout)
		}
	}
	lin := runSeatCommand(t, "blackwell-16")
	if lin.err != nil {
		t.Fatal(lin.err)
	}
	for _, want := range []string{"TWO ROOT STEPS REMAIN", "systemctl daemon-reload", "polkit"} {
		if !strings.Contains(lin.stdout, want) {
			t.Errorf("the linux-systemd output lost %q:\n%s", want, lin.stdout)
		}
	}
}

// A flag that reads as a decision and is never read is how a box drifts from the tier it
// claims to be. The windows-wsl flags are refused for a seat that starts through systemd.
func TestInstallVLLMSeatRefusesWSLOnlyFlagsOnALinuxTier(t *testing.T) {
	for flag, val := range map[string]string{
		"-distro": "distro", "-wsl-seat-dir": "/srv/seat", "-host-prefix": "/mnt/distro",
		"-cache-mount-src": "//store.example/kvcache", "-cache-mount-opts": "vers=3.1.1",
	} {
		t.Run(flag, func(t *testing.T) {
			r := runSeatCommand(t, "blackwell-16", flag, val)
			if r.err == nil {
				t.Fatalf("%s was accepted for a linux-systemd seat, where nothing reads it", flag)
			}
			for _, want := range []string{"-" + flag, "windows-wsl"} {
				if !strings.Contains(r.err.Error(), want) {
					t.Errorf("the refusal must name %q, got %v", want, r.err)
				}
			}
			if got := renderedFiles(t, r.out); len(got) != 0 {
				t.Errorf("a refused render wrote %v", got)
			}
		})
	}
}

// The distro's venv and HF cache are distro paths: without --host-prefix the check statted
// "/opt/seat/..." on the host and said "build the venv" about a venv that was there. With it,
// the check reads the distro and the render carries the distro path.
func TestInstallVLLMSeatHostPrefixLetsTheCheckReadTheDistro(t *testing.T) {
	seat := committedLaneSeat(t, "blackwell-3x16")
	distro := t.TempDir()
	put := func(rel string, dir bool) {
		p := filepath.Join(distro, filepath.FromSlash(rel))
		if dir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return
		}
		putScript(t, filepath.ToSlash(p))
	}
	put("opt/seat/vllm-env/bin/vllm", false)
	put("hf/"+seat.ModelRepo+"/snapshots/abc", true)

	run := func(extra ...string) (string, error) {
		home := filepath.ToSlash(t.TempDir())
		args := append([]string{"-profile", "blackwell-3x16", "-root", ".", "-home", home, "-out", home + "/rendered",
			"-user", "svcuser", "-proxy-host", "127.0.0.1", "-venv", "/opt/seat/vllm-env", "-hf-home", "/hf",
			"-distro", "distro", "-lmcache-overlay", wslTestOverlay}, extra...)
		var err error
		captureStderr(t, func() { captureStdout(t, func() { err = runInstallVLLMSeat(args) }) })
		return home + "/rendered", err
	}

	if _, err := run(); err == nil {
		t.Fatal("the distro's paths do not exist on this filesystem; the check must refuse without --host-prefix")
	} else if !strings.Contains(err.Error(), "--host-prefix") {
		t.Errorf("the refusal must tell a windows-wsl operator about --host-prefix, got %v", err)
	}

	out, err := run("-host-prefix", distro)
	if err != nil {
		t.Fatalf("with --host-prefix the check must read the distro: %v", err)
	}
	if got := envLines(readRendered(t, out, "qwen3.8-27b-vllm.env"))["SEAT_HF_HOME"]; got != "/hf" {
		t.Errorf("SEAT_HF_HOME = %q, want the distro path /hf (the host prefix never reaches a file)", got)
	}
	for _, f := range renderedFiles(t, out) {
		if strings.Contains(readRendered(t, out, f), filepath.ToSlash(distro)) {
			t.Errorf("%s carries the host prefix %q; it is for the check only", f, distro)
		}
	}
}
