package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// vllmRuntimeFlags is the deployment half of a vLLM seat as the CLI receives it.
// A tier declares the ENGINE (window, utilization, parsers — hardware-class facts);
// these are the box's own (who llama-swap runs as, what address the engine binds,
// where this machine keeps its venv and HF cache). Hardening any of them into the
// tier table would make a hardware class describe one host's layout.
type vllmRuntimeFlags struct {
	user      string
	proxyHost string
	venv      string
	seatDir   string
	hfHome    string
}

// resolve fills the path defaults from the install root. Empty user/proxyHost are
// left empty on purpose: they have no safe default, and a seat rendered against a
// guess would proxy nowhere.
func (f vllmRuntimeFlags) resolve(home string) vllmseat.Runtime {
	pick := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	hf := f.hfHome
	if hf == "" {
		hf = os.Getenv("HF_HOME")
	}
	// path (not filepath): these are POSIX paths on the TARGET node, which is Linux
	// for every vLLM seat, regardless of the OS doing the rendering.
	slash := filepath.ToSlash(home)
	return vllmseat.Runtime{
		User:      f.user,
		ProxyHost: f.proxyHost,
		StackDir:  slash,
		SeatDir:   pick(f.seatDir, path.Join(slash, "seat")),
		VenvDir:   pick(f.venv, path.Join(slash, "vllm-env")),
		HFHome:    pick(hf, path.Join(slash, "hf")),
	}
}

// vllmSeatFor decides whether THIS box renders the tier's vLLM agent seat.
//
// The seat is only ever rendered when the box can actually run it. The engine is a
// hand-built venv (vLLM 0.28, torch 2.13+cu130) holding an HF snapshot, and the
// installer does not create either — so a missing prerequisite is the documented
// fallback path, not an error. Rendering a unit that points at a venv nobody built
// produces a seat that fails at boot, which is strictly worse than the llama.cpp
// fallback the tier already names.
//
// Every skip prints WHY. The failure this replaces was silent: the reference box
// measured the vLLM seat 3.5x faster at 4x the window, the operator accepted it, and
// fresh installs went on shipping the arm that lost with nothing saying so.
func vllmSeatFor(p servingProfile, home string, f vllmRuntimeFlags) (*vllmseat.Spec, vllmseat.Runtime) {
	if p.VLLMSeat == nil {
		return nil, vllmseat.Runtime{}
	}
	s := *p.VLLMSeat
	rt := f.resolve(home)
	skip := func(why string) (*vllmseat.Spec, vllmseat.Runtime) {
		fmt.Fprintf(os.Stderr, "NOTE  vLLM agent seat %q not rendered (%s); falling back to %s\n",
			s.ID, why, s.Fallback)
		return nil, vllmseat.Runtime{}
	}
	if home == "" {
		return skip("no --home, so the seat's paths cannot be resolved")
	}
	if rt.User == "" || rt.ProxyHost == "" {
		return skip("--vllm-user and --vllm-proxy-host were not supplied")
	}
	ok, why := s.Detect(rt)
	if !ok {
		return skip(why)
	}
	resolved, err := s.Resolve(rt)
	if err != nil {
		return skip(err.Error())
	}
	return &resolved, rt
}

// runInstallVLLMSeat writes the systemd unit, the two llama-swap wrappers, the run
// script and the polkit rule for a tier's vLLM agent seat.
//
// It deliberately does NOT install them: the unit goes to /etc/systemd/system and the
// polkit rule to /etc/polkit-1/rules.d, both root-owned, and this command is run by
// the same unprivileged installer that cannot escalate. It writes the files and
// prints the two root steps, so the operator performs them knowingly.
func runInstallVLLMSeat(args []string) error {
	fs := flag.NewFlagSet("install vllm-seat", flag.ExitOnError)
	profileID := fs.String("profile", "", "tier id (default: classify this machine)")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	home := fs.String("home", "", "install root (the unit's WorkingDirectory and log location)")
	outDir := fs.String("out", "", "directory to write the rendered artifacts into (default: <home>/seat)")
	user := fs.String("user", "", "account llama-swap runs as; the polkit rule is scoped to it")
	proxy := fs.String("proxy-host", "", "LITERAL address the engine binds (an IP)")
	venv := fs.String("venv", "", "hand-built vLLM virtualenv (default: <home>/vllm-env)")
	hfHome := fs.String("hf-home", "", "HF cache root (default: $HF_HOME, else <home>/hf)")
	modelPath := fs.String("model-path", "", "exact HF snapshot directory, overriding resolution from the tier's model_repo (the hash differs per download, so --force needs this)")
	force := fs.Bool("force", false, "write the artifacts even when the venv or the weights are absent")
	_ = fs.Parse(args)

	if *home == "" {
		return fmt.Errorf("--home is required: the unit's WorkingDirectory, log path and default venv all hang off it")
	}
	id := *profileID
	if id == "" {
		id = hwdetect.Classify(hwdetect.Detect()).Profile
	}
	raw, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("profiles.json: %w", err)
	}
	p, ok := doc.Profiles[id]
	if !ok {
		return fmt.Errorf("unknown tier %q", id)
	}
	if p.VLLMSeat == nil {
		return fmt.Errorf("tier %s declares no vllm_seat — nothing to render", id)
	}
	s := *p.VLLMSeat
	if *modelPath != "" {
		s.ModelPath = *modelPath
	}
	rt := vllmRuntimeFlags{user: *user, proxyHost: *proxy, venv: *venv, hfHome: *hfHome}.resolve(*home)
	if err := rt.Validate(); err != nil {
		return err
	}
	if ok, why := s.Detect(rt); !ok {
		if !*force {
			return fmt.Errorf("tier %s: %s — the box cannot run this seat, so the artifacts would describe a unit "+
				"that fails at boot. Build the vLLM venv and fetch the weights first, or pass --force to write them anyway", id, why)
		}
		fmt.Fprintf(os.Stderr, "WARN  %s — writing anyway (--force)\n", why)
		// --force skips the prerequisite CHECK, but the unit must still name a real
		// snapshot directory: the hash differs per download, so there is nothing to
		// guess. Say so plainly rather than emitting a unit with an empty --model.
		if s.ModelPath == "" {
			if _, err := s.Resolve(rt); err != nil {
				return fmt.Errorf("%w; --force still needs --model-path, because the snapshot directory is a "+
					"content hash that differs per download and cannot be guessed", err)
			}
		}
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Join(*home, "seat")
	}
	files, err := s.Artifacts(filepath.Join(*root, "setup", "templates", "vllm-seat", "linux-systemd"), rt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mode := os.FileMode(0o644)
		if vllmseat.Executable(n) {
			mode = 0o755
		}
		dst := filepath.Join(dir, n)
		if err := os.WriteFile(dst, []byte(files[n]), mode); err != nil {
			return err
		}
		// Windows ignores the exec bit; the artifacts are for a Linux node, and
		// install.sh chmods them there. Say so rather than pretending it took.
		fmt.Printf("wrote %s\n", filepath.ToSlash(dst))
	}

	unit := s.Unit + ".service"
	rules := "50-llama-swap-" + s.Unit + ".rules"
	fmt.Printf("\nTWO ROOT STEPS REMAIN (this command runs unprivileged and does not take them):\n"+
		"  sudo install -m 0644 %s/%s /etc/systemd/system/%s\n"+
		"  sudo install -m 0644 -o root -g root %s/%s /etc/polkit-1/rules.d/%s\n"+
		"  sudo systemctl daemon-reload && sudo systemctl enable --now %s\n"+
		"Then verify WITHOUT changing state — as %s, `systemctl start %s` on the already-active unit must succeed;\n"+
		"if it prompts for a password the polkit rule is not in effect and llama-swap (NoNewPrivileges) cannot drive the seat.\n",
		filepath.ToSlash(dir), unit, unit,
		filepath.ToSlash(dir), rules, rules,
		unit, rt.User, unit)
	if strings.Contains(rt.HFHome, "/") && len(rt.HFHome) > 24 {
		fmt.Printf("NOTE  hf-home %q is long: LMCache's fs_native page names embed the resolved model path and the\n"+
			"      reference box's long path produced 268-byte names against NAME_MAX 255, failing every L2 store\n"+
			"      silently. Mount a short path (the reference deployment uses /hf) before configuring an L2 tier.\n", rt.HFHome)
	}
	return nil
}
