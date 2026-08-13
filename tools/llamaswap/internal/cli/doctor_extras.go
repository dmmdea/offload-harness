// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): llama-swap-specific doctor checks.
//
// The generated doctor answers framework questions (config path, reachability,
// cache freshness). These answer deployment questions the framework cannot know
// to ask. They are wired in ADDITIVELY: doctor.go calls runDoctorExtras and
// renderDoctorExtras, and every check lives here. See
// .printing-press-patches/internal-cli-doctor.go.md for the reprint guard.
// Not a command: no pp:data-source marker.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
	"llamaswap-pp-cli/internal/mirror"
)

// doctorFinding is one deployment-level check result.
type doctorFinding struct {
	// Check is the stable identifier a script branches on.
	Check string `json:"check"`
	// Severity is ok, info, warn, or error.
	Severity string `json:"severity"`
	// Detail is the finding in one sentence.
	Detail string `json:"detail"`
	// Fix is the concrete next action, when there is one. Never a command
	// this CLI would run itself — surfaced, not executed.
	Fix string `json:"fix,omitempty"`
}

const (
	doctorOK    = "ok"
	doctorInfo  = "info"
	doctorWarn  = "warn"
	doctorError = "error"
)

// doctorExtraCheck is one check. Each is a pure-ish function of the live
// environment so a new one can be added without touching the generated doctor
// a second time.
type doctorExtraCheck func(ctx context.Context, flags *rootFlags) doctorFinding

// doctorExtraChecks is the registry. Order is display order.
var doctorExtraChecks = []doctorExtraCheck{
	doctorCheckLANExposure,
	doctorCheckStorePath,
	doctorCheckConfigDirMode,
	doctorCheckKeepSetAnswering,
	doctorCheckVersionCapabilities,
	doctorCheckSurfaceDrift,
	doctorCheckBackendKind,
}

// runDoctorExtras executes every registered check and files the results under
// report["llamaswap"].
//
// The value is a map with a "status" key on purpose: the generated
// doctorExitForFailOn inspects map values for exactly that field, so
// `doctor --fail-on warn` trips on a LAN-open finding without the generated
// gate needing to know this package exists.
func runDoctorExtras(ctx context.Context, flags *rootFlags, report map[string]any) {
	if report == nil {
		return
	}
	findings := make([]doctorFinding, 0, len(doctorExtraChecks))
	worst := doctorOK
	for _, check := range doctorExtraChecks {
		f := check(ctx, flags)
		if f.Check == "" {
			continue
		}
		findings = append(findings, f)
		worst = doctorWorse(worst, f.Severity)
	}
	report["llamaswap"] = map[string]any{
		"status":   worst,
		"findings": findings,
	}
}

// doctorWorse returns the more severe of two severities.
func doctorWorse(a, b string) string {
	rank := map[string]int{doctorOK: 0, doctorInfo: 1, doctorWarn: 2, doctorError: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// renderDoctorExtras prints the findings block for human output.
func renderDoctorExtras(w io.Writer, report map[string]any) {
	section, ok := report["llamaswap"].(map[string]any)
	if !ok {
		return
	}
	findings, ok := section["findings"].([]doctorFinding)
	if !ok || len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "  llama-swap deployment:\n")
	for _, f := range findings {
		indicator := green("OK")
		switch f.Severity {
		case doctorWarn:
			indicator = yellow("WARN")
		case doctorInfo:
			indicator = yellow("INFO")
		case doctorError:
			indicator = red("FAIL")
		}
		fmt.Fprintf(w, "    %s %s: %s\n", indicator, f.Check, f.Detail)
		if f.Fix != "" {
			fmt.Fprintf(w, "         fix: %s\n", f.Fix)
		}
	}
}

// ---------------------------------------------------------------- checks

// doctorListenPattern reads the bind address out of the launcher's command
// line. The launcher is authoritative: the bind address is a PROCESS argument,
// not a YAML key, so no amount of reading the config can answer this.
var doctorListenPattern = regexp.MustCompile(`(?i)--?listen[= ]+(\S+)`)

// doctorLogListenPattern is the fallback: llama-swap announces its bind
// address in its own log on startup.
var doctorLogListenPattern = regexp.MustCompile(`llama-swap listening on\s+(\S+)`)

// doctorCheckLANExposure warns when the proxy is bound to every interface with
// no API keys configured.
//
// This is not theoretical on this deployment: llama-swap itself logs
// "reachable by all hosts on the network" at startup, and with apiKeys unset
// anyone who can route to the box can list the roster, load a model, and spend
// the GPU. It is a WARN rather than an ERROR because on a Tailscale-only
// network that may be exactly the intent — but it should be a decision, not a
// default nobody noticed.
func doctorCheckLANExposure(ctx context.Context, flags *rootFlags) doctorFinding {
	f := doctorFinding{Check: "listen-exposure", Severity: doctorOK}
	bind, source := doctorDetectBind(ctx, flags)
	if bind == "" {
		f.Severity = doctorInfo
		f.Detail = "could not determine the bind address (no launcher script and no startup line in the log buffer)"
		return f
	}
	keys := -1
	if cf, err := doctorLoadConfig(); err == nil {
		keys = cf.APIKeyCount
	}
	host := bind
	if i := strings.LastIndex(bind, ":"); i > 0 {
		host = bind[:i]
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "http://"), "https://")
	wideOpen := host == "0.0.0.0" || host == "" || host == "*" || host == "[::]" || host == "::"

	switch {
	case wideOpen && keys == 0:
		f.Severity = doctorWarn
		f.Detail = fmt.Sprintf("LAN-OPEN: bound to %s (%s) with NO apiKeys configured — any host that can route here can list models, trigger multi-GB loads, and spend the GPU", bind, source)
		f.Fix = "either restrict the launcher to --listen 127.0.0.1:11436, or add an apiKeys list to the llama-swap YAML. Both are operator edits; this CLI never writes either file."
	case wideOpen:
		f.Severity = doctorInfo
		f.Detail = fmt.Sprintf("bound to %s (%s) but %d apiKey(s) are configured, so the open bind is authenticated", bind, source, keys)
	case keys == 0:
		f.Detail = fmt.Sprintf("bound to %s (%s), loopback-only; apiKeys unset is fine at this scope", bind, source)
	default:
		f.Detail = fmt.Sprintf("bound to %s (%s) with %d apiKey(s)", bind, source, keys)
	}
	return f
}

// doctorDetectBind finds the bind address from the launcher script first, the
// live log buffer second.
func doctorDetectBind(ctx context.Context, flags *rootFlags) (string, string) {
	if script, text, ok := doctorReadLauncher(); ok {
		if m := doctorListenPattern.FindStringSubmatch(text); m != nil {
			return m[1], "from " + filepath.Base(script)
		}
	}
	if cliutil.IsVerifyEnv() {
		return "", ""
	}
	base, err := spineBaseURL(flags)
	if err != nil {
		return "", ""
	}
	logText, lerr := glueFetchLogs(ctx, flags, base)
	if lerr != nil {
		return "", ""
	}
	if m := doctorLogListenPattern.FindStringSubmatch(logText); m != nil {
		return m[1], "from the proxy's startup log line"
	}
	if strings.Contains(logText, "reachable by all hosts on the network") {
		return "0.0.0.0", "from the proxy's own network-exposure warning"
	}
	return "", ""
}

// doctorReadLauncher locates the launcher next to the llama-swap config and
// returns its text. The launcher carries the process arguments — bind address,
// --config vs --config-dir, --watch-config — none of which appear in the YAML.
func doctorReadLauncher() (string, string, bool) {
	cfgPath, err := lsconfig.DefaultConfigPath()
	if err != nil {
		return "", "", false
	}
	dir := filepath.Dir(cfgPath)
	for _, name := range []string{"start-llama-swap.cmd", "start-llama-swap.bat", "start-llama-swap.ps1", "start-llama-swap.sh"} {
		p := filepath.Join(dir, name)
		if raw, rerr := os.ReadFile(p); rerr == nil {
			return p, string(raw), true
		}
	}
	return "", "", false
}

// doctorLoadConfig parses the llama-swap YAML read-only.
func doctorLoadConfig() (*lsconfig.File, error) {
	path, err := lsconfig.DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return lsconfig.Load(path, lsconfig.LoadOptions{})
}

// doctorCheckStorePath reports whether llama-swap's own SQLite persistence is
// enabled.
//
// With store.path unset, the activity ring and metrics live only in memory and
// a restart destroys them. This CLI's own mirror covers that, but belt and
// braces are cheap here: the server's persistence survives even when nobody
// runs 'sync'.
func doctorCheckStorePath(_ context.Context, _ *rootFlags) doctorFinding {
	f := doctorFinding{Check: "store-persistence", Severity: doctorOK}
	cf, err := doctorLoadConfig()
	if err != nil {
		f.Severity = doctorInfo
		f.Detail = "llama-swap YAML unreadable (" + err.Error() + "); persistence state unknown"
		return f
	}
	if strings.TrimSpace(cf.StorePath) != "" {
		f.Detail = "upstream persistence enabled (store.path = " + cf.StorePath + ")"
		return f
	}
	f.Severity = doctorInfo
	f.Detail = "store.path is unset: llama-swap keeps activity and metrics in memory only, so a restart destroys them"
	f.Fix = "llama-swap v236+ persists to SQLite when store.path is set. Adding it complements this CLI's mirror rather than replacing it — the mirror still captures what the ring drops between restarts."
	return f
}

// doctorCheckConfigDirMode reports whether fragment mode is active.
//
// This check exists to explain an ABSENCE. There are no `models add` /
// `models rm` commands in this CLI, and that is deliberate: they only work when
// llama-swap runs with -config-dir, and shipping commands that are inert on the
// actual deployment would mean shipping a green test for a no-op.
func doctorCheckConfigDirMode(_ context.Context, _ *rootFlags) doctorFinding {
	f := doctorFinding{Check: "config-dir-mode", Severity: doctorInfo}
	script, text, ok := doctorReadLauncher()
	if !ok {
		f.Detail = "launcher script not found; cannot tell whether -config-dir fragment mode is enabled"
		return f
	}
	hasDir := strings.Contains(text, "-config-dir")
	hasWatch := strings.Contains(text, "-watch-config")
	switch {
	case hasDir:
		f.Severity = doctorOK
		f.Detail = "fragment mode IS enabled (-config-dir in " + filepath.Base(script) + ")"
		f.Fix = "per-model YAML fragments are supported on this deployment; edits still go through the operator, not this CLI."
	default:
		f.Detail = "fragment mode is NOT enabled (" + filepath.Base(script) + " passes a single --config, no -config-dir" +
			map[bool]string{true: ", but -watch-config is on", false: " and no -watch-config"}[hasWatch] + ")"
		f.Fix = "this is why the CLI has no 'models add/rm/enable/disable': those write YAML fragments that only a -config-dir deployment reads. Use 'config lint' and 'config apply' against the single file instead."
	}
	return f
}

// doctorCheckKeepSetAnswering reports whether every protected model is both
// resident and answering.
//
// Three distinct states, and only the third one means the memory stack works:
// listed in the roster, resident in VRAM, and answering its own health probe. A
// non-resident member is NOT probed — any /upstream request auto-starts the
// model, which would convert the finding into a multi-GB load.
func doctorCheckKeepSetAnswering(ctx context.Context, flags *rootFlags) doctorFinding {
	f := doctorFinding{Check: "keepset-answering", Severity: doctorOK}
	if cliutil.IsVerifyEnv() {
		f.Severity = doctorInfo
		f.Detail = "skipped under PRINTING_PRESS_VERIFY=1 (no live reads)"
		return f
	}
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	if keep.Empty() {
		f.Severity = doctorWarn
		f.Detail = "keep-set is EMPTY: nothing is structurally protected from 'unload --all'"
		f.Fix = "set " + mirror.EnvYAMLPath + " to the llama-swap config, or add a keep_set array to this CLI's config.json"
		return f
	}
	c, err := spineClient(flags)
	if err != nil {
		f.Severity = doctorInfo
		f.Detail = "keep-set residency not checked: " + err.Error()
		return f
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	running, err := c.Running(probeCtx)
	if err != nil {
		f.Severity = doctorInfo
		f.Detail = "keep-set residency not checked (/running unreadable: " + err.Error() + ")"
		return f
	}
	resident := map[string]bool{}
	for _, r := range running {
		resident[strings.ToLower(r.Model)] = true
	}
	var missing, notAnswering, ok []string
	for _, m := range keep.Members {
		if !resident[strings.ToLower(m.ID)] {
			missing = append(missing, m.ID)
			continue
		}
		status, perr := c.UpstreamHealth(probeCtx, m.ID)
		if perr != nil || status < 200 || status >= 300 {
			notAnswering = append(notAnswering, m.ID)
			continue
		}
		ok = append(ok, m.ID)
	}
	switch {
	case len(missing) > 0 || len(notAnswering) > 0:
		f.Severity = doctorWarn
		parts := []string{}
		if len(missing) > 0 {
			parts = append(parts, "not resident: "+strings.Join(missing, ", "))
		}
		if len(notAnswering) > 0 {
			parts = append(parts, "resident but NOT answering: "+strings.Join(notAnswering, ", "))
		}
		f.Detail = "keep-set degraded — " + strings.Join(parts, "; ")
		f.Fix = "run 'llamaswap-pp-cli keepset status' for the per-member detail, and 'load <model> --wait' to bring one back"
	default:
		f.Detail = fmt.Sprintf("all %d keep-set member(s) resident and answering (%s)", len(ok), strings.Join(ok, ", "))
	}
	return f
}

// doctorCapability is one version-gated feature.
type doctorCapability struct {
	// MinVersion is the first llama-swap major version that shipped it.
	MinVersion int
	// Name is the feature, phrased as the command that depends on it.
	Name string
}

// doctorCapabilityMap maps this CLI's version-sensitive surfaces to the
// llama-swap build that first supported them. v249 (the reference deployment)
// satisfies every row.
var doctorCapabilityMap = []doctorCapability{
	{212, "metrics (Prometheus /metrics)"},
	{226, "load (UI-era model warm)"},
	{229, "captures / captures export (request-response capture)"},
	{236, "store.path (upstream SQLite persistence)"},
	{241, "profile (profiles API)"},
	{247, "hw (hardware inventory API)"},
}

// doctorVersionPattern extracts the numeric part of a version like "v249".
var doctorVersionPattern = regexp.MustCompile(`v?(\d+)`)

// doctorCheckVersionCapabilities compares the running build against the
// capability map, so "your build lacks X" is a fact rather than a 404 surfacing
// as a mysterious error later.
func doctorCheckVersionCapabilities(ctx context.Context, flags *rootFlags) doctorFinding {
	f := doctorFinding{Check: "version-capabilities", Severity: doctorOK}
	if cliutil.IsVerifyEnv() {
		f.Severity = doctorInfo
		f.Detail = "skipped under PRINTING_PRESS_VERIFY=1 (no live reads)"
		return f
	}
	var ver struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := mcGetJSON(probeCtx, flags, "/api/version", 10*time.Second, &ver); err != nil {
		f.Severity = doctorInfo
		f.Detail = "could not read /api/version (" + err.Error() + "); capability gating unavailable"
		return f
	}
	m := doctorVersionPattern.FindStringSubmatch(ver.Version)
	if m == nil {
		f.Severity = doctorInfo
		f.Detail = "unrecognized version string " + strconv.Quote(ver.Version) + "; capability gating unavailable"
		return f
	}
	n, _ := strconv.Atoi(m[1])
	var lacking []string
	for _, feat := range doctorCapabilityMap {
		if n < feat.MinVersion {
			lacking = append(lacking, fmt.Sprintf("%s (needs v%d)", feat.Name, feat.MinVersion))
		}
	}
	if len(lacking) == 0 {
		f.Detail = fmt.Sprintf("llama-swap %s (commit %s) supports every version-gated command in this CLI", ver.Version, ver.Commit)
		return f
	}
	f.Severity = doctorWarn
	f.Detail = fmt.Sprintf("llama-swap %s is older than some commands require — unavailable: %s", ver.Version, strings.Join(lacking, "; "))
	f.Fix = "upgrade llama-swap, or avoid those commands on this build; each one reports a 404 as 'feature absent' rather than as an error."
	return f
}
