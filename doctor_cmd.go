// The doctor verb: its report sections and the config, data-home, fleet and
// media checks it prints.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/datahome"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)
	// Disclose the config source FIRST — and truthfully: SourceLine never credits a file
	// that was not actually read. Every binding verdict below is only as good as the file
	// that produced it, so a file that FAILED validation is named here, verbatim, before
	// anything else is printed.
	tainted := doctorConfigRow(src, os.Stdout)
	err := doctorRunChecked(cfg, mediacap.RoutesChecked(cfg, mediacap.LiveNodeChecker(doctorComfyAPI(), 3*time.Second)), os.Stdout, liveDataReport(cfg))
	if err != nil {
		return err
	}
	if tainted {
		return fmt.Errorf("config at %s %s: %w", src.Path, config.LoadFailure(src.LoadErr), src.LoadErr)
	}
	return nil
}

// doctorComfyAPI is the ComfyUI endpoint doctor asks for its node classes: the one the
// render runners use (COMFY_API, else ComfyUI's default port). doctor never starts
// ComfyUI; when nothing answers there, the node classes are decided from
// <comfy_dir>/custom_nodes on disk (mediacap.LiveNodeChecker).
func doctorComfyAPI() string {
	if v := strings.TrimSpace(os.Getenv("COMFY_API")); v != "" {
		return v
	}
	return "http://127.0.0.1:8188"
}

// doctorConfigRow prints doctor's config-source disclosure and, when the file
// failed validation, the loader's refusal text VERBATIM as a FAIL row. It
// reports whether doctor must exit non-zero because of it.
//
// The refusal text is the only thing that names the KEY and the VALUE. doctor
// kept the Source and printed only the generic one-liner, so on a config the
// loader had refused it printed a vague "failed to load", ran every other check
// against the file's surviving settings, and exited 0 — the one verb an operator
// runs to find out what is wrong stayed silent about the thing that was wrong.
//
// The row goes FIRST and the rest of doctor still runs: the media, cache-server
// and config-findings sections are pure config and stay useful, and an operator
// fixing one key wants to see the others in the same pass.
func doctorConfigRow(src config.Source, w io.Writer) bool {
	fmt.Fprintln(w, config.SourceLine(src))
	if src.LoadErr == nil {
		return false
	}
	fmt.Fprintf(w, "config:     FAIL  %s — %v\n", strings.ToUpper(config.LoadFailure(src.LoadErr)), src.LoadErr)
	return true
}

// aliasCheck pairs a config key with its configured llama-swap model alias.
type aliasCheck struct{ Key, Alias string }

// modelAliases is the subset of config.ModelRoutes the alias-vs-live-roster gate
// checks — the bindings whose absence from the roster means a route this node
// advertises cannot actually run. The table itself lives in internal/config so
// `doctor`, `acceptance`, `report`, and the MCP server's `offload_status` read
// ONE list; they used to carry two hardcoded ones that had drifted 8 keys to 10.
func modelAliases(cfg config.Config) []aliasCheck {
	routes := cfg.ModelRoutes()
	out := make([]aliasCheck, 0, len(routes))
	for _, r := range routes {
		if r.Diffed {
			out = append(out, aliasCheck{Key: r.Key, Alias: r.Configured})
		}
	}
	return out
}

// doctorRun checks endpoint health, then diffs the LIVE /v1/models roster
// against every configured model alias (LO-11: doctor used to GET only
// /health, so a renamed/removed llama-swap alias passed doctor yet every call
// deferred). Any missing alias prints FAIL and the command exits non-zero.
//
// It also reports the DERIVED media routes (internal/mediacap): doctor checked
// model aliases only, so on a node whose generate_image deferred on a render
// script that was not on disk it still printed green. Passing them in keeps the
// filesystem probe at the caller's edge, so the check itself stays testable.
func doctorRun(cfg config.Config, routes []mediacap.Route, w io.Writer) error {
	return doctorRunChecked(cfg, routes, w, nil)
}

// Seams for the machine facts the data-volume audit reads: the drive Windows boots
// from and the volumes this host mounts. A test stands in for the machine.
var (
	doctorOSDrive = volumes.OSDriveRoot
	doctorVolumes = volumes.List
)

// liveDataReport audits THIS machine: nil when it has no OS drive letter (the rule is
// about the drive Windows boots from), else the report built from the mounted volumes.
// A volume list that cannot be read is an empty one, which the audit words as "no
// other volume qualifies" rather than guessing a target.
func liveDataReport(cfg config.Config) *datahome.Report {
	osDrive := doctorOSDrive()
	if osDrive == "" {
		return nil
	}
	// The volume list is only worth enumerating when something sits on the OS drive: a
	// clean node must not pay for a drive scan (a dead mapped share can stall one).
	r := datahome.Audit(cfg, osDrive, nil)
	if len(r.OnOS) == 0 {
		return &r
	}
	vols, err := doctorVolumes()
	if err != nil {
		vols = nil
	}
	r = datahome.Audit(cfg, osDrive, vols)
	return &r
}

// writeDataHomeSection prints the data-volume verdict (register C-92) and returns how
// many data locations make doctor FAIL: those on the OS drive while a data volume
// qualifies to take them. The operator rule is that C: holds Windows and program
// installs, never data; the section names every config key whose resolved path sits
// there and the way off it. A node with nowhere to move to gets one note and no FAIL,
// and a clean node (or a host with no OS drive letter) prints nothing at all.
func writeDataHomeSection(w io.Writer, r *datahome.Report) int {
	if r == nil || len(r.OnOS) == 0 {
		return 0
	}
	if r.Target == nil {
		fmt.Fprintf(w, "data volume: -     %d data location(s) are on the OS drive %s but no other volume qualifies (%s); there is nowhere to move them to\n",
			len(r.OnOS), r.OSDrive, r.Why)
		return 0
	}
	home := datahome.ProposedHome(r.Target.Volume.Root)
	fmt.Fprintf(w, "data volume (%s holds Windows and program installs, never data):\n", strings.TrimRight(r.OSDrive, `\/`))
	root, under, others := r.Grouped()
	if root != nil {
		fmt.Fprintf(w, "  FAIL  %-28s %s\n", root.Key, root.Path)
		if len(under) > 0 {
			names := make([]string, len(under))
			for i, l := range under {
				names[i] = l.Key
			}
			fmt.Fprintf(w, "        and %d paths under it that follow it: %s\n", len(under), strings.Join(names, ", "))
		}
	}
	for _, l := range others {
		fmt.Fprintf(w, "  FAIL  %-28s %s\n", l.Key, l.Path)
	}
	fmt.Fprintf(w, "  move to %s (%s). The data is COPIED, never moved and never put behind a junction:\n", r.Target.Volume.Root, r.Target.Because)
	fmt.Fprintf(w, "    1. stop fleet-serve and every MCP door (they hold the cache and the ledger open)\n")
	fmt.Fprintf(w, "    2. local-offload data migrate --to %s --apply --stopped\n", home)
	fmt.Fprintf(w, "    3. in config.json set  \"home\": \"%s\"  (a key you wrote by hand above keeps its value: change it too)\n", datahome.HomeValue(home))
	fmt.Fprintf(w, "    4. restart the doors and re-run doctor; delete the old tree only after the new home has run clean\n")
	return len(r.OnOS)
}

// writeFleetSkewSection prints one row per fleet remote (delegate_remotes): OK
// when the node runs this binary's release, SKEW with both versions when it
// does not, UNKNOWN when the node publishes no version, UNREACHABLE when its
// health cannot be read. Parity broke twice on 2026-09-30 within an hour (a
// merge landed between deploys); the session-start audit catches it, and now
// so does any session that runs doctor. No remotes prints nothing.
func writeFleetSkewSection(w io.Writer, cfg config.Config, self string, read func(base string) (string, error)) {
	members := rosterprobe.Members(cfg.DelegateRemotes)
	if len(members) == 0 {
		return
	}
	fmt.Fprintf(w, "fleet versions (this binary %s):\n", self)
	for _, m := range members {
		v, err := read(m.Base)
		base := rosterLabel(m)
		switch {
		case rosterprobe.IsRefusal(err):
			fmt.Fprintf(w, "  REFUSED      %s — not dialled, %s\n", base, rosterprobe.Scrub(m.Base, err))
		case err != nil:
			fmt.Fprintf(w, "  UNREACHABLE  %s — %s\n", base, rosterprobe.Scrub(m.Base, err))
		case v == "":
			fmt.Fprintf(w, "  UNKNOWN      %s — the node publishes no harness_version\n", base)
		case v != self:
			fmt.Fprintf(w, "  SKEW         %s runs %s (this binary %s): redeploy to parity (plans/fleet-parity-deploy.sh)\n", base, v, self)
		default:
			fmt.Fprintf(w, "  OK           %s %s\n", base, v)
		}
	}
}

// doctorFleetProbeTimeout is how long doctor waits for one node's health, and for its answer to the
// token probe. The roster is read concurrently, so this is the whole section's worst case.
const doctorFleetProbeTimeout = 5 * time.Second

// rosterLabel is how doctor names a roster entry: its slot in delegate_remotes (the number the config
// findings use, blank slots counted) and its base with whatever a URL can carry that is a secret
// removed, so an operator can find the line to fix and the output is safe to paste. The redaction
// is the roster package's (Member.Shown, rosterprobe.Scrub), shared with the lanes' "probed ..." lines.
func rosterLabel(m rosterprobe.Member) string {
	return fmt.Sprintf("delegate_remotes[%d] %s", m.Index, m.Shown())
}

// rosterVersionReader answers writeFleetSkewSection from one roster reading: a node's published
// harness_version, or why there is none (the entry was refused and never dialled, or the node did not
// answer).
func rosterVersionReader(roster []rosterprobe.Reading) func(base string) (string, error) {
	byBase := make(map[string]rosterprobe.Reading, len(roster))
	for _, r := range roster {
		byBase[r.Base] = r
	}
	return func(base string) (string, error) {
		r, ok := byBase[base]
		switch {
		case !ok:
			return "", fmt.Errorf("not read")
		case r.Err != nil:
			return "", r.Err
		}
		return r.View.HarnessVersion, nil
	}
}

// writeFleetTokenSection prints whether each reachable node accepts this box's fleet_auth_token, and
// says plainly when this box has none.
//
// /fleet/health ignores the Authorization header, so a token that is missing, wrong or absent on the
// node reads healthy on every other row; the first sign was a 401 on dispatch, which the delegator does
// not re-place. check asks the node the one question health cannot (rosterprobe.CheckToken: an agent
// dispatch with no job_id, which the node answers after its bearer check and before it creates
// anything). Only nodes whose health was read are asked, concurrently; a refused or unreachable
// entry is named UNCHECKED, with the reason the version rows already gave.
//
// Informational, like the version rows: it never changes doctor's exit code, because a node that
// disagrees about a token is a finding about the pair, not a broken local box. The token itself and
// the node's reply text are never printed, only the status class.
func writeFleetTokenSection(w io.Writer, cfg config.Config, roster []rosterprobe.Reading, check func(base string) (status int, body string, err error)) {
	if len(roster) == 0 {
		return
	}
	if cfg.FleetAuthToken == "" {
		fmt.Fprintln(w, "fleet token: NOT SET here (fleet_auth_token) — every lane of a node requires the bearer (401 when the node has a token, 403 when it listens beyond loopback with none), so this box's calls to a node fail until the same token is set here; only a node on loopback with no token is open")
		return
	}
	type verdict struct {
		state  rosterprobe.TokenState
		status int
		err    error
	}
	out := make([]verdict, len(roster))
	var wg sync.WaitGroup
	for i, r := range roster {
		if r.Err != nil {
			continue
		}
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			status, body, err := check(base)
			out[i] = verdict{state: rosterprobe.ClassifyToken(status, body), status: status, err: err}
		}(i, r.Base)
	}
	wg.Wait()
	fmt.Fprintln(w, "fleet token (fleet_auth_token is set here; its value is never printed):")
	for i, r := range roster {
		base, v := rosterLabel(r.Member), out[i]
		switch {
		case rosterprobe.IsRefusal(r.Err):
			fmt.Fprintf(w, "  UNCHECKED    %s — not asked: the entry is refused by the tailnet guard (see fleet versions)\n", base)
		case r.Err != nil:
			fmt.Fprintf(w, "  UNCHECKED    %s — not asked: the node's health could not be read (see fleet versions)\n", base)
		case v.err != nil:
			fmt.Fprintf(w, "  UNKNOWN      %s — the token probe failed: %s\n", base, rosterprobe.Scrub(r.Base, v.err))
		case v.state == rosterprobe.TokenAccepted:
			fmt.Fprintf(w, "  OK           %s — the node accepts this box's token\n", base)
		case v.state == rosterprobe.TokenMismatch:
			fmt.Fprintf(w, "  MISMATCH     %s — the node answered 401: its fleet_auth_token is not this box's, so every call to it (every lane) fails until the two match\n", base)
		case v.state == rosterprobe.TokenNodeHasNone:
			fmt.Fprintf(w, "  NO-TOKEN     %s — the node answered 403: it listens beyond loopback with no fleet_auth_token, so it refuses every lane for every caller (a current fleet-serve will not start so); set the same token on the node\n", base)
		default:
			fmt.Fprintf(w, "  UNKNOWN      %s — the node answered %d to the token probe, which does not say whether the token is right (an older node?)\n", base, v.status)
		}
	}
}

// writeConfigFindingsSection prints ONE LINE PER non-fatal configuration finding
// (config.Config.Findings) and returns how many — the number that makes doctor
// exit non-zero.
//
// These are values that LOAD and then cannot do what they say: a fleet remote on
// a port no node listens on, a lane base carrying the /v1 segment the client
// appends itself, a media-lane GPU wait sitting at ten minutes against the 90 s
// its own key documents (register C-33), a retired key the file still carries.
// Every one of them was previously invisible: the retired-key note was a single
// stderr line at startup that scrolls past every command, and the rest were only
// ever observable as a dial timeout or a ten-minute block on a media call.
//
// Findings are reported on the config THIS PROCESS loaded, not a re-read of the
// file, so the rows explain the behaviour the operator is actually seeing — the
// `config:` line above says which file that was.
//
// A clean config prints nothing at all, header included: a green doctor stays as
// short as it is today.
func writeConfigFindingsSection(w io.Writer, cfg config.Config) int {
	findings := cfg.Findings()
	if len(findings) == 0 {
		return 0
	}
	fmt.Fprintln(w, "config findings (values in the loaded config that cannot do what they say):")
	for _, f := range findings {
		fmt.Fprintf(w, "  FAIL  %s\n", f)
	}
	return len(findings)
}

// writeMulticardNotesSection prints a NOTE row for each flat setting the multicard seats do not apply (config.MulticardLaunchNotes): comfy_cuda_device and
// comfy_dynamic_vram "on" still serve the single-card routes, so the row says what the config does and asks for nothing. It prints nothing without one.
func writeMulticardNotesSection(w io.Writer, cfg config.Config) {
	for _, n := range cfg.MulticardLaunchNotes() {
		fmt.Fprintf(w, "multicard:  NOTE  %s\n", n)
	}
}

// writeTailnetZonesSection prints the tailnet DNS zones this config admits hostnames under, one
// row, with the key each came from: tailnet_suffix is this operator's own tailnet and each
// tailnet_suffixes entry is another tailnet that shared a node in (ADR 0074). Those are the zones
// netguard.TailnetURL, the cache-store host check and the research lane's refusal all judge, so a
// roster entry that is refused by name is read against this row first.
//
// It prints nothing when no zone is configured, so a green doctor stays as short as it is today:
// loopback, tailnet CGNAT-range address literals and dotless MagicDNS names need no zone, and a dotted name
// that does is reported as a finding on its own row.
func writeTailnetZonesSection(w io.Writer, cfg config.Config) {
	var parts []string
	seen := map[string]bool{}
	add := func(raw, key string) {
		z, err := netguard.ParseZone(key, raw)
		if err != nil || z == "" || seen[z] {
			return // a malformed zone already failed the config row; a repeat is one zone
		}
		seen[z] = true
		parts = append(parts, fmt.Sprintf("%s (%s)", z, key))
	}
	add(cfg.TailnetSuffix, "tailnet_suffix")
	for i, s := range cfg.TailnetSuffixes {
		add(s, fmt.Sprintf("tailnet_suffixes[%d]", i))
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintf(w, "tailnet zones: %s\n", strings.Join(parts, ", "))
}

// writeCacheServerSection prints ONE LINE PER vLLM SEAT of this box and returns how
// many have no binding — the number that makes doctor exit non-zero.
//
// The rule is the operator directive of 2026-09-10: the second device's KV store
// backs EVERY vLLM seat while that device is online. What made that rule
// unenforceable was a config that bound one `kv_cache_server.seat` to a single seat
// name, so a second seat running with no tier looked exactly like a second seat
// nobody had thought about. Here the two are different lines.
//
// An explicit opt-out PASSES: `{"seat":"…","storeless":true,"reason":"…"}` is a
// decision, and a tier that is optional by charter must let a box decline it. What
// does not pass is silence — no binding at all, or a binding switched off with
// nothing saying why.
//
// A box that declares no vllm_seats prints nothing and fails nothing: it runs no
// vLLM seat, so the gate has no subject — and the cascade deliberately STAYS on
// llama.cpp (Gemma-4 hybrids crash LMCache's V2 path, upstream #4263), which is why
// the roster is declared in config rather than sniffed from /v1/models.
func writeCacheServerSection(w io.Writer, cfg config.Config) int {
	seats := cfg.VLLMSeats
	if len(seats) == 0 {
		return 0
	}
	fmt.Fprintln(w, "cache server (one binding per vLLM seat; the store backs every seat while the second device is online):")
	unbound := 0
	for _, seat := range seats {
		b := cfg.KVCacheServers.For(seat)
		switch {
		case b == nil:
			unbound++
			fmt.Fprintf(w, "  %-24s FAIL  no kv_cache_server binding — give this seat a store, or opt out explicitly with {\"seat\":%q,\"storeless\":true,\"reason\":\"<why>\"}\n",
				seat+":", seat)
		case b.Storeless:
			fmt.Fprintf(w, "  %-24s OK    storeless by declaration: %s\n", seat+":", b.Reason)
		case !b.Enabled:
			unbound++
			fmt.Fprintf(w, "  %-24s FAIL  binding present but enabled:false and no storeless reason — turn the store on, or say why this seat runs without one\n", seat+":")
		default:
			fmt.Fprintf(w, "  %-24s OK    %s %s key_prefix=%s l1=%dGB\n",
				seat+":", b.StoreName(), b.Address, b.EffectiveKeyPrefix(), b.EffectiveL1StagingGB())
		}
	}
	for _, seat := range cfg.KVCacheServers.BoundSeatsNotDeclared(seats) {
		fmt.Fprintf(w, "  %-24s -     a binding names this seat, but vllm_seats does not list it (a store nothing reads, or a rename that left the binding behind)\n", seat+":")
	}
	return unbound
}

// writeMediaSection prints this machine's DERIVED media capability and returns
// the number of routes bound to something that is absent — the script, a model
// file its graph loads, a custom-node class, or (voice) the TTS python, its
// packages and weights. Only that class is a failure: NOT CONFIGURED is a
// legitimate machine (the task defers by design), while BOUND-BUT-MISSING is a
// config that promises a capability the box cannot deliver — which until now
// surfaced only as a defer at call time.
func writeMediaSection(w io.Writer, routes []mediacap.Route) int {
	if len(routes) == 0 {
		return 0
	}
	fmt.Fprintln(w, "media routes (derived from this config; anything a route loads that is absent — script, model file, custom node class, TTS python/weights — means the task DEFERS when called):")
	missing := 0
	for _, r := range routes {
		mark := "OK  "
		switch r.State {
		case mediacap.BoundButMissing:
			mark = "FAIL"
			missing++
		case mediacap.NotConfigured:
			mark = "-   "
		}
		fmt.Fprintf(w, "  %-22s %s  %-18s %-15s %s\n", r.Name+":", mark, string(r.State), r.Engine, r.Detail)
	}
	return missing
}

// writeSdcppDeviceSection reports which Vulkan device an sdcpp generate_image
// render will use on this box (mediacap.DetectSdcppDevice mirrors
// render/sdcpp-generate.mjs's own device-resolution order exactly), and flags it
// when that device is an integrated GPU. Prints nothing when sdcpp is not this
// box's image-gen engine or its binary is unbound — writeMediaSection already
// says so.
func writeSdcppDeviceSection(w io.Writer, cfg config.Config) {
	if cfg.ImageGenEngine != "sdcpp" || cfg.SdcppBin == "" {
		return
	}
	d := mediacap.DetectSdcppDevice(cfg.SdcppBin, os.Getenv("GGML_VK_VISIBLE_DEVICES"), nil)
	switch {
	case d.ProbeErr != "" && d.Name == "":
		fmt.Fprintf(w, "sdcpp device: ?     could not determine the Vulkan device (%s) — GGML_VK_VISIBLE_DEVICES=%s will be used as-is\n", d.ProbeErr, d.Index)
	case d.IsIGPU:
		src := "auto-selected (no discrete adapter found)"
		if d.EnvOverride {
			src = "from GGML_VK_VISIBLE_DEVICES"
		}
		fmt.Fprintf(w, "sdcpp device: WARN  Vulkan%s %s is an INTEGRATED GPU (%s) — renders will be slow; set GGML_VK_VISIBLE_DEVICES to the discrete adapter's index to override\n", d.Index, d.Name, src)
	default:
		how := "auto-selected discrete adapter"
		if d.EnvOverride {
			how = "from GGML_VK_VISIBLE_DEVICES"
		}
		fmt.Fprintf(w, "sdcpp device: OK    Vulkan%s %s (%s)\n", d.Index, d.Name, how)
	}
}

// writeVideoFamilyBindingsSection prints the RESOLVED per-family video bindings
// (gap: videogen_text_encoder and its siblings used to be a single machine-wide
// value applied to whichever family actually rendered — an explicit per-request
// `model` override could silently inherit the box's OTHER family's transformer/
// text-encoder/VAEs; bigger-models-2026-09-24.md "Interim Phase 2 round 2").
// Sorted key order for a stable diff between doctor runs. Purely informational —
// it never changes doctor's exit code, matching the sdcpp device line above.
func writeVideoFamilyBindingsSection(w io.Writer, rows []mediacap.VideoFamilyBindingRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "video family bindings (what THIS machine resolves for each family, independent of which one a request actually selects):")
	for _, r := range rows {
		tag := ""
		if r.Default {
			tag = " (this box's default)"
		}
		fmt.Fprintf(w, "  %s%s:\n", r.Family, tag)
		keys := make([]string, 0, len(r.Files))
		for k := range r.Files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// The declared license pair, as fields (CT-47; operator order 2026-10-01: no
		// warning text). A family that declares none prints nothing, like an image
		// family's route line.
		if r.License != nil && r.CommercialUse != nil {
			fmt.Fprintf(w, "    %-55s %s (commercial_use %t)\n", "license:", *r.License, *r.CommercialUse)
		}
		for _, k := range keys {
			fmt.Fprintf(w, "    %-55s %s\n", k+":", r.Files[k])
		}
	}
}

// writeModelBindingsSection prints one line per configured ComfyUI model name
// with where it was found under <comfy_dir>/models, and returns how many are
// MISSING or MISPLACED (register F-31). A box with no ComfyUI models directory
// prints nothing: there is nothing to bind there.
func writeModelBindingsSection(w io.Writer, bindings []mediacap.Binding) int {
	if len(bindings) == 0 {
		return 0
	}
	fmt.Fprintln(w, "comfyui model bindings (each name must sit in the class directory its loader node opens; MISSING/MISPLACED = the graph is rejected at render time; INCOMPLETE = a file of this name is there but it is smaller than the size the installer recorded when it downloaded it, e.g. a copy still in progress):")
	broken := 0
	for _, b := range bindings {
		mark := "OK  "
		if b.State != mediacap.BindingFound {
			mark = "FAIL"
			broken++
		}
		fmt.Fprintf(w, "  %-26s %s  %-10s %s\n", b.Key+":", mark, string(b.State), b.Detail)
	}
	return broken
}
