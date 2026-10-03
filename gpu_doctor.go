package main

// `local-offload gpu doctor`: the reader audit (plan P3, invariant I7).
//
// Card-scoped leases are written only on a host whose readers all understand the format,
// because a reader that predates it reads a directory holding only card-scoped leases as
// a free card (see internal/gpulease/audit.go for the mechanism and why the build version
// cannot tell). This verb finds the readers and says which ones fail:
//
//   - every harness binary it can reach: the running one, the install directory (the
//     node-swap backups a rollback restores sit beside it), binaries on PATH (by name),
//     every directory given with --scan (point it at a media repository that carries its
//     own wrapper copy: a copy under ANY name is found there by the lease package path it
//     carries), the image of every running harness process (the agent binary included)
//     and of every lease holder or waiter;
//   - every Node `gpu-lock.mjs` under those directories.
//
// Nothing is executed and nothing is changed unless --write-audit is given, which records
// the verdict as the reader-audit marker (red as well as green, so a stale green cannot
// outlive a new old binary). Exit status is non-zero when the audit is not green.
//
// WHAT GREEN MEANS: every reader the audit REACHED is aware. It does not know about a
// Node reader or a binary in a directory nobody told it about; that is what --scan is
// for, and the output lists what was scanned AND what was not entered (directories under
// the depth cap, node_modules, .git) so the gap is visible. What it cannot reach by
// design: a renamed copy that is neither under a scan root nor running as a lease holder,
// a Node reader not called gpu-lock.mjs, and a binary a packer has compressed.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// buildMarker is this binary's own build marker; the audit reads it back from other
// binaries to show their versions, and a binary that carries it is one built after the
// marker existed.
const buildMarker = gpulease.BuildMarkerPrefix + version

// Seams, so tests audit a fixture host and not the machine they run on.
var (
	doctorExe                 = os.Executable
	doctorPathEnv             = func() string { return os.Getenv("PATH") }
	runningImagesFn           = gpulease.RunningImages
	doctorOut       io.Writer = os.Stdout
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runGPUDoctor(args []string) error {
	fs := flag.NewFlagSet("gpu doctor", flag.ExitOnError)
	fs.String("config", "", "config file path")
	var scan stringList
	fs.Var(&scan, "scan", "an extra directory to search for harness binaries and gpu-lock.mjs readers (repeatable); point it at every media repository that carries its own copy")
	write := fs.Bool("write-audit", false, "record the verdict as the reader-audit marker (green or red); card-scoped leases are written only on a host whose marker is green and whose config asks for them")
	asJSON := fs.Bool("json", false, "emit JSON")
	depth := fs.Int("depth", 0, "how many directory levels below each scan root to search (0 = the default, 3)")
	_ = fs.Parse(args)

	m, err := openLease(fs)
	if err != nil {
		return err
	}
	cfg := loadCfg(fs)

	exe, exeErr := doctorExe()
	opts := gpulease.AuditOptions{SelfPath: exe, MaxDepth: *depth}
	var notes []string
	if exeErr != nil {
		notes = append(notes, fmt.Sprintf("could not resolve the running executable: %v", exeErr))
	}
	if exe != "" {
		dir := filepath.Dir(exe)
		opts.Roots = append(opts.Roots, dir)
	}
	for _, s := range scan {
		opts.Roots = append(opts.Roots, s)
	}
	// The directories the configured render scripts live in: `gpu-lock.mjs` sits beside
	// them, and an install keeps its binary and its render tree in different places.
	opts.Roots = appendUniqueRoots(opts.Roots, renderScriptDirs(cfg, exe)...)
	opts.Files = pathCandidates(doctorPathEnv())

	holders := map[int]string{}
	for _, l := range m.Leases() {
		if l.PID > 0 {
			holders[l.PID] = "lease holder"
		}
	}
	for _, w := range m.Waiters() {
		if _, dup := holders[w.PID]; !dup && w.PID > 0 {
			holders[w.PID] = "waiter"
		}
	}
	opts.Images, opts.ImagesErr = runningImagesFn(func(pid int, name string) string {
		if why, ok := holders[pid]; ok {
			return why
		}
		if gpulease.IsHarnessBinaryName(name, 1<<20) || isHarnessProcessName(name) {
			return "harness process"
		}
		return ""
	})

	rep := gpulease.Audit(opts)
	before := m.ReaderAuditResult()
	if *write {
		if werr := m.WriteReaderAudit(rep); werr != nil {
			return werr
		}
	}
	after := m.ReaderAuditResult()

	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"green": rep.Green, "items": rep.Items, "reasons": rep.Reasons, "notes": notes,
			"marker": after, "marker_before": before, "marker_path": m.ReaderAuditPath(),
			"config_card_scoped_leases": cfg.GPUCardScopedLeases, "writer_enabled": m.CardScoped(),
			"signature": gpulease.FormatSignature, "build_marker": buildMarker, "not_searched": rep.NotSearched,
		}, "", "  ")
		fmt.Fprintln(doctorOut, string(b))
	} else {
		printDoctor(doctorOut, rep, notes, scanRoots(opts), m, cfg.GPUCardScopedLeases, before, after, *write)
	}
	if !rep.Green {
		return fmt.Errorf("reader audit is not green: %d finding(s)", len(rep.Reasons))
	}
	return nil
}

// renderScriptDirs lists the existing directories that hold the configured render
// scripts. A relative script path is tried against the working directory, the executable's
// directory and its parent, because that is where an install keeps its render tree.
func renderScriptDirs(cfg config.Config, exe string) []string {
	scripts := []string{cfg.ImageGenScript, cfg.InpaintScript, cfg.GenEditScript, cfg.UpscaleScript, cfg.VideoGenScript,
		cfg.RunGraphScript, cfg.VoiceGenScript, cfg.MusicGenScript, cfg.AnimateGenScript, cfg.ComposeScript}
	var bases []string
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	if exe != "" {
		bases = append(bases, filepath.Dir(exe), filepath.Dir(filepath.Dir(exe)))
	}
	var out []string
	for _, sc := range scripts {
		if strings.TrimSpace(sc) == "" {
			continue
		}
		var candidates []string
		if filepath.IsAbs(sc) {
			candidates = []string{filepath.Dir(sc)}
		} else {
			for _, b := range bases {
				candidates = append(candidates, filepath.Dir(filepath.Join(b, sc)))
			}
		}
		for _, c := range candidates {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				out = appendUniqueRoots(out, c)
			}
		}
	}
	return out
}

func appendUniqueRoots(list []string, more ...string) []string {
	for _, m := range more {
		dup := false
		for _, l := range list {
			if strings.EqualFold(filepath.Clean(l), filepath.Clean(m)) {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, m)
		}
	}
	return list
}

// isHarnessProcessName catches the harness's process names as the OS reports them, which
// on Windows is the executable's base name and on Linux may be the comm name (no .exe).
func isHarnessProcessName(name string) bool {
	l := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(l, "local-offload") || strings.HasPrefix(l, "offload-harness") || strings.HasPrefix(l, "local-agent")
}

// pathCandidates lists harness binaries directly inside PATH directories, without walking
// them (a system directory is neither small nor ours).
func pathCandidates(pathEnv string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil || !gpulease.IsHarnessBinaryName(e.Name(), info.Size()) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if !seen[strings.ToLower(p)] {
				seen[strings.ToLower(p)] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func scanRoots(o gpulease.AuditOptions) []string { return o.Roots }

func printDoctor(w io.Writer, rep gpulease.AuditReport, notes, roots []string, m *gpulease.Manager,
	configOn bool, before, after string, wrote bool) {
	fmt.Fprintf(w, "gpu doctor: reader audit for the GPU lease on this host (state root %s)\n", m.Root())
	fmt.Fprintf(w, "  card-scoped leases: config gpu_card_scoped_leases=%v, reader-audit marker %s, writer %s\n",
		configOn, after, onOff(m.CardScoped()))
	fmt.Fprintf(w, "  this binary: build %s, format signature %s\n", version, gpulease.FormatSignature)
	for _, kind := range []struct{ k, label string }{{gpulease.KindBinary, "binaries"}, {gpulease.KindNodeReader, "Node readers (gpu-lock.mjs)"}} {
		var items []gpulease.AuditItem
		for _, it := range rep.Items {
			if it.Kind == kind.k {
				items = append(items, it)
			}
		}
		fmt.Fprintf(w, "  %s (%d):\n", kind.label, len(items))
		for _, it := range items {
			status := "OK  "
			switch {
			case it.Err != "":
				status = "ERR "
			case !it.Aware:
				status = "OLD "
			}
			ver := ""
			if it.Kind == gpulease.KindBinary {
				ver = "  build " + it.Version
				if it.Version == "" {
					ver = "  build unknown (no build marker)"
				}
			}
			fmt.Fprintf(w, "    %s %s%s\n", status, it.Path, ver)
			extra := strings.Join(it.Found, "; ")
			if len(it.PIDs) > 0 {
				extra += fmt.Sprintf("  [running as pid %v]", it.PIDs)
			}
			if it.Err != "" {
				extra += "  error: " + it.Err
			}
			fmt.Fprintf(w, "         found: %s\n", extra)
		}
	}
	if len(roots) > 0 {
		fmt.Fprintf(w, "  scanned: %s (pass --scan <dir> for every media repository that carries its own copy)\n", strings.Join(roots, ", "))
	}
	if len(rep.NotSearched) > 0 {
		const show = 12
		fmt.Fprintf(w, "  not searched (%d; pass --scan <dir> for any that matter, or --depth N to search deeper):\n", len(rep.NotSearched))
		for i, n := range rep.NotSearched {
			if i == show {
				fmt.Fprintf(w, "    ... and %d more (`gpu doctor --json` lists them all)\n", len(rep.NotSearched)-show)
				break
			}
			fmt.Fprintf(w, "    %s\n", n)
		}
	}
	for _, n := range notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	if len(rep.Reasons) > 0 {
		fmt.Fprintln(w, "  findings:")
		for _, r := range rep.Reasons {
			fmt.Fprintf(w, "    - %s\n", r)
		}
	}
	if rep.Green && len(rep.NotSearched) > 0 {
		fmt.Fprintf(w, "  verdict: GREEN, with %d director(ies) not searched (listed above): every reader this audit reached understands the per-epoch fence\n", len(rep.NotSearched))
	} else if rep.Green {
		fmt.Fprintln(w, "  verdict: GREEN: every reader this audit reached understands the per-epoch fence")
	} else {
		fmt.Fprintln(w, "  verdict: NOT GREEN: replace or remove the readers marked OLD/ERR (a node-swap backup counts: a rollback restores it)")
	}
	switch {
	case wrote:
		fmt.Fprintf(w, "  marker: wrote %s (was %s, now %s)\n", m.ReaderAuditPath(), before, after)
	case rep.Green:
		fmt.Fprintf(w, "  marker: not written; run with --write-audit to record it (now %s)\n", after)
	default:
		fmt.Fprintf(w, "  marker: not written (now %s); a green marker is what enables card-scoped leases\n", after)
	}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "off"
}
