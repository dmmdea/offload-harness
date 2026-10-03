// Package hfinstall is the HyperFrames step of the composition lane (ADR 0059) as one
// implementation: the two version reads that status and the runner must agree on, and
// the re-install a deploy runs when the pin moves.
//
// The runner (render/compose-hyperframes.mjs) refuses every op while the install under
// hyperframes_dir holds anything but its PINNED_VERSION. Until this package existed the
// re-install lived only in the installers' first-install path, so a release that moved
// the pin left every already-installed node refusing every composition while doctor and
// offload_status still read the route CONFIGURED (they checked file existence only).
package hfinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// pinRe matches the runner's one pin line: `export const PINNED_VERSION = "0.8.114";`.
var pinRe = regexp.MustCompile(`(?m)^export const PINNED_VERSION = "([^"]+)";`)

// RunnerPin reads the HyperFrames version the runner at path enforces.
func RunnerPin(runner string) (string, error) {
	b, err := os.ReadFile(runner)
	if err != nil {
		return "", err
	}
	m := pinRe.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("no PINNED_VERSION line in %s", runner)
	}
	return string(m[1]), nil
}

// InstalledVersion reads the version of the HyperFrames package installed under dir —
// the same file the runner reads (node_modules/hyperframes/package.json).
func InstalledVersion(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "node_modules", "hyperframes", "package.json"))
	if err != nil {
		return "", err
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", fmt.Errorf("unreadable hyperframes package.json under %s: %w", dir, err)
	}
	if p.Version == "" {
		return "", fmt.Errorf("hyperframes package.json under %s carries no version", dir)
	}
	return p.Version, nil
}

// PackagePin reads the exact version a committed setup/hyperframes/package.json pins.
func PackagePin(packageJSON []byte) (string, error) {
	var p struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(packageJSON, &p); err != nil {
		return "", fmt.Errorf("unreadable setup package.json: %w", err)
	}
	v := p.Dependencies["hyperframes"]
	if v == "" {
		return "", errors.New("setup package.json pins no hyperframes dependency")
	}
	return v, nil
}

// Drift reports nil when the install under dir holds exactly the runner's pin, and
// otherwise the reason every composition on this box defers CLI_MISSING. An unreadable
// runner pin is not drift (nothing to compare against); an unreadable install is.
func Drift(runner, dir string) error {
	pin, err := RunnerPin(runner)
	if err != nil {
		return nil
	}
	have, err := InstalledVersion(dir)
	if err != nil {
		return fmt.Errorf("cannot read the installed HyperFrames version under %s (%v), and the runner pins %s — every composition defers CLI_MISSING; run `local-offload install hyperframes`", dir, err, pin)
	}
	if have != pin {
		return fmt.Errorf("the install at %s holds hyperframes %s, not the runner's pin %s — every composition defers CLI_MISSING; run `local-offload install hyperframes`", dir, have, pin)
	}
	return nil
}

// Runner executes one command and returns its stdout. Injectable so the step order and
// the failure handling are unit-tested without npm, node or a network.
type Runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// Options for Install. PackageJSON and PackageLock are the committed setup/hyperframes
// files, embedded in the binary so a node without a repository checkout can re-install.
type Options struct {
	Dir         string // hyperframes_dir: where node_modules/hyperframes lives
	Runner      string // the resolved compose_script
	Node        string // the node executable every render script runs under
	Npm         string // npm; empty = found on PATH
	PackageJSON []byte
	PackageLock []byte
	Log         io.Writer
	Run         Runner // nil = exec
}

// Result is what Install verified, all of it read back rather than assumed.
type Result struct {
	Dir           string `json:"dir"`
	Version       string `json:"version"`
	Reinstalled   bool   `json:"reinstalled"`
	BrowserPath   string `json:"browser_path"`
	ChromeVersion string `json:"chrome_version"`
	MatchesPin    bool   `json:"matches_pin"`
}

// Install brings the install under o.Dir to the embedded pin, the way the installers'
// first-install step does: write the committed package.json + lock, `npm ci
// --ignore-scripts` when the installed version differs (or the CLI entry is missing),
// `npm audit signatures` (FATAL: an unverifiable install is never bound), `npm rebuild
// esbuild` (the one postinstall the CLI needs, run on purpose), then the runner's own
// `browser` op (ensure + path, under its scrubbed env) and `version` op, which must
// report matches_pin. It refuses up front when the runner beside it pins another
// version: the binary and the render tree of one release ship together, and installing
// a pin the runner rejects would turn a working lane into a broken one.
func Install(ctx context.Context, o Options) (Result, error) {
	res := Result{Dir: o.Dir}
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	run := o.Run
	if run == nil {
		run = execRun
	}
	if o.Dir == "" {
		return res, errors.New("no hyperframes_dir: bind it in the config or pass --dir")
	}
	pin, err := PackagePin(o.PackageJSON)
	if err != nil {
		return res, err
	}
	res.Version = pin
	runnerPin, err := RunnerPin(o.Runner)
	if err != nil {
		return res, fmt.Errorf("cannot read the runner's pin: %w", err)
	}
	if runnerPin != pin {
		return res, fmt.Errorf("the runner at %s pins hyperframes %s but this binary carries %s: ship the binary and the render tree of one release together, then re-run", o.Runner, runnerPin, pin)
	}
	npm := o.Npm
	if npm == "" {
		npm = "npm"
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return res, err
	}
	for name, want := range map[string][]byte{"package.json": o.PackageJSON, "package-lock.json": o.PackageLock} {
		p := filepath.Join(o.Dir, name)
		if have, err := os.ReadFile(p); err == nil && bytes.Equal(have, want) {
			continue
		}
		if err := os.WriteFile(p, want, 0o644); err != nil {
			return res, fmt.Errorf("writing %s: %w", p, err)
		}
	}
	have, _ := InstalledVersion(o.Dir)
	_, entryErr := os.Stat(filepath.Join(o.Dir, "node_modules", "hyperframes", "bin", "hyperframes.mjs"))
	if have != pin || entryErr != nil {
		logf("npm ci: hyperframes %s -> %s in %s", orNone(have), pin, o.Dir)
		if out, err := run(ctx, o.Dir, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
			return res, fmt.Errorf("npm ci (hyperframes %s) failed: %v%s", pin, err, tail(out))
		}
		res.Reinstalled = true
	} else {
		logf("hyperframes %s already installed in %s", pin, o.Dir)
	}
	if out, err := run(ctx, o.Dir, npm, "audit", "signatures"); err != nil {
		return res, fmt.Errorf("npm audit signatures FAILED for the hyperframes install: registry signature or provenance check did not pass; refusing to bind the compose lane: %v%s", err, tail(out))
	}
	if out, err := run(ctx, o.Dir, npm, "rebuild", "esbuild"); err != nil {
		return res, fmt.Errorf("npm rebuild esbuild failed: %v%s", err, tail(out))
	}
	var browser struct {
		OK            bool   `json:"ok"`
		Class         string `json:"class"`
		Detail        string `json:"detail"`
		BrowserPath   string `json:"browser_path"`
		ChromeVersion string `json:"chrome_version"`
	}
	if err := runnerOp(ctx, run, o, "browser", &browser); err != nil {
		return res, err
	}
	if !browser.OK {
		return res, fmt.Errorf("hyperframes browser ensure failed: %s: %s", browser.Class, browser.Detail)
	}
	res.BrowserPath, res.ChromeVersion = browser.BrowserPath, browser.ChromeVersion
	var version struct {
		OK         bool   `json:"ok"`
		Class      string `json:"class"`
		Detail     string `json:"detail"`
		MatchesPin bool   `json:"matches_pin"`
	}
	if err := runnerOp(ctx, run, o, "version", &version); err != nil {
		return res, err
	}
	if !version.OK || !version.MatchesPin {
		return res, fmt.Errorf("the runner's version check did not pass after the install: %s %s", version.Class, version.Detail)
	}
	res.MatchesPin = true
	return res, nil
}

// runnerOp runs one runner op and decodes its result: the LAST stdout line that is a
// JSON object, the runner's contract (every op ends with one).
func runnerOp(ctx context.Context, run Runner, o Options, op string, into any) error {
	out, err := run(ctx, o.Dir, o.Node, o.Runner, op, "--hyperframes-dir", o.Dir)
	line := lastJSONLine(out)
	if line == "" {
		if err != nil {
			return fmt.Errorf("hyperframes %s op failed: %v%s", op, err, tail(out))
		}
		return fmt.Errorf("hyperframes %s op printed no result%s", op, tail(out))
	}
	if jerr := json.Unmarshal([]byte(line), into); jerr != nil {
		return fmt.Errorf("hyperframes %s op printed an unreadable result: %v", op, jerr)
	}
	return nil
}

func lastJSONLine(out string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}

func execRun(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil && stderr.Len() > 0 {
		return stdout.String() + "\n" + stderr.String(), err
	}
	return stdout.String(), err
}

func tail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > 600 {
		out = "…" + out[len(out)-600:]
	}
	return ": " + out
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}
