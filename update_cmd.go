package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/update"
)

// The floors the update verb never selects under, embedded from the one file that may name a build number.
//
//go:embed setup/floors.json
var floorsJSON []byte

// runUpdate is `local-offload update`: resolve each harness component's newest complete build from its publisher,
// verify, stage, promote as a generation, roll back on failure (docs/systems/update.md).
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.String("config", "", "the node config (read for update.channel, update.paths, update.restart); a global --config is hoisted here")
	comp := fs.String("component", "all", "llama|llama-swap|whisper|sdcpp|hyperframes|comfy-packs|all (comfy-packs is never part of all: run it under the GPU lease, docs/systems/update.md)")
	channel := fs.String("channel", "", "nightly (default) or release; only llama.cpp has a release channel")
	dry := fs.Bool("dry-run", false, "print the plan (installed, resolved, action) without downloading or changing anything")
	asJSON := fs.Bool("json", false, "print the reports as JSON")
	resolve := fs.String("resolve", "", "print the candidate (id, assets, digests) for one component and exit")
	rollback := fs.String("rollback", "", "return one component to its previous generation")
	repoint := fs.String("repoint", "", "rewrite a hand-kept llama-swap yaml to name the live llama path (needs --component llama)")
	var from []string
	fs.Func("from", "with --repoint: a llama build directory to repoint that has no build number in its name (repeatable)", func(s string) error {
		from = append(from, s)
		return nil
	})
	live := fs.String("live", "", "the live path of the single selected component (default <home>/<component>, or update.paths in config.json)")
	noGate := fs.Bool("no-gate", false, "skip the side-port smoke gate (the GPU lease and the measured comparison with the previous llama build); the offline self-check and the post-flip watch still run")
	gateChild := fs.String("gate-child", "", "internal: run the smoke measurement from this job file; started by the smoke gate under the GPU lease")
	gateResult := fs.String("gate-result", "", "internal: where --gate-child writes its verdict")
	home := fs.String("home", "", "the install root (default $OFFLOAD_HOME, else update.home in config.json, else the nearest folder above this binary that holds installed.json, else ~/offload-stack when it holds one)")
	if err := fs.Parse(args); err != nil {
		return &exitCodeError{code: 2, msg: "update: " + err.Error()}
	}

	if *gateChild != "" {
		if *gateResult == "" {
			return &exitCodeError{code: 2, msg: "update: --gate-child needs --gate-result"}
		}
		if err := update.RunGateChild(context.Background(), *gateChild, *gateResult, os.Stderr); err != nil {
			return &exitCodeError{code: 2, msg: "update: gate child: " + err.Error()}
		}
		return nil
	}

	// The update keys name commands this verb runs (update.restart) and directories it renames and prunes
	// (update.paths), so its config comes only from a file the operator named or owns: --config,
	// $LOCAL_OFFLOAD_CONFIG or ~/.local-offload/config.json, never a config.json in whatever directory the verb
	// happens to run from.
	cfgPath := updateConfigPath(scanConfigFlag(args))
	if cfgPath != "" {
		if _, serr := os.Stat(cfgPath); serr != nil {
			return &exitCodeError{code: 2, msg: "update: config " + cfgPath + " (from --config or $LOCAL_OFFLOAD_CONFIG): " + serr.Error()}
		}
	}
	cfg, lerr := config.Load(cfgPath)
	if lerr != nil {
		return &exitCodeError{code: 2, msg: "update: config " + cfgPath + ": " + lerr.Error()}
	}
	fc := update.ReadFileConfig(cfgPath)
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "update: no config file (--config, $LOCAL_OFFLOAD_CONFIG, ~/.local-offload/config.json); built-in defaults")
	} else {
		fmt.Fprintln(os.Stderr, "update: config "+cfgPath)
	}
	if *comp == update.ComfyPacks || *resolve == update.ComfyPacks || *rollback == update.ComfyPacks {
		return runUpdateComfyPacks(cfg, *comp, *resolve, *rollback, *dry, *asJSON)
	}
	root, rsrc, rerr := updateRoot(*home, fc.Home)
	if rerr != nil {
		return &exitCodeError{code: 2, msg: "update: " + rerr.Error()}
	}
	fmt.Fprintln(os.Stderr, "update: install root "+root+" ("+rsrc+")")
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	env, err := update.NewEnv(root, fc, cfg.Endpoint, floorsJSON, os.Stdout, logf)
	if err != nil {
		return &exitCodeError{code: 2, msg: "update: " + err.Error()}
	}

	if sg, ok := env.Gate.(*update.SmokeGate); ok {
		sg.ConfigPath = cfgPath
	}
	if *noGate {
		env.Gate = update.OfflineGate{}
		fmt.Fprintln(os.Stderr, "gate: --no-gate: the side-port smoke is skipped; only the offline self-check stands before promotion")
	}

	opts := update.Options{Channel: *channel, DryRun: *dry, Live: *live}
	switch {
	case *resolve != "":
		opts.ResolveOnly, opts.Components = true, []string{*resolve}
	case *rollback != "":
		opts.Rollback, opts.Components = true, []string{*rollback}
	case *comp == "all" || *comp == "":
		opts.Components = update.AllComponents
	default:
		opts.Components, opts.Explicit = []string{*comp}, true
	}
	if *live != "" && len(opts.Components) != 1 {
		return &exitCodeError{code: 2, msg: "update: --live names one component's path; pass --component"}
	}

	if len(from) > 0 && *repoint == "" {
		return &exitCodeError{code: 2, msg: "update: --from names a directory for --repoint to rewrite; pass --repoint <yaml>"}
	}
	if *repoint != "" {
		if len(opts.Components) != 1 || opts.Components[0] != update.Llama {
			return &exitCodeError{code: 2, msg: "update: --repoint rewrites llama build directories; pass --component llama"}
		}
		return runUpdateRepoint(env, *repoint, from, opts)
	}

	reports, code := env.Run(context.Background(), opts)
	if *asJSON {
		b, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Println(string(b))
	} else {
		update.PrintReports(os.Stdout, reports, opts)
	}
	if code != 0 {
		return &exitCodeError{code: code, msg: fmt.Sprintf("update: %s", summarize(reports))}
	}
	return nil
}

// runUpdateComfyPacks is the comfy-packs component: render/comfy-packs-update.mjs (beside the run-graph runner, started
// with the node executable the config names, on the ComfyUI directory it names) moves the installed node packs behind its
// import gate. The verb relays its report and exit code and takes no install root: the packs live in the ComfyUI tree,
// not in installed.json. The operator runs the verb under the GPU lease wrapper; the verb does not take the lease itself.
func runUpdateComfyPacks(cfg config.Config, comp, resolve, rollback string, dry, asJSON bool) error {
	for _, other := range []string{resolve, rollback} {
		if other != "" && other != update.ComfyPacks {
			return &exitCodeError{code: 2, msg: "update: comfy-packs cannot be combined with --resolve or --rollback of " + other}
		}
	}
	if resolve != "" && rollback != "" {
		return &exitCodeError{code: 2, msg: "update: --resolve and --rollback are separate runs"}
	}
	if comp != update.ComfyPacks && comp != "all" && comp != "" {
		return &exitCodeError{code: 2, msg: "update: --component " + comp + " cannot be combined with comfy-packs"}
	}
	rg := cfg.RunGraphScript
	if rg == "" {
		rg = "render/comfy-run-graph.mjs"
	}
	script, err := gpugen.ResolveScript(update.ComfyPacksScript(rg))
	if err != nil {
		return &exitCodeError{code: 2, msg: "update: comfy-packs: " + err.Error()}
	}
	code := update.RunComfyPacks(context.Background(), update.ComfyPacksRun{
		Node: cfg.NodePath, Script: script, ComfyDir: cfg.ComfyDir,
		DryRun: dry, Resolve: resolve != "", Rollback: rollback != "", JSON: asJSON,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if code != 0 {
		return &exitCodeError{code: code, msg: fmt.Sprintf("update: comfy-packs exited %d (the report above says why)", code)}
	}
	return nil
}

func summarize(reports []update.Report) string {
	var bad []string
	for _, r := range reports {
		if r.Action == "error" || r.Action == "rejected" || r.Action == "rolled-back" {
			bad = append(bad, fmt.Sprintf("%s %s: %s", r.Component, r.Action, r.Detail))
		}
	}
	return strings.Join(bad, "; ")
}

func runUpdateRepoint(env *update.Env, yamlPath string, from []string, opts update.Options) error {
	livePath := env.LivePath(update.Llama, opts)
	// The live build decides whether a bin segment stays: a yaml repointed at a path that is not there would name
	// programs that do not exist, and a llama-swap watching its config would reload into it.
	if _, err := os.Stat(livePath); err != nil {
		return &exitCodeError{code: 2, msg: "update: repoint: the live llama path " + livePath + " is not there; install or copy the build there first (nothing was changed)"}
	}
	res, err := update.Repoint(yamlPath, livePath, from, time.Now())
	if err != nil {
		return &exitCodeError{code: 2, msg: "update: repoint: " + err.Error()}
	}
	if res.Backup == "" {
		fmt.Printf("repoint: %s names no build directory; nothing changed\n", yamlPath)
		return nil
	}
	fmt.Printf("repoint: %s\n  backup: %s\n  %d build-directory occurrence(s) now name %s; %d LD_LIBRARY_PATH line(s) set\n",
		yamlPath, res.Backup, res.Replaced, livePath, res.LDLines)
	for _, d := range res.Diff {
		fmt.Println(d)
	}
	fmt.Println("repoint: llama-swap reads its config at start; reload it (the update verb's llama step unloads the seats, a config change needs a llama-swap restart)")
	return nil
}

// updateRoot is the install root the verb works on, and where it came from: --home, else $OFFLOAD_HOME, else
// update.home in the config, else the nearest folder that holds the installer's installed.json among the running
// binary's folder and the three above it (the Linux installer's <root>/bin, the Windows installer's <root>/harness, a
// clone built inside the root; how the fleet registry calls the verb: over ssh, with no environment), else
// ~/offload-stack when it holds an installed.json. A root that cannot be found is an error: the verb never starts a
// fresh install in a guessed folder (with the variable unset, an earlier build put a second llama under the user
// profile, on the system drive).
func updateRoot(flagHome, cfgHome string) (string, string, error) {
	if flagHome != "" {
		return flagHome, "--home", nil
	}
	if v := strings.TrimSpace(os.Getenv("OFFLOAD_HOME")); v != "" {
		return v, "$OFFLOAD_HOME", nil
	}
	if v := strings.TrimSpace(cfgHome); v != "" {
		if !filepath.IsAbs(v) {
			return "", "", fmt.Errorf("update.home in config.json is %q, not an absolute path; nothing was changed", v)
		}
		return v, "update.home in config.json", nil
	}
	holdsManifest := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "installed.json"))
		return err == nil
	}
	if exe, err := os.Executable(); err == nil {
		if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 4; i++ {
			if holdsManifest(dir) {
				return dir, "installed.json above this binary", nil
			}
			up := filepath.Dir(dir)
			if up == dir {
				break
			}
			dir = up
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		if d := filepath.Join(h, "offload-stack"); holdsManifest(d) {
			return d, "~/offload-stack", nil
		}
	}
	return "", "", errors.New("no install root: pass --home, set OFFLOAD_HOME or update.home in config.json (no installed.json in this binary's folder or the three above it, or in ~/offload-stack); nothing was changed")
}

// updateConfigPath is the config the update verb reads: an explicit --config, else $LOCAL_OFFLOAD_CONFIG, else the
// operator's own ~/.local-offload/config.json; the current directory's config.json is never a candidate here.
func updateConfigPath(flagPath string) string {
	home, _ := os.UserHomeDir()
	return config.ResolvePath(flagPath, os.Getenv("LOCAL_OFFLOAD_CONFIG"), home, func(p string) bool {
		if p == "config.json" { // the cwd candidate
			return false
		}
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir()
	})
}
