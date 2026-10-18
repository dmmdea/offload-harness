package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
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

	// The windows-wsl half. A seat whose engine lives in a WSL distro under a Windows box is
	// rendered with these (Runtime documents each); a linux-systemd seat reads none of them.
	distro         string
	wslSeatDir     string
	lmcacheOverlay string
	cacheMountSrc  string
	cacheMountOpts string
	hostPrefix     string
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

		// Passed through as given: empty means the Runtime's own default (/opt/seat) or "not
		// used", and the render refuses the combinations that cannot work (check below).
		Distro:         f.distro,
		WSLSeatDir:     f.wslSeatDir,
		LMCacheOverlay: f.lmcacheOverlay,
		CacheMountSrc:  f.cacheMountSrc,
		CacheMountOpts: f.cacheMountOpts,
		HostPrefix:     f.hostPrefix,
	}
}

// check refuses the flag combinations that cannot describe a working seat, naming the FLAG the
// operator can pass. They are the facts vllmseat.Artifacts refuses by Runtime field name, caught
// before anything is probed or written (Artifacts stays the backstop for every other caller),
// plus the reverse: a windows-wsl-only flag given for a seat that starts through systemd is
// refused rather than ignored, because a setting that reads as a decision and is never read is
// how a box drifts from the tier it claims to be.
func (f vllmRuntimeFlags) check(tier string, s vllmseat.Spec) error {
	wsl := s.Launch == vllmseat.LaunchWindowsWSL
	if !wsl {
		var stray []string
		for _, x := range []struct{ flag, val string }{
			{"--distro", f.distro}, {"--wsl-seat-dir", f.wslSeatDir}, {"--host-prefix", f.hostPrefix},
			{"--cache-mount-src", f.cacheMountSrc}, {"--cache-mount-opts", f.cacheMountOpts},
			// The linux-systemd templates carry no LMCache wiring and no overlay token, so the
			// flag would clear the fp8 refusal below and change no byte of the output.
			{"--lmcache-overlay", f.lmcacheOverlay},
		} {
			if x.val != "" {
				stray = append(stray, x.flag)
			}
		}
		if len(stray) > 0 {
			verb, them := "apply", "them"
			if len(stray) == 1 {
				verb, them = "applies", "it"
			}
			return fmt.Errorf("tier %s: %s %s to launch %s only, and this seat starts through %s — "+
				"nothing would read %s", tier, strings.Join(stray, ", "), verb, vllmseat.LaunchWindowsWSL, vllmseat.LaunchLinuxSystemd, them)
		}
	}
	if wsl && f.distro == "" {
		return fmt.Errorf("tier %s: launch %s needs --distro, the WSL distribution the engine runs in — "+
			"the scheduled tasks and the wrappers name it", tier, vllmseat.LaunchWindowsWSL)
	}
	if wsl {
		// Everything the ENGINE reads is a path inside the distro. The defaults hang off --home (a
		// Windows path here) and the HF cache falls back to $HF_HOME (also a Windows path), so a
		// render that took either would write a Windows path into an env bash cannot open, and the
		// seat would fail at its first start. Name them, as distro paths, each with an example of its
		// own: one shared example sent an operator who left out --venv to pass the HF cache.
		for _, x := range []struct {
			flag, val, example string
			optional           bool // empty is a default (or checked below), not a missing value
		}{
			{"--venv", f.venv, "/opt/seat/vllm-env", false},
			{"--hf-home", f.hfHome, "/hf", false},
			{"--wsl-seat-dir", f.wslSeatDir, "/opt/seat", true},
			{"--lmcache-overlay", f.lmcacheOverlay, "/opt/seat/lmcache-overlay", true},
			// Rendered into SEAT_MODEL, so it is what the engine opens: a directory that merely
			// exists on the Windows side would pass the probe by accident.
			{"--model-path", s.ModelPath, "/hf/hub/models--<org>--<name>/snapshots/<revision>", true},
		} {
			if x.val == "" && x.optional {
				continue
			}
			if !strings.HasPrefix(x.val, "/") {
				got := fmt.Sprintf("%q", x.val)
				if x.val == "" {
					got = "no value"
				}
				return fmt.Errorf("tier %s: launch %s needs %s as an absolute path INSIDE the distro %q (for example %s), got %s — "+
					"the engine and the seat env read it there, and a Windows path or a default under --home would not open",
					tier, vllmseat.LaunchWindowsWSL, x.flag, f.distro, x.example, got)
			}
		}
		// --host-prefix reads ONE distro's filesystem and --distro names the one the tasks run in. When
		// the prefix is a share named after a distro, the two must agree, or the check vouches for a
		// venv and weights the seat will not find. Share and distro names are case-insensitive, so the
		// comparison is too; a prefix that is a plain directory names no distro and is not compared.
		if name, ok := wslShareDistro(f.hostPrefix); ok && !strings.EqualFold(name, f.distro) {
			return fmt.Errorf("tier %s: --host-prefix %q reads the filesystem of the distro %q, but --distro names %q — "+
				"the prerequisite check would vouch for a venv and weights the seat will not find in %q",
				tier, f.hostPrefix, name, f.distro, f.distro)
		}
		// The two cache-mount flags are read only for a seat that has a cache server. On a storeless
		// seat their tokens render empty, and an operator who believes the share is mounted would get a
		// same-box seat with no word that the flags were discarded.
		if s.CacheServer == nil {
			var stray []string
			for _, x := range []struct{ flag, val string }{
				{"--cache-mount-src", f.cacheMountSrc}, {"--cache-mount-opts", f.cacheMountOpts},
			} {
				if x.val != "" {
					stray = append(stray, x.flag)
				}
			}
			if len(stray) > 0 {
				return fmt.Errorf("tier %s: %s given for a seat with no cache_server (the tier declares none, or --config binds it "+
					"storeless): the wrapper mounts nothing, so nothing would read it", tier, strings.Join(stray, ", "))
			}
		}
	}
	if s.NeedsLMCacheOverlay() && f.lmcacheOverlay == "" {
		if !wsl {
			// Only a windows-wsl render wires an LMCache overlay into the files it writes. No flag could
			// clear this for a seat that starts through systemd, so asking for one would send the
			// operator to a second refusal.
			return fmt.Errorf("tier %s: kv_cache_dtype %q with a cache_server cannot be rendered for a seat that starts through %s: "+
				"only launch %s wires an LMCache overlay into the files it writes, and stock LMCache restores fp8 pages corrupt "+
				"(upstream PR #4253) while reporting success", tier, s.KVCacheDtype, vllmseat.LaunchLinuxSystemd, vllmseat.LaunchWindowsWSL)
		}
		return fmt.Errorf("tier %s: kv_cache_dtype %q with a cache_server needs --lmcache-overlay, the patched LMCache "+
			"directory (a path the engine sees) — stock LMCache restores fp8 pages corrupt (upstream PR #4253) and "+
			"reports success while doing it, so this pair would serve wrong context silently", tier, s.KVCacheDtype)
	}
	return nil
}

// wslShareDistro is the distro a --host-prefix names when it is a WSL share (\\wsl.localhost\<name> or
// \\wsl$\<name>), in either slash direction and any case; ok is false for any other prefix.
func wslShareDistro(prefix string) (name string, ok bool) {
	p := strings.ReplaceAll(prefix, `\`, "/")
	lower := strings.ToLower(p)
	for _, host := range []string{"//wsl.localhost/", "//wsl$/"} {
		if strings.HasPrefix(lower, host) {
			name, _, _ = strings.Cut(p[len(host):], "/")
			return name, name != ""
		}
	}
	return "", false
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

// extraVLLMSeatsFor is vllmSeatFor for a tier's EXTRA seats (`extra_vllm_seats`): each one
// renders only when THIS box can run it — the hand-built venv, that seat's own weights and the
// wrapper scripts its entry runs (vllmseat.Spec.DetectExtra), with the same deployment inputs
// the lane seat gets. A seat that cannot run is skipped with the reason printed, and the layer
// it backs is left out of the seeded layers (tierseed.ResolveLayers), so the box never
// advertises a seat it cannot serve. The lane seat's presence is irrelevant here: the seats are
// independent prerequisites.
//
// What the installer renders for an extra seat is its llama-swap ENTRY (the render's job); its
// systemd unit, wrapper scripts and polkit rule are NOT rendered, because the seat's launch line
// carries flags the shared unit template cannot express (the 35B's `--language-model-only`).
// They are the operator's step, so they are also the prerequisite: llama-swap does not check that
// an entry's `cmd` exists when it loads its config, so a seat advertised without them is listed
// and fails only when a contract asks for it.
//
// It also returns the box's runtime (the account, the address the engine binds, the seat
// directory), because an entry cannot be rendered without one, and the lane seat is not always the
// one that carries it: a box that runs the extra seat alone (the lane seat's weights absent, or two
// snapshots under them mid-upgrade) still needs it. The runtime is the zero value when no extra
// seat renders, exactly as vllmSeatFor's is when the lane seat does not, so a box with nothing to
// run keeps the provenance stamp it had.
func extraVLLMSeatsFor(p servingProfile, home string, f vllmRuntimeFlags) ([]*vllmseat.Spec, vllmseat.Runtime) {
	var out []*vllmseat.Spec
	if len(p.ExtraVLLMSeats) == 0 {
		return nil, vllmseat.Runtime{}
	}
	rt := f.resolve(home)
	for _, e := range p.ExtraVLLMSeats {
		s := e
		skip := func(why string) {
			fmt.Fprintf(os.Stderr, "NOTE  extra vLLM seat %q not rendered (%s); the layer it backs is not seeded\n", s.ID, why)
		}
		if home == "" {
			skip("no --home, so the seat's paths cannot be resolved")
			continue
		}
		if rt.User == "" || rt.ProxyHost == "" {
			skip("--vllm-user and --vllm-proxy-host were not supplied")
			continue
		}
		if ok, why := s.DetectExtra(rt); !ok {
			skip(why)
			continue
		}
		resolved, err := s.Resolve(rt)
		if err != nil {
			skip(err.Error())
			continue
		}
		out = append(out, &resolved)
	}
	if len(out) == 0 {
		return nil, vllmseat.Runtime{}
	}
	return out, rt
}

// detectVLLMSeats is the ONE prerequisite detection `install seed` and `audit-config` share:
// whether this box can run the tier's lane seat and each of its extra seats. It mirrors what
// `install render` decides (vllmSeatFor / extraVLLMSeatsFor) minus the two deployment flags
// only the render needs, so the binding and the rendered llama-swap entry agree by
// construction — pass the same venv, HF home and seat directory to both, or the config will name
// a model llama-swap never serves. An extra seat also needs its wrapper scripts in the seat
// directory (Spec.DetectExtra). Every skip is said out loud, on note.
func detectVLLMSeats(p tierseed.Profile, rt vllmseat.Runtime, note io.Writer) (lane bool, extras map[string]bool) {
	extras = map[string]bool{}
	if p.VLLMSeat != nil {
		var why string
		if lane, why = p.VLLMSeat.Detect(rt); !lane {
			fmt.Fprintf(note, "NOTE  vLLM agent seat %q unavailable (%s); binding %s\n", p.VLLMSeat.ID, why, p.VLLMSeat.Fallback)
		}
	}
	for _, e := range p.ExtraVLLMSeats {
		ok, why := e.DetectExtra(rt)
		extras[e.ID] = ok
		if !ok {
			fmt.Fprintf(note, "NOTE  extra vLLM seat %q unavailable (%s); the layer it backs is not seeded\n", e.ID, why)
		}
	}
	return lane, extras
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
	user := fs.String("user", "", "account llama-swap runs as; the polkit rule is scoped to it (launch windows-wsl: the interactive Windows account the seat tasks are registered for)")
	proxy := fs.String("proxy-host", "", "LITERAL address the engine binds (an IP)")
	venv := fs.String("venv", "", "hand-built vLLM virtualenv (default: <home>/vllm-env; launch windows-wsl: required, a path inside the distro, written to SEAT_VENV)")
	hfHome := fs.String("hf-home", "", "HF cache root (default: $HF_HOME, else <home>/hf; launch windows-wsl: required, a path inside the distro, e.g. /hf)")
	modelPath := fs.String("model-path", "", "exact HF snapshot directory, overriding resolution from the tier's model_repo (the hash differs per download, so --force needs this; launch windows-wsl: a path inside the distro, written to SEAT_MODEL)")
	force := fs.Bool("force", false, "write the artifacts even when the venv or the weights are absent")
	cfgPath := fs.String("config", "", "harness config.json whose kv_cache_server binding FOR THIS SEAT overrides the tier's cache_server (per-seat store dir and key_prefix)")
	distro := fs.String("distro", "", "launch windows-wsl: the WSL distribution the engine runs in (required)")
	wslSeatDir := fs.String("wsl-seat-dir", "", "launch windows-wsl: the directory INSIDE the distro holding the seat env and wrapper scripts (default /opt/seat)")
	overlay := fs.String("lmcache-overlay", "", "launch windows-wsl: patched LMCache directory prepended to PYTHONPATH, a path inside the distro (required for an fp8-KV seat with a cache server)")
	mountSrc := fs.String("cache-mount-src", "", "launch windows-wsl: the cache export the seat wrapper mounts before the MP server starts, e.g. //store.example/kvcache (empty = already mounted; refused for a seat with no cache server)")
	mountOpts := fs.String("cache-mount-opts", "", "launch windows-wsl: mount options for that export (carries the credentials path; refused for a seat with no cache server)")
	hostPrefix := fs.String("host-prefix", "", "launch windows-wsl: where this process reads the distro's filesystem for the prerequisite check, e.g. \\\\wsl.localhost\\<distro>; a share must name --distro (never written to a file)")
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
	if *cfgPath != "" {
		applied, err := applySeatBinding(s, *cfgPath)
		if err != nil {
			return err
		}
		s = applied
	}
	flags := vllmRuntimeFlags{user: *user, proxyHost: *proxy, venv: *venv, hfHome: *hfHome,
		distro: *distro, wslSeatDir: *wslSeatDir, lmcacheOverlay: *overlay,
		cacheMountSrc: *mountSrc, cacheMountOpts: *mountOpts, hostPrefix: *hostPrefix}
	rt := flags.resolve(*home)
	if err := rt.Validate(); err != nil {
		return err
	}
	if err := flags.check(id, s); err != nil {
		return err
	}
	if ok, why := s.Detect(rt); !ok {
		if !*force {
			if s.Launch == vllmseat.LaunchWindowsWSL && rt.HostPrefixProblem() != "" {
				// Every probe went through a prefix that cannot be read, so nothing was checked: this is
				// not a missing venv, and "build it" would send the operator to the wrong place.
				return fmt.Errorf("tier %s: %s — --host-prefix is how this process reads the distro's venv and HF cache, so "+
					"nothing could be checked. Pass the distro's filesystem as this machine sees it (a share under wsl.localhost "+
					"named after --distro %q), or pass --force to write the artifacts without the check", id, why, rt.Distro)
			}
			hint := ""
			if s.Launch == vllmseat.LaunchWindowsWSL {
				if rt.HostPrefix == "" {
					// The venv and the HF cache of a windows-wsl seat are paths INSIDE the distro; this
					// process can only look at them through the distro's share of the host filesystem.
					hint = ". The venv and the HF cache of a windows-wsl seat are paths inside the distro: if they are " +
						"there, pass --host-prefix (the distro's filesystem as this machine sees it, a share under wsl.localhost) " +
						"so this check can read them"
				} else {
					hint = fmt.Sprintf(". This check read the distro through --host-prefix %q: confirm it is the filesystem of "+
						"the distro %q, and that the venv and the HF cache are at the paths the flags give", rt.HostPrefix, rt.Distro)
				}
			}
			return fmt.Errorf("tier %s: %s — the box cannot run this seat, so the artifacts would describe a unit "+
				"that fails at boot. Build the vLLM venv and fetch the weights first, or pass --force to write them anyway%s", id, why, hint)
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
	// The two PowerShell stubs are one file for every Windows seat of the tier (each seat's llama-swap entry runs the same
	// seat-cmd.ps1 with its own seat id), so the render that writes them is given the tier's other Windows seats too.
	siblings := windowsSiblings(s, p)
	files, err := s.ArtifactsWith(s.TemplatesDir(*root), rt, siblings)
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
		dst := filepath.Join(dir, filepath.FromSlash(n))
		// An artifact may live in a subdirectory (a shipped chat template lands in
		// templates/), so its parent is created here rather than assumed.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(files[n]), mode); err != nil {
			return err
		}
		// Windows ignores the exec bit; the artifacts are for a Linux node, and
		// install.sh chmods them there. Say so rather than pretending it took.
		fmt.Printf("wrote %s\n", filepath.ToSlash(dst))
	}

	if s.Launch == vllmseat.LaunchWindowsWSL {
		fmt.Print(wslNextSteps(s, rt, dir, names, siblings))
	} else {
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
	}
	if strings.Contains(rt.HFHome, "/") && len(rt.HFHome) > 24 {
		fmt.Printf("NOTE  hf-home %q is long: LMCache's fs_native page names embed the resolved model path and the\n"+
			"      reference box's long path produced 268-byte names against NAME_MAX 255, failing every L2 store\n"+
			"      silently. Mount a short path (the reference deployment uses /hf) before configuring an L2 tier.\n", rt.HFHome)
	}
	fmt.Print(extraSeatsNote(p))
	return nil
}

// windowsSiblings are the tier's other Windows seats: its extra vLLM seats that start through WSL, in the tier's order.
// They share the lane seat's seat directory and its two PowerShell stubs, so the render of those stubs must hold them
// (vllmseat.Spec.ArtifactsWith). Nil for a lane seat that starts through systemd, which renders no stub, and for a tier
// with no extra Windows seat. A sibling is listed whether or not this box can run it: the table is the same on every box
// of the tier, and an entry for a seat that never starts is read by nothing.
func windowsSiblings(s vllmseat.Spec, p servingProfile) []vllmseat.Spec {
	if s.Launch != vllmseat.LaunchWindowsWSL {
		return nil
	}
	var out []vllmseat.Spec
	for _, e := range p.ExtraVLLMSeats {
		if e.Launch == vllmseat.LaunchWindowsWSL {
			out = append(out, e)
		}
	}
	return out
}

// wslNextSteps is what is left to do after a windows-wsl render, which writes files and takes no
// step itself: nothing is copied into the distro and no task is registered. The engine starts
// through a scheduled task in the operator's interactive session (a WSL distro belongs to that
// user, and llama-swap runs as SYSTEM), so the steps are a copy into the distro, an elevated
// registration, and the llama-swap entry that drives the stubs. names are the files this run wrote;
// siblings are the tier's other Windows seats, which the two stubs also serve.
func wslNextSteps(s vllmseat.Spec, rt vllmseat.Runtime, dir string, names []string, siblings []vllmseat.Spec) string {
	into := fmt.Sprintf("  1. In the distro %s, put these in %s (copy; this command copies nothing into the distro):\n"+
		"       %s/%s.env", rt.Distro, rt.DistroSeatDir(), filepath.ToSlash(dir), s.ID)
	for _, n := range names {
		if strings.HasPrefix(n, "templates/") {
			into += fmt.Sprintf("\n       %s/templates/  (the whole directory: the env's --chat-template names the file in it)", filepath.ToSlash(dir))
			break
		}
	}
	into += "\n       setup/templates/vllm-seat/seat_fg.sh and seat_stop.sh  (shipped as they are, not rendered)\n"
	where := "were written there"
	if filepath.ToSlash(filepath.Clean(dir)) != path.Clean(rt.SeatDir) {
		where = fmt.Sprintf("were written to %s: copy them there", filepath.ToSlash(dir))
	}
	stubs := ""
	if len(siblings) > 0 {
		ids := []string{s.ID}
		for _, o := range siblings {
			ids = append(ids, o.ID)
		}
		sort.Strings(ids)
		stubs = fmt.Sprintf("     The two stubs serve every Windows seat of the tier (%s): each seat's llama-swap entry passes its own seat id, and a\n"+
			"     seat id they were not rendered for is refused (exit 5). Every render of the tier writes them whole, so a later\n"+
			"     render does not drop a seat. This run wrote the env file and the tasks script of %s only.\n", strings.Join(ids, ", "), s.ID)
	}
	return "\nSTEPS REMAIN (this command only writes files):\n" + into +
		fmt.Sprintf("  2. The four Windows scripts (seat-cmd.ps1, seat-cmdstop.ps1, hidden.vbs, register-seat-tasks.ps1) belong in %s,\n"+
			"     the directory the llama-swap entry runs them from; they %s. Run register-seat-tasks.ps1 once from an\n"+
			"     elevated PowerShell: it registers the two interactive-session tasks for the account %s.\n"+
			"%s"+
			"  3. Add the seat's llama-swap entry from setup/templates/vllm-seat/llama-swap-entry.yaml: cmd and cmdStop run the\n"+
			"     two PowerShell stubs, ttl is the 300 s idle window and concurrencyLimit equals max_num_seqs (%d).\n"+
			"Then start the seat once and confirm its /health answers before binding the agent lane to it.\n",
			rt.SeatDir, where, rt.User, stubs, s.MaxNumSeqs)
}

// extraSeatsNote says what this command did NOT render for the tier's extra vLLM seats, at the
// moment the operator is installing seats. The installer renders an extra seat's llama-swap entry
// (`install render`) and seeds its roster entry, binding and layer (`install seed`), but not its
// systemd unit, wrapper scripts or polkit rule: its production launch line carries flags the shared
// unit template cannot express, so those stay the operator's step. "" for a tier with none.
func extraSeatsNote(p servingProfile) string {
	var b strings.Builder
	windowsLane := p.VLLMSeat != nil && p.VLLMSeat.Launch == vllmseat.LaunchWindowsWSL
	for _, e := range p.ExtraVLLMSeats {
		if windowsLane && e.Launch == vllmseat.LaunchWindowsWSL {
			// A Windows seat has no unit, wrapper scripts or polkit rule: it shares the lane seat's two PowerShell stubs, whose
			// table this run wrote (windowsSiblings). What it still needs is its own env file and scheduled tasks.
			fmt.Fprintf(&b, "NOTE  the tier's extra vLLM seat %s starts through WSL: it is in the table of the two PowerShell stubs this command wrote, "+
				"but its env file (%s.env) and its two scheduled tasks (register-seat-tasks.ps1) are NOT rendered by this command, which renders the lane seat's.\n",
				e.ID, e.ID)
			continue
		}
		fmt.Fprintf(&b, "NOTE  the tier's extra vLLM seat %s is NOT rendered by this command: its launch line carries flags the shared "+
			"unit template cannot express.\n"+
			"      Install its unit (%s.service, no [Install] section), its wrapper scripts (%s-run.sh, %s-cmd.sh, %s-cmdstop.sh) and its "+
			"polkit rule by hand; docs/systems/composite-tier.md lists exactly what differs from the rendered lane seat.\n",
			e.ID, e.Unit, e.Unit, e.Unit, e.Unit)
	}
	return b.String()
}
