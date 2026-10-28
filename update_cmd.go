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
	comp := fs.String("component", "all", "llama|llama-swap|whisper|sdcpp|hyperframes|all")
	channel := fs.String("channel", "", "nightly (default) or release; only llama.cpp has a release channel")
	dry := fs.Bool("dry-run", false, "print the plan (installed, resolved, action) without downloading or changing anything")
	asJSON := fs.Bool("json", false, "print the reports as JSON")
	resolve := fs.String("resolve", "", "print the candidate (id, assets, digests) for one component and exit")
	rollback := fs.String("rollback", "", "return one component to its previous generation")
	repoint := fs.String("repoint", "", "rewrite a hand-kept llama-swap yaml to name the live llama path (needs --component llama)")
	live := fs.String("live", "", "the live path of the single selected component (default <home>/<component>, or update.paths in config.json)")
	noGate := fs.Bool("no-gate", false, "skip the side-port smoke gate (the GPU lease and the measured comparison with the previous llama build); the offline self-check and the post-flip watch still run")
	gateChild := fs.String("gate-child", "", "internal: run the smoke measurement from this job file; started by the smoke gate under the GPU lease")
	gateResult := fs.String("gate-result", "", "internal: where --gate-child writes its verdict")
	home := fs.String("home", "", "the install root (default $OFFLOAD_HOME, else the folder above this binary's bin/ when it holds installed.json, else ~/offload-stack when it holds one)")
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
	root, rerr := updateRoot(*home)
	if rerr != nil {
		return &exitCodeError{code: 2, msg: "update: " + rerr.Error()}
	}
	fmt.Fprintln(os.Stderr, "update: install root "+root)
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
		opts.Components = []string{*comp}
	}
	if *live != "" && len(opts.Components) != 1 {
		return &exitCodeError{code: 2, msg: "update: --live names one component's path; pass --component"}
	}

	if *repoint != "" {
		if len(opts.Components) != 1 || opts.Components[0] != update.Llama {
			return &exitCodeError{code: 2, msg: "update: --repoint rewrites llama build directories; pass --component llama"}
		}
		return runUpdateRepoint(env, *repoint, opts)
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

func summarize(reports []update.Report) string {
	var bad []string
	for _, r := range reports {
		if r.Action == "error" || r.Action == "rejected" || r.Action == "rolled-back" {
			bad = append(bad, fmt.Sprintf("%s %s: %s", r.Component, r.Action, r.Detail))
		}
	}
	return strings.Join(bad, "; ")
}

func runUpdateRepoint(env *update.Env, yamlPath string, opts update.Options) error {
	livePath := env.LivePath(update.Llama, opts)
	res, err := update.Repoint(yamlPath, livePath, time.Now())
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

// updateConfigPath is the config the update verb reads: an explicit --config, else $LOCAL_OFFLOAD_CONFIG, else the
// operator's own ~/.local-offload/config.json; the current directory's config.json is never a candidate here.
// updateRoot is the install root the verb works on: --home, else $OFFLOAD_HOME, else the folder above the running
// binary when the binary sits in <root>/bin beside the installer's installed.json (the installers' layout, and how the
// fleet registry calls the verb: over ssh, with no environment), else ~/offload-stack when it holds an installed.json.
// A root that cannot be found is an error: the verb never starts a fresh install in a guessed folder (with the
// variable unset, an earlier build put a second llama under the user profile, on the system drive).
func updateRoot(flagHome string) (string, error) {
	if flagHome != "" {
		return flagHome, nil
	}
	if v := strings.TrimSpace(os.Getenv("OFFLOAD_HOME")); v != "" {
		return v, nil
	}
	holdsManifest := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "installed.json"))
		return err == nil
	}
	if exe, err := os.Executable(); err == nil {
		if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = real
		}
		if bin := filepath.Dir(exe); strings.EqualFold(filepath.Base(bin), "bin") && holdsManifest(filepath.Dir(bin)) {
			return filepath.Dir(bin), nil
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		if d := filepath.Join(h, "offload-stack"); holdsManifest(d) {
			return d, nil
		}
	}
	return "", errors.New("no install root: pass --home or set OFFLOAD_HOME (no installed.json beside this binary's bin folder or in ~/offload-stack); nothing was changed")
}

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
