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

	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/hfinstall"
)

// The committed setup/hyperframes package.json + lock, embedded so a node that runs
// from a binary drop (no repository checkout) can re-install the composition lane's
// pinned CLI when a release moves the pin.
//
//go:embed setup/hyperframes/package.json
var hyperframesPackageJSON []byte

//go:embed setup/hyperframes/package-lock.json
var hyperframesPackageLock []byte

// runInstallHyperframes is the installers' HyperFrames step on its own, for a node that
// is already installed: a release that moves the pin runs it after the node's binary and
// render tree are swapped, because the new runner refuses every op until the install
// under hyperframes_dir holds its pin.
func runInstallHyperframes(args []string) error {
	fs := flag.NewFlagSet("install hyperframes", flag.ExitOnError)
	fs.String("config", "", "config file path")
	dir := fs.String("dir", "", "install directory (default: the config's hyperframes_dir)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
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
	res, err := hfinstall.Install(context.Background(), hfinstall.Options{
		Dir: target, Runner: runner, Node: node, Npm: npmBeside(node),
		PackageJSON: hyperframesPackageJSON, PackageLock: hyperframesPackageLock,
		Log: os.Stderr,
	})

	// The installers never rewrite an existing config, so say what this one should bind.
	var notes []string
	if err == nil {
		if !samePath(cfg.HyperframesDir, target) {
			notes = append(notes, fmt.Sprintf("hyperframes_dir in the config is %q; this install is %q", cfg.HyperframesDir, target))
		}
		if !samePath(cfg.HyperframesBrowserPath, res.BrowserPath) {
			notes = append(notes, fmt.Sprintf("set hyperframes_browser_path to %q (the config holds %q)", res.BrowserPath, cfg.HyperframesBrowserPath))
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
	verb := "already at the pin"
	if res.Reinstalled {
		verb = "installed"
	}
	fmt.Printf("OK    hyperframes %s %s in %s (chrome-headless-shell %s at %s; the runner's version check matches its pin)\n",
		res.Version, verb, res.Dir, res.ChromeVersion, res.BrowserPath)
	for _, n := range notes {
		fmt.Printf("NOTE  %s\n", n)
	}
	return nil
}

// npmBeside finds the npm that ships beside node, so an absolute node_path never pairs
// with another installation's npm on PATH. Empty = let PATH decide.
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
