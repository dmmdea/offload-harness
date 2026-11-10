package config

// Config-source resolution shared by every binary (local-offload AND local-agent), so no
// entry point can silently run on built-in defaults. Extracted from the root CLI after a
// review found two silent-fallback holes: an explicit --config/$LOCAL_OFFLOAD_CONFIG
// pointing at a NONEXISTENT file loaded defaults with a nil error and zero warning (Load
// deliberately treats IsNotExist as "fresh install"), and cmd/local-agent bypassed the
// discovery + warning entirely.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Source records where a Config actually came from — the loaded-vs-resolved distinction.
// A resolved path is NOT proof anything was read: disclosure must key on Loaded().
type Source struct {
	Path     string // the resolved candidate ("" = nothing resolved, built-in defaults)
	NotFound bool   // Path was explicit (--config/env) but no file exists there
	LoadErr  error  // the file existed but failed: a *ParseError (nothing from it in effect, built-in defaults) or a validation refusal (its other settings in effect)

	// ShadowedHome is the ~/.local-offload/config.json that was NOT read because Path came
	// from ./config.json in the current directory (no flag, no env) and the two are
	// different files. "" = nothing is shadowed.
	ShadowedHome string
}

// Loaded reports whether cfg values actually came from Path.
func (s Source) Loaded() bool { return s.Path != "" && !s.NotFound && s.LoadErr == nil }

// ResolvePath picks the config file by precedence: explicit flag > $LOCAL_OFFLOAD_CONFIG >
// ./config.json if it exists > ~/.local-offload/config.json if it exists > "" (defaults).
// exists is injected for testability. Flag/env paths are returned WITHOUT an existence
// check on purpose — an explicit path that is missing must surface as NotFound (loud),
// never fall through to a different file the operator did not name.
func ResolvePath(flagPath, envPath, home string, exists func(string) bool) string {
	if flagPath != "" {
		return flagPath
	}
	if envPath != "" {
		return envPath
	}
	if exists("config.json") { // cwd-relative, the README quickstart convention
		return "config.json"
	}
	if home != "" {
		if def := filepath.Join(home, ".local-offload", "config.json"); exists(def) {
			return def
		}
	}
	return ""
}

// LoadWithSource resolves (flag > env > cwd > home), loads, and reports the true source.
func LoadWithSource(flagPath string) (Config, Source) {
	home, _ := os.UserHomeDir()
	exists := func(p string) bool { info, err := os.Stat(p); return err == nil && !info.IsDir() }
	path := ResolvePath(flagPath, os.Getenv("LOCAL_OFFLOAD_CONFIG"), home, exists)
	src := Source{Path: path, ShadowedHome: shadowedHome(flagPath, os.Getenv("LOCAL_OFFLOAD_CONFIG"), path, home)}
	if path != "" && !exists(path) {
		// Load() maps IsNotExist to (defaults, nil) by design (fresh installs); for an
		// EXPLICIT path that silence is the trap — record it so callers can warn.
		src.NotFound = true
	}
	cfg, err := Load(path)
	src.LoadErr = err
	return cfg, src
}

// shadowedHome returns the home config that a cwd-resolved path hides, or "". A checkout
// of this repo keeps an untracked dev config.json at its root, so a command run there read
// it instead of the machine's real config and reported the wrong bindings with no hint
// which file it read. Same file (a symlink, or the cwd IS the home directory) is not a
// shadow: os.SameFile compares the files, not the spellings.
func shadowedHome(flagPath, envPath, path, home string) string {
	if flagPath != "" || envPath != "" || path != "config.json" || home == "" {
		return ""
	}
	def := filepath.Join(home, ".local-offload", "config.json")
	homeInfo, err := os.Stat(def)
	if err != nil || homeInfo.IsDir() {
		return ""
	}
	cwdInfo, err := os.Stat(path)
	if err != nil || os.SameFile(cwdInfo, homeInfo) {
		return ""
	}
	return def
}

var (
	shadowHomeMu   sync.Mutex
	shadowHomeSeen bool
)

// WarnOnShadowedHomeOnce prints one stderr line, once per process, when src.ShadowedHome is
// set, naming both files and the ways out. Callers pass stderr only: the MCP server's
// stdout is the JSON-RPC stream. Returns whether it printed.
func WarnOnShadowedHomeOnce(src Source, w io.Writer) bool {
	if src.ShadowedHome == "" {
		return false
	}
	shadowHomeMu.Lock()
	seen := shadowHomeSeen
	shadowHomeSeen = true
	shadowHomeMu.Unlock()
	if seen {
		return false
	}
	cwdPath, err := filepath.Abs(src.Path)
	if err != nil {
		cwdPath = src.Path
	}
	fmt.Fprintf(w, "NOTE: config read from ./config.json in the current directory (%s); %s also exists and was NOT read. Pass --config <path>, set LOCAL_OFFLOAD_CONFIG, or run from another directory to use it.\n", cwdPath, src.ShadowedHome)
	return true
}

// WarnOnDefaults prints one stderr-style warning when cfg is effectively built-in
// defaults, naming why and the escape hatches. Returns whether it warned.
func WarnOnDefaults(src Source, w io.Writer) bool {
	switch {
	case src.Path == "":
		fmt.Fprintln(w, "WARNING: running on BUILT-IN DEFAULTS — no config file found (no --config, no $LOCAL_OFFLOAD_CONFIG, no ./config.json, no ~/.local-offload/config.json). Machine bindings (vision, media, cascade tiers) are inactive and those calls will defer. Create ~/.local-offload/config.json or pass --config.")
		return true
	case src.NotFound:
		fmt.Fprintf(w, "WARNING: config file NOT FOUND at %s (from --config/$LOCAL_OFFLOAD_CONFIG) — running on BUILT-IN DEFAULTS; machine bindings are inactive. Fix the path.\n", src.Path)
		return true
	case IsParseError(src.LoadErr):
		// The file could not be DECODED, so Load returned the built-in defaults: the
		// opposite of the validation case below. This branch used to fall into that one
		// and print "the file's other settings ARE in effect" while the process ran on
		// defaults (<node-e>, 2026-09-23: a BOM-prefixed config, a run-graph on defaults).
		fmt.Fprintf(w, "WARNING: config at %s could NOT BE PARSED: %v\n"+
			"  NOTHING from this file is in effect — this process runs on BUILT-IN DEFAULTS; machine bindings (vision, media, cascade tiers) are inactive and those calls will defer.\n"+
			"  fleet-serve refuses to start on it; the MCP server starts but defers every tool except offload_status; the ComfyUI media verbs (generate-image, run-graph, generate-video and the like) refuse; the other one-shot CLI verbs proceed on the defaults. Fix the JSON.\n",
			src.Path, src.LoadErr)
		return true
	case src.LoadErr != nil:
		// NOT "built-in defaults": Load returns the FILE's settings with only the
		// five composite keys stripped, so this process runs on that file — which
		// is exactly why a refusal matters. Saying "defaults" sent an operator
		// hunting a path problem while the real one was a named key in the file
		// they already had open, and it hid the only text that names it.
		fmt.Fprintf(w, "WARNING: config at %s loaded but FAILED VALIDATION: %v\n"+
			"  The file's other settings ARE in effect — this is not a fall back to built-in defaults.\n"+
			"  fleet-serve refuses to start on it; the MCP server starts but defers every tool except offload_status; the ComfyUI media verbs (generate-image, run-graph, generate-video and the like) refuse; the other one-shot CLI verbs proceed. Fix the key named above.\n",
			src.Path, src.LoadErr)
		return true
	}
	return false
}

// WarnOnShadowedAgentModel prints one line when cfg's agent_model is shadowed by
// the layers it also declares (AgentModelShadowNote), naming the file that
// supplied both so a config copy is the first suspect rather than the node's own
// configuration. Returns whether it warned. Register C-95.
func WarnOnShadowedAgentModel(src Source, cfg Config, w io.Writer) bool {
	note := cfg.AgentModelShadowNote()
	if note == "" {
		return false
	}
	from := "built-in defaults"
	if src.Path != "" {
		from = src.Path
	}
	fmt.Fprintf(w, "note: config %s: %s\n", from, note)
	return true
}

var (
	shadowNoteMu   sync.Mutex
	shadowNoteSeen = map[[2]string]bool{}
)

// WarnOnShadowedAgentModelOnce is WarnOnShadowedAgentModel for the shared
// config-loading path: every entry point that loads a config (the MCP server,
// agent_delegate and agent_run through it, acceptance, report, delegate, the
// local-agent binary) calls it, and a process says each distinct (file, note)
// once however many times it loads the config. Callers pass stderr only: the
// MCP server's stdout is the JSON-RPC stream. Register C-95.
func WarnOnShadowedAgentModelOnce(src Source, cfg Config, w io.Writer) bool {
	note := cfg.AgentModelShadowNote()
	if note == "" {
		return false
	}
	key := [2]string{src.Path, note}
	shadowNoteMu.Lock()
	seen := shadowNoteSeen[key]
	shadowNoteSeen[key] = true
	shadowNoteMu.Unlock()
	if seen {
		return false
	}
	return WarnOnShadowedAgentModel(src, cfg, w)
}

// SourceLine renders a one-line, truthful config-source disclosure (doctor's first line).
// It must never credit a file that was not actually read.
func SourceLine(src Source) string {
	switch {
	case src.Loaded():
		if src.ShadowedHome != "" {
			return "config:     " + src.Path + " (./config.json in the current directory; " + src.ShadowedHome + " also exists and was NOT read)"
		}
		return "config:     " + src.Path
	case src.Path == "":
		return "config:     BUILT-IN DEFAULTS (no config file found)"
	case src.NotFound:
		return "config:     BUILT-IN DEFAULTS (file not found: " + src.Path + ")"
	case IsParseError(src.LoadErr):
		// Nothing from the file is in effect: say defaults, and name the file and why.
		return "config:     BUILT-IN DEFAULTS (" + src.Path + " could not be parsed: " + src.LoadErr.Error() + ")"
	case src.LoadErr != nil:
		// Same correction as WarnOnDefaults above, on the line doctor prints
		// FIRST: this config came from the file, it just did not pass validation,
		// and the error text is the only thing that names the offending key.
		return "config:     " + src.Path + " — FAILED VALIDATION: " + src.LoadErr.Error()
	default:
		return "config:     BUILT-IN DEFAULTS (" + src.Path + " was not read)"
	}
}

// LoadFailure names how a config load failed, for messages that wrap LoadErr: a file
// that could not be parsed (nothing from it in effect) or one that failed validation
// (its other settings in effect).
func LoadFailure(err error) string {
	if IsParseError(err) {
		return "could not be parsed"
	}
	return "failed validation"
}
