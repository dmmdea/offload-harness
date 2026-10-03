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
	timeout := fs.Duration("timeout", 45*time.Minute, "give up after this long (npm ci, the signature audit and the browser download included)")
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
	res, err := hfinstall.Install(ctx, hfinstall.Options{
		Dir: target, Runner: runner, Node: node, Npm: npmBeside(node),
		PackageJSON: hyperframesPackageJSON, PackageLock: hyperframesPackageLock,
		Log: os.Stderr,
	})

	// The installers never rewrite an existing config, so say what this one should bind.
	// A config bound to ANOTHER Chrome build than the pin resolves is an error, not a note:
	// the runner would keep rendering with the old browser and status would not notice.
	var notes []string
	if err == nil {
		if !samePath(cfg.HyperframesDir, res.Dir) {
			notes = append(notes, fmt.Sprintf("hyperframes_dir in the config is %q; this install is %q", cfg.HyperframesDir, res.Dir))
		}
		if !samePath(cfg.HyperframesBrowserPath, res.BrowserPath) {
			if res.ChromeVersion != "" && !strings.Contains(filepath.ToSlash(cfg.HyperframesBrowserPath), res.ChromeVersion) {
				err = fmt.Errorf("the config binds hyperframes_browser_path %q, which is not the pinned Chrome %s: set it to %q", cfg.HyperframesBrowserPath, res.ChromeVersion, res.BrowserPath)
			} else {
				notes = append(notes, fmt.Sprintf("the config binds the same Chrome build at another path (%q); the pinned one is %q", cfg.HyperframesBrowserPath, res.BrowserPath))
			}
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
	if res.PreviousTree != "" {
		fmt.Printf("NOTE  the replaced tree is kept at %s; to roll back with an older release, rename it to node_modules and put package.json.prev and package-lock.json.prev back\n", res.PreviousTree)
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
