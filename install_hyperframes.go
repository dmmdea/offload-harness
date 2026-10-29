package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/hfinstall"
)

// The committed setup/hyperframes package.json (hyperframes: "latest") and the floors file,
// embedded so a node that runs from a binary drop (no repository checkout) can install the
// composition lane's CLI: the newest npm release, at or above the floor in setup/floors.json.
//
//go:embed setup/hyperframes/package.json
var hyperframesPackageJSON []byte

//go:embed setup/floors.json
var hyperframesFloorsJSON []byte

// runInstallHyperframes is the installers' HyperFrames step on its own, for a node that
// is already installed: it brings hyperframes_dir to the newest npm release (never below the
// floor in setup/floors.json, never a downgrade) and records the version in installed.json.
func runInstallHyperframes(args []string) error {
	fs := flag.NewFlagSet("install hyperframes", flag.ExitOnError)
	fs.String("config", "", "config file path")
	dir := fs.String("dir", "", "install directory (default: the config's hyperframes_dir)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	timeout := fs.Duration("timeout", 45*time.Minute, "give up after this long (npm install, the signature audit and the browser download included)")
	_ = fs.Parse(args)
	cfg, _ := loadCfgWithSource(fs)

	if cfg.ComposeScript == "" {
		return fmt.Errorf("install hyperframes: compose_script is unset, so this box has no composition lane (the installers bind it on first install when node >= 22)")
	}
	runner, err := gpugen.ResolveScript(cfg.ComposeScript)
	if err != nil {
		return fmt.Errorf("install hyperframes: compose_script: %w", err)
	}
	target := *dir
	if target == "" {
		target = cfg.HyperframesDir
	}
	node := cfg.NodePath
	if node == "" {
		node = "node"
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	floor, ferr := hfinstall.FloorFromJSON(hyperframesFloorsJSON)
	if ferr != nil {
		return fmt.Errorf("install hyperframes: %w", ferr)
	}
	res, err := hfinstall.Install(ctx, hfinstall.Options{
		Dir: target, Runner: runner, Node: node, Npm: npmBeside(node),
		PackageJSON: hyperframesPackageJSON, Floor: floor,
		ManifestPath: fleetnode.InstalledJSONPath(),
		Log:          os.Stderr,
	})

	// The installers never rewrite an existing config, so say what this one should bind.
	// HyperFrames fetches its own Chrome; the harness only reports where it put it.
	var notes []string
	if err == nil {
		if !samePath(cfg.HyperframesDir, res.Dir) {
			notes = append(notes, fmt.Sprintf("hyperframes_dir in the config is %q; this install is %q", cfg.HyperframesDir, res.Dir))
		}
		// "auto" follows whatever browser the installed release resolves, so there is nothing to compare;
		// an explicit path binds one file and is flagged when it is not the release's own browser.
		if !cfg.HyperframesBrowserAuto() && !samePath(cfg.HyperframesBrowserPath, res.BrowserPath) {
			notes = append(notes, fmt.Sprintf("hyperframes_browser_path in the config is %q; HyperFrames' own browser is at %q - set it to %q so the config follows the installed release instead of one browser version", cfg.HyperframesBrowserPath, res.BrowserPath, config.BrowserAuto))
		}
	}
	if *asJSON {
		out := map[string]any{"ok": err == nil, "result": res, "notes": notes}
		if err != nil {
			out["error"] = err.Error()
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return err
	}
	if err != nil {
		return err
	}
	verb := "already the newest"
	if res.Reinstalled {
		verb = "installed"
	}
	fmt.Printf("OK    hyperframes %s %s in %s (browser at %s; the runner's version check passed the floor)\n",
		res.Version, verb, res.Dir, res.BrowserPath)
	if res.PreviousTree != "" {
		fmt.Printf("NOTE  the replaced tree is kept at %s; to roll back, rename it to node_modules and put package.json.prev back\n", res.PreviousTree)
	}
	for _, n := range notes {
		fmt.Printf("NOTE  %s\n", n)
	}
	return nil
}

// npmBeside prefers the npm that ships beside an absolute node_path; otherwise it is the
// npm on PATH (or "" to let PATH decide). Either way hfinstall puts node's directory first
// on PATH for every command, so npm and the scripts it starts run under the configured node.
func npmBeside(node string) string {
	if !filepath.IsAbs(node) {
		return ""
	}
	names := []string{"npm"}
	if runtime.GOOS == "windows" {
		names = []string{"npm.cmd", "npm.exe"}
	}
	for _, n := range names {
		p := filepath.Join(filepath.Dir(node), n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("npm"); err == nil {
		return p
	}
	return ""
}

func samePath(a, b string) bool {
	clean := func(p string) string {
		p = filepath.Clean(filepath.FromSlash(p))
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		return p
	}
	return clean(a) == clean(b)
}
