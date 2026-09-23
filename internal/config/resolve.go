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
)

// Source records where a Config actually came from — the loaded-vs-resolved distinction.
// A resolved path is NOT proof anything was read: disclosure must key on Loaded().
type Source struct {
	Path     string // the resolved candidate ("" = nothing resolved, built-in defaults)
	NotFound bool   // Path was explicit (--config/env) but no file exists there
	LoadErr  error  // the file existed but failed: a *ParseError (nothing from it in effect, built-in defaults) or a validation refusal (its other settings in effect)
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
	src := Source{Path: path}
	if path != "" && !exists(path) {
		// Load() maps IsNotExist to (defaults, nil) by design (fresh installs); for an
		// EXPLICIT path that silence is the trap — record it so callers can warn.
		src.NotFound = true
	}
	cfg, err := Load(path)
	src.LoadErr = err
	return cfg, src
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
		// defaults (OptiPlex, 2026-09-23: a BOM-prefixed config, a run-graph on defaults).
		fmt.Fprintf(w, "WARNING: config at %s could NOT BE PARSED: %v\n"+
			"  NOTHING from this file is in effect — this process runs on BUILT-IN DEFAULTS; machine bindings (vision, media, cascade tiers) are inactive and those calls will defer.\n"+
			"  fleet-serve refuses to start on it; the MCP server starts but defers every tool except offload_status; one-shot CLI verbs proceed on the defaults. Fix the JSON.\n",
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
			"  fleet-serve refuses to start on it; the MCP server starts but defers every tool except offload_status; one-shot CLI verbs proceed. Fix the key named above.\n",
			src.Path, src.LoadErr)
		return true
	}
	return false
}

// SourceLine renders a one-line, truthful config-source disclosure (doctor's first line).
// It must never credit a file that was not actually read.
func SourceLine(src Source) string {
	switch {
	case src.Loaded():
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
