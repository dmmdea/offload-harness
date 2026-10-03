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
	"runtime"
	"strings"
	"time"
)

// SetupPackageName is the name the committed setup/hyperframes/package.json carries. A
// directory whose package.json names anything else is not a harness install, and
// Install refuses to write into it.
const SetupPackageName = "offload-harness-hyperframes"

const (
	stagingDirName = ".install-staging"  // the new tree is built and verified here first
	prevDirName    = "node_modules.prev" // the replaced tree, kept for a rollback
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

// LockPin reads the hyperframes version a committed package-lock.json resolves.
func LockPin(packageLock []byte) (string, error) {
	var l struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(packageLock, &l); err != nil {
		return "", fmt.Errorf("unreadable setup package-lock.json: %w", err)
	}
	v := l.Packages["node_modules/hyperframes"].Version
	if v == "" {
		return "", errors.New("setup package-lock.json resolves no node_modules/hyperframes")
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

// Runner executes one command in dir and returns its STDOUT; a failure's error carries
// the tail of its stderr. Injectable so the step order and every failure path are
// unit-tested without npm, node or a network.
type Runner func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)

// Options for Install. PackageJSON and PackageLock are the committed setup/hyperframes
// files, embedded in the binary so a node without a repository checkout can re-install.
type Options struct {
	Dir         string // hyperframes_dir: where node_modules/hyperframes lives
	Runner      string // the resolved compose_script
	Node        string // the node executable every render script runs under
	Npm         string // npm; empty = found on PATH (with Node's directory first)
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
	PreviousTree  string `json:"previous_tree,omitempty"`
	BrowserPath   string `json:"browser_path"`
	ChromeVersion string `json:"chrome_version"`
	MatchesPin    bool   `json:"matches_pin"`
}

// Install brings the install under o.Dir to the embedded pin without ever leaving the
// node worse off than it found it:
//
//   - It refuses before touching anything when the runner beside the binary pins another
//     version than the binary carries (the binary and the render tree of one release ship
//     together) or when o.Dir holds something that is not a harness install.
//   - A healthy install — the committed package.json and lock, the pinned version, the CLI
//     entry and a dependency tree `npm ls` accepts — is kept; its signatures are audited
//     again and the runner's checks run.
//   - Anything else is rebuilt in a staging directory inside o.Dir: `npm ci
//     --ignore-scripts` and `npm audit signatures` (FATAL) run there, so a failure leaves
//     the live tree exactly as it was. Only a verified tree is swapped in; the replaced one
//     is kept as node_modules.prev for a rollback.
//   - `npm rebuild esbuild` (the one postinstall the CLI needs) and the runner's own
//     `browser` (ensure + path, under its scrubbed env) and `version` ops (must report
//     matches_pin) run on the swapped-in tree; when one fails, the previous tree and its
//     package files are put back.
func Install(ctx context.Context, o Options) (Result, error) {
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
		return Result{}, errors.New("no hyperframes_dir: bind it in the config or pass --dir")
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return Result{}, err
	}
	res := Result{Dir: dir}
	pin, err := PackagePin(o.PackageJSON)
	if err != nil {
		return res, err
	}
	if lp, err := LockPin(o.PackageLock); err != nil || lp != pin {
		return res, fmt.Errorf("the embedded lock resolves hyperframes %q, not the embedded package.json's %s (%v)", lp, pin, err)
	}
	res.Version = pin
	runnerPin, err := RunnerPin(o.Runner)
	if err != nil {
		return res, fmt.Errorf("cannot read the runner's pin: %w", err)
	}
	if runnerPin != pin {
		return res, fmt.Errorf("the runner at %s pins hyperframes %s but this binary carries %s: ship the binary and the render tree of one release together, then re-run", o.Runner, runnerPin, pin)
	}
	if err := ownsDir(dir); err != nil {
		return res, err
	}
	npm := o.Npm
	if npm == "" {
		npm = "npm"
	}
	env := envWithNodeFirst(o.Node)

	swapped := false
	if healthy(ctx, run, env, npm, dir, pin, o.PackageJSON, o.PackageLock) {
		logf("hyperframes %s already installed in %s; re-verifying", pin, dir)
		if out, err := run(ctx, dir, env, npm, "audit", "signatures"); err != nil {
			return res, fmt.Errorf("npm audit signatures FAILED on the installed tree (left unchanged): %v%s", err, tail(out))
		}
	} else {
		have, _ := InstalledVersion(dir)
		logf("npm ci: hyperframes %s -> %s, staged in %s", orNone(have), pin, filepath.Join(dir, stagingDirName))
		if err := stage(ctx, run, env, npm, dir, pin, o.PackageJSON, o.PackageLock); err != nil {
			return res, err
		}
		prev, err := swapIn(dir, o.PackageJSON, o.PackageLock)
		if err != nil {
			return res, err
		}
		swapped, res.Reinstalled, res.PreviousTree = true, true, prev
	}

	fail := func(err error) (Result, error) {
		if swapped {
			if rerr := rollback(dir); rerr != nil {
				return res, fmt.Errorf("%v; restoring the previous install also failed: %v", err, rerr)
			}
			res.Reinstalled, res.PreviousTree = false, ""
			return res, fmt.Errorf("%v; the previous install was put back", err)
		}
		return res, err
	}
	if out, err := run(ctx, dir, env, npm, "rebuild", "esbuild"); err != nil {
		return fail(fmt.Errorf("npm rebuild esbuild failed: %v%s", err, tail(out)))
	}
	var browser struct {
		OK            bool   `json:"ok"`
		Class         string `json:"class"`
		Detail        string `json:"detail"`
		BrowserPath   string `json:"browser_path"`
		ChromeVersion string `json:"chrome_version"`
	}
	if err := runnerOp(ctx, run, env, o.Node, o.Runner, dir, "browser", &browser); err != nil {
		return fail(err)
	}
	if !browser.OK {
		return fail(fmt.Errorf("hyperframes browser ensure failed: %s: %s", browser.Class, browser.Detail))
	}
	res.BrowserPath, res.ChromeVersion = browser.BrowserPath, browser.ChromeVersion
	var version struct {
		OK         bool   `json:"ok"`
		Class      string `json:"class"`
		Detail     string `json:"detail"`
		MatchesPin bool   `json:"matches_pin"`
	}
	if err := runnerOp(ctx, run, env, o.Node, o.Runner, dir, "version", &version); err != nil {
		return fail(err)
	}
	if !version.OK || !version.MatchesPin {
		return fail(fmt.Errorf("the runner's version check did not pass after the install: %s %s", version.Class, version.Detail))
	}
	res.MatchesPin = true
	return res, nil
}

// ownsDir refuses a directory that holds something other than a harness install: it
// must be absent, empty, or carry the setup package.json's name.
func ownsDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return fmt.Errorf("%s is not empty and holds no package.json: not a harness HyperFrames install, refusing to write into it", dir)
	}
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &p) != nil || p.Name != SetupPackageName {
		return fmt.Errorf("%s holds package %q, not %q: not a harness HyperFrames install, refusing to write into it", dir, p.Name, SetupPackageName)
	}
	return nil
}

// healthy reports whether the install under dir can be kept as it is: the committed
// package files, the pinned version, the CLI entry, and a dependency tree npm accepts
// (`npm ls` fails on a missing or mismatched package, e.g. after an interrupted npm ci).
func healthy(ctx context.Context, run Runner, env []string, npm, dir, pin string, pkg, lock []byte) bool {
	for name, want := range map[string][]byte{"package.json": pkg, "package-lock.json": lock} {
		if have, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(have, want) {
			return false
		}
	}
	if have, err := InstalledVersion(dir); err != nil || have != pin {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "hyperframes", "bin", "hyperframes.mjs")); err != nil {
		return false
	}
	_, err := run(ctx, dir, env, npm, "ls", "--all")
	return err == nil
}

// stage builds and verifies the new tree under dir/.install-staging. On any failure the
// staging directory is removed and the live install has not been touched.
func stage(ctx context.Context, run Runner, env []string, npm, dir, pin string, pkg, lock []byte) error {
	st := filepath.Join(dir, stagingDirName)
	if err := os.RemoveAll(st); err != nil {
		return fmt.Errorf("clearing a stale staging dir %s: %w", st, err)
	}
	if err := os.MkdirAll(st, 0o755); err != nil {
		return err
	}
	clean := func(err error) error {
		_ = os.RemoveAll(st)
		return err
	}
	for name, b := range map[string][]byte{"package.json": pkg, "package-lock.json": lock} {
		if err := os.WriteFile(filepath.Join(st, name), b, 0o644); err != nil {
			return clean(fmt.Errorf("writing %s: %w", name, err))
		}
	}
	if out, err := run(ctx, st, env, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
		return clean(fmt.Errorf("npm ci (hyperframes %s) failed; the live install is unchanged: %v%s", pin, err, tail(out)))
	}
	if out, err := run(ctx, st, env, npm, "audit", "signatures"); err != nil {
		return clean(fmt.Errorf("npm audit signatures FAILED for the new hyperframes %s tree (registry signatures or provenance did not verify, or the registry was unreachable); refusing to swap it in, the live install is unchanged: %v%s", pin, err, tail(out)))
	}
	if v, err := InstalledVersion(st); err != nil || v != pin {
		return clean(fmt.Errorf("npm ci produced hyperframes %q, not %s", v, pin))
	}
	return nil
}

// swapIn moves the verified staging tree into place: the live node_modules (if any) and
// its package files become the rollback copy, then the new tree and package files take
// their place. A rename that fails (a render holding files open on Windows) leaves the
// live install as it was. Returns the rollback tree's path ("" when there was none).
func swapIn(dir string, pkg, lock []byte) (string, error) {
	st := filepath.Join(dir, stagingDirName)
	live := filepath.Join(dir, "node_modules")
	prev := filepath.Join(dir, prevDirName)
	defer os.RemoveAll(st)
	if err := os.RemoveAll(prev); err != nil {
		return "", fmt.Errorf("clearing the old rollback copy %s: %w", prev, err)
	}
	oldFiles := map[string][]byte{}
	for _, name := range []string{"package.json", "package-lock.json"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			oldFiles[name] = b
		}
	}
	kept := ""
	if _, err := os.Stat(live); err == nil {
		if err := renameRetry(live, prev); err != nil {
			return "", fmt.Errorf("cannot move the live install aside (is a composition rendering?); nothing changed: %w", err)
		}
		kept = prev
		for name, b := range oldFiles {
			_ = os.WriteFile(filepath.Join(dir, name+".prev"), b, 0o644)
		}
	}
	restore := func(cause error) error {
		if kept != "" {
			_ = os.RemoveAll(live)
			if err := renameRetry(prev, live); err != nil {
				return fmt.Errorf("%v; restoring the previous tree also failed: %v", cause, err)
			}
		}
		for name, b := range oldFiles {
			_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
		}
		return fmt.Errorf("%v; the previous install is back", cause)
	}
	if err := renameRetry(filepath.Join(st, "node_modules"), live); err != nil {
		return "", restore(fmt.Errorf("moving the new tree into place failed: %w", err))
	}
	for name, b := range map[string][]byte{"package.json": pkg, "package-lock.json": lock} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return "", restore(fmt.Errorf("writing %s: %w", name, err))
		}
	}
	return kept, nil
}

// renameRetry retries a rename that fails, a few times over about three seconds: on
// Windows an antivirus scan or an indexer holds a just-written file for a moment.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 4; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 250 * time.Millisecond)
	}
	return err
}

// rollback puts the previous tree and package files back after a swapped-in tree failed
// its checks.
func rollback(dir string) error {
	live := filepath.Join(dir, "node_modules")
	prev := filepath.Join(dir, prevDirName)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous tree at %s to restore", prev)
	}
	if err := os.RemoveAll(live); err != nil {
		return err
	}
	if err := os.Rename(prev, live); err != nil {
		return err
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		if b, err := os.ReadFile(filepath.Join(dir, name+".prev")); err == nil {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// runnerOp runs one runner op and decodes its result: the LAST stdout line that is a
// JSON object, the runner's contract (every op ends with one, failures included).
func runnerOp(ctx context.Context, run Runner, env []string, node, runner, dir, op string, into any) error {
	out, err := run(ctx, dir, env, node, runner, op, "--hyperframes-dir", dir)
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

// envWithNodeFirst is the process env with the configured node's directory first on
// PATH, so npm — and every script npm starts, which finds node through PATH or a
// `#!/usr/bin/env node` shebang — runs under the node the config names. A bare "node"
// changes nothing.
func envWithNodeFirst(node string) []string {
	env := os.Environ()
	if !filepath.IsAbs(node) {
		return env
	}
	dir := filepath.Dir(node)
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, "PATH") && (runtime.GOOS == "windows" || k == "PATH") {
			env[i] = k + "=" + dir + string(os.PathListSeparator) + v
			return env
		}
	}
	return append(env, "PATH="+dir)
}

func execRun(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return stdout.String(), fmt.Errorf("%w%s", err, tail(s))
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
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
