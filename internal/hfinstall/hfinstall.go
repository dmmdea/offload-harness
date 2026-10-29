// Package hfinstall is the HyperFrames step of the composition lane (ADR 0059) as one
// implementation: the version read that status and the runner agree on, and the install a
// deploy runs.
//
// The installed HyperFrames is a record with a floor, never an exact version (ADR 0099).
// The harness installs the newest release npm resolves, stages and verifies it, swaps it in
// with the previous tree kept for a rollback, and records {version, installed_at} in
// installed.json. The runner (render/compose-hyperframes.mjs) and capability status refuse
// only a CLI that is absent or older than the floor in setup/floors.json.
package hfinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// SetupPackageName is the name the committed setup/hyperframes/package.json carries. A
// directory whose package.json names anything else is not a harness install, and
// Install refuses to write into it.
const SetupPackageName = "offload-harness-hyperframes"

const (
	stagingDirName = ".install-staging"  // the new tree is built and verified here first
	prevDirName    = "node_modules.prev" // the replaced tree, kept for a rollback
)

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

// FloorFromJSON reads the HyperFrames floor ("hyperframes".harness) from the bytes of
// setup/floors.json. "" with a nil error means the file names no floor.
func FloorFromJSON(floorsJSON []byte) (string, error) {
	var f struct {
		Hyperframes struct {
			Harness string `json:"harness"`
		} `json:"hyperframes"`
	}
	if err := json.Unmarshal(floorsJSON, &f); err != nil {
		return "", fmt.Errorf("unreadable floors.json: %w", err)
	}
	return f.Hyperframes.Harness, nil
}

// FloorFor reads the floor from setup/floors.json beside the runner's repo root
// (<runner dir>/../setup/floors.json), the file the runner itself reads. "" when the tree
// does not carry it: no floor is then enforced, presence only.
func FloorFor(runner string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(runner), "..", "setup", "floors.json"))
	if err != nil {
		return ""
	}
	v, _ := FloorFromJSON(b)
	return v
}

// versionCore parses the leading numeric dotted part of a version ("9.9.9-rc.1" -> 9,9,9).
func versionCore(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	end := 0
	for end < len(v) && (v[end] == '.' || (v[end] >= '0' && v[end] <= '9')) {
		end++
	}
	if end == 0 {
		return nil, false
	}
	var out []int
	for _, part := range strings.Split(strings.TrimRight(v[:end], "."), ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// AtLeast reports whether have is the floor or newer, comparing the numeric parts (a
// pre-release tag on an equal or newer core does not lower it). An empty floor is always
// met; an unreadable have is below any floor.
func AtLeast(have, floor string) bool {
	f, ok := versionCore(floor)
	if !ok {
		return true
	}
	h, ok := versionCore(have)
	if !ok {
		return false
	}
	for i := 0; i < len(h) || i < len(f); i++ {
		var a, b int
		if i < len(h) {
			a = h[i]
		}
		if i < len(f) {
			b = f[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// Check reports nil when the install under dir has the CLI and a version at or above the
// floor beside the runner, and otherwise the reason every composition on this box defers
// CLI_MISSING. A newer release than any this code has seen is not a problem.
func Check(runner, dir string) error {
	have, err := InstalledVersion(dir)
	if err != nil {
		return fmt.Errorf("cannot read the installed HyperFrames version under %s (%v) — every composition defers CLI_MISSING; run `local-offload install hyperframes`", dir, err)
	}
	if floor := FloorFor(runner); !AtLeast(have, floor) {
		return fmt.Errorf("the install at %s holds hyperframes %s, below the floor %s in setup/floors.json — every composition defers CLI_MISSING; run `local-offload install hyperframes`", dir, have, floor)
	}
	return nil
}

// Runner executes one command in dir and returns its STDOUT; a failure's error carries
// the tail of its stderr. Injectable so the step order and every failure path are
// unit-tested without npm, node or a network.
type Runner func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)

// Options for Install. PackageJSON is the committed setup/hyperframes/package.json (its
// hyperframes dependency is "latest"), embedded in the binary so a node without a
// repository checkout can install. Floor is the minimum version from setup/floors.json.
type Options struct {
	Dir          string // hyperframes_dir: where node_modules/hyperframes lives
	Runner       string // the resolved compose_script
	Node         string // the node executable every render script runs under
	Npm          string // npm; empty = found on PATH (with Node's directory first)
	PackageJSON  []byte
	Floor        string // minimum acceptable version ("" = none known)
	ManifestPath string // installed.json to record {version, installed_at} in ("" = do not record)
	Log          io.Writer
	Run          Runner           // nil = exec
	Now          func() time.Time // nil = time.Now
}

// Result is what Install verified, all of it read back rather than assumed.
type Result struct {
	Dir          string `json:"dir"`
	Version      string `json:"version"`
	Reinstalled  bool   `json:"reinstalled"`
	PreviousTree string `json:"previous_tree,omitempty"`
	BrowserPath  string `json:"browser_path"`
	Floor        string `json:"floor,omitempty"`
}

// Install brings the install under o.Dir to the newest release npm resolves without ever
// leaving the node worse off than it found it:
//
//   - It refuses before touching anything when o.Dir holds something that is not a
//     harness install.
//   - An installed tree that npm accepts (`npm ls`), that holds the CLI entry and the
//     committed package.json, and that is at or above the registry's latest, is kept (a
//     newer hand-placed build is never downgraded); its signatures are audited again. An
//     unreachable registry keeps a healthy tree at or above the floor.
//   - Anything else is rebuilt in a staging directory inside o.Dir: `npm install
//     --ignore-scripts` (resolving dist-tags.latest; npm verifies each tarball against the
//     registry's integrity field) and `npm audit signatures` (FATAL) run there, the staged
//     package's version is read back and checked against the floor, so a failure leaves
//     the live tree exactly as it was. Only a verified tree is swapped in; the replaced
//     one is kept as node_modules.prev for a rollback.
//   - `npm rebuild esbuild` (the one postinstall the CLI needs) and the runner's own
//     `browser` (ensure + path, under its scrubbed env) and `version` ops run on the
//     swapped-in tree; when one fails, the previous tree and its package.json are put back.
//   - The installed version and time are recorded in installed.json.
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
	res := Result{Dir: dir, Floor: o.Floor}
	if err := ownsDir(dir); err != nil {
		return res, err
	}
	npm := o.Npm
	if npm == "" {
		npm = "npm"
	}
	env := envWithNodeFirst(o.Node)

	have, _ := InstalledVersion(dir)
	latest := ""
	if out, err := run(ctx, dir, env, npm, "view", "hyperframes", "dist-tags.latest"); err == nil {
		latest = strings.TrimSpace(out)
	} else {
		logf("could not ask the registry for the newest hyperframes (%v); keeping a healthy install", err)
	}

	swapped := false
	keep := healthy(ctx, run, env, npm, dir, o.PackageJSON) && AtLeast(have, o.Floor) &&
		(latest == "" || AtLeast(have, latest))
	if keep {
		logf("hyperframes %s already installed in %s; re-verifying", have, dir)
		if out, err := run(ctx, dir, env, npm, "audit", "signatures"); err != nil {
			return res, fmt.Errorf("npm audit signatures FAILED on the installed tree (left unchanged): %v%s", err, tail(out))
		}
	} else {
		logf("npm install: hyperframes %s -> newest, staged in %s", orNone(have), filepath.Join(dir, stagingDirName))
		if err := stage(ctx, run, env, npm, dir, o.PackageJSON, o.Floor); err != nil {
			return res, err
		}
		prev, err := swapIn(dir, o.PackageJSON)
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
	browserPath, got, verr := Verify(ctx, run, o.Node, npm, o.Runner, dir)
	res.BrowserPath = browserPath
	if verr != nil {
		return fail(verr)
	}
	res.Version = got
	if o.ManifestPath != "" {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		if err := RecordInstalled(o.ManifestPath, got, now()); err != nil {
			return res, fmt.Errorf("hyperframes %s installed, but recording it in %s failed: %w", got, o.ManifestPath, err)
		}
	}
	return res, nil
}

// Verify checks an install whose node_modules are in place under dir: `npm rebuild esbuild`
// (the one postinstall the CLI needs), then the runner's own `browser` op (ensure + path, under
// its scrubbed env, so the browser is the one THIS release resolves) and its `version` op (ok
// and at_floor). It returns the browser path the release resolved and the installed version.
// run nil = exec; npm empty = "npm". Install runs it on a swapped-in tree; `update` runs it on
// the staged generation before that generation can go live.
func Verify(ctx context.Context, run Runner, node, npm, runner, dir string) (browserPath, version string, err error) {
	if run == nil {
		run = execRun
	}
	if npm == "" {
		npm = "npm"
	}
	env := envWithNodeFirst(node)
	if out, err := run(ctx, dir, env, npm, "rebuild", "esbuild"); err != nil {
		return "", "", fmt.Errorf("npm rebuild esbuild failed: %v%s", err, tail(out))
	}
	var browser struct {
		OK          bool   `json:"ok"`
		Class       string `json:"class"`
		Detail      string `json:"detail"`
		BrowserPath string `json:"browser_path"`
	}
	if err := runnerOp(ctx, run, env, node, runner, dir, "browser", &browser); err != nil {
		return "", "", err
	}
	if !browser.OK {
		return "", "", fmt.Errorf("hyperframes browser ensure failed: %s: %s", browser.Class, browser.Detail)
	}
	browserPath = browser.BrowserPath
	var ver struct {
		OK      bool   `json:"ok"`
		Class   string `json:"class"`
		Detail  string `json:"detail"`
		Version string `json:"version"`
		AtFloor bool   `json:"at_floor"`
	}
	if err := runnerOp(ctx, run, env, node, runner, dir, "version", &ver); err != nil {
		return browserPath, "", err
	}
	if !ver.OK || !ver.AtFloor {
		return browserPath, "", fmt.Errorf("the runner's version check did not pass after the install: %s %s", ver.Class, ver.Detail)
	}
	version, err = InstalledVersion(dir)
	if err != nil {
		return browserPath, "", err
	}
	return browserPath, version, nil
}

// RecordInstalled writes {version, installed_at} under components.hyperframes.current of the
// installed.json at path (schema 2: current carries the version as its id). Every other key is
// kept as found; a missing file is created; a flat schema-1 entry (components.hyperframes as a
// bare version string) is replaced by the record. The file is replaced by rename.
func RecordInstalled(path, version string, at time.Time) error {
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is not readable JSON: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(doc) == 0 {
		doc["schema"] = 2
	}
	comps, _ := doc["components"].(map[string]any)
	if comps == nil {
		comps = map[string]any{}
	}
	rec := map[string]any{"id": version, "version": version, "installed_at": at.UTC().Format(time.RFC3339)}
	entry, _ := comps["hyperframes"].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["current"] = rec
	comps["hyperframes"] = entry
	doc["components"] = comps
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return renameRetry(tmp, path)
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
// package.json, a readable version, the CLI entry, and a dependency tree npm accepts
// (`npm ls` fails on a missing or mismatched package, e.g. after an interrupted install).
func healthy(ctx context.Context, run Runner, env []string, npm, dir string, pkg []byte) bool {
	if have, err := os.ReadFile(filepath.Join(dir, "package.json")); err != nil || !bytes.Equal(have, pkg) {
		return false
	}
	if _, err := InstalledVersion(dir); err != nil {
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
func stage(ctx context.Context, run Runner, env []string, npm, dir string, pkg []byte, floor string) error {
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
	if err := os.WriteFile(filepath.Join(st, "package.json"), pkg, 0o644); err != nil {
		return clean(fmt.Errorf("writing package.json: %w", err))
	}
	if out, err := run(ctx, st, env, npm, "install", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
		return clean(fmt.Errorf("npm install (newest hyperframes) failed; the live install is unchanged: %v%s", err, tail(out)))
	}
	v, err := InstalledVersion(st)
	if err != nil {
		return clean(fmt.Errorf("npm install produced no readable node_modules/hyperframes/package.json; the live install is unchanged: %w", err))
	}
	if !AtLeast(v, floor) {
		return clean(fmt.Errorf("npm resolved hyperframes %s, below the floor %s in setup/floors.json; the live install is unchanged", v, floor))
	}
	if out, err := run(ctx, st, env, npm, "audit", "signatures"); err != nil {
		return clean(fmt.Errorf("npm audit signatures FAILED for the new hyperframes %s tree (registry signatures or provenance did not verify, or the registry was unreachable); refusing to swap it in, the live install is unchanged: %v%s", v, err, tail(out)))
	}
	return nil
}

// swapIn moves the verified staging tree into place: the live node_modules (if any) and
// its package.json become the rollback copy, then the new tree and package.json take
// their place. A rename that fails (a render holding files open on Windows) leaves the
// live install as it was. Returns the rollback tree's path ("" when there was none).
func swapIn(dir string, pkg []byte) (string, error) {
	st := filepath.Join(dir, stagingDirName)
	live := filepath.Join(dir, "node_modules")
	prev := filepath.Join(dir, prevDirName)
	defer os.RemoveAll(st)
	if err := os.RemoveAll(prev); err != nil {
		return "", fmt.Errorf("clearing the old rollback copy %s: %w", prev, err)
	}
	// The .prev files a rollback restores are this swap's own: one left by an earlier swap (an old installer's
	// lockfile among them) must never come back as the live package.json or package-lock.json.
	for _, name := range []string{"package.json.prev", "package-lock.json.prev"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("clearing the old rollback file %s: %w", name, err)
		}
	}
	oldPkg, readErr := os.ReadFile(filepath.Join(dir, "package.json"))
	kept := ""
	if _, err := os.Stat(live); err == nil {
		if err := renameRetry(live, prev); err != nil {
			return "", fmt.Errorf("cannot move the live install aside (is a composition rendering?); nothing changed: %w", err)
		}
		kept = prev
		if readErr == nil {
			_ = os.WriteFile(filepath.Join(dir, "package.json.prev"), oldPkg, 0o644)
		}
	}
	// A lockfile an older installer left is a pin set for the tree being replaced: keep it as
	// package-lock.json.prev and take it out of the live directory.
	lockPath := filepath.Join(dir, "package-lock.json")
	oldLock, lockErr := os.ReadFile(lockPath)
	if lockErr == nil {
		_ = os.WriteFile(lockPath+".prev", oldLock, 0o644)
		_ = os.Remove(lockPath)
	}
	restore := func(cause error) error {
		if kept != "" {
			_ = os.RemoveAll(live)
			if err := renameRetry(prev, live); err != nil {
				return fmt.Errorf("%v; restoring the previous tree also failed: %v", cause, err)
			}
		}
		if readErr == nil {
			_ = os.WriteFile(filepath.Join(dir, "package.json"), oldPkg, 0o644)
		}
		if lockErr == nil {
			_ = os.WriteFile(lockPath, oldLock, 0o644)
		}
		return fmt.Errorf("%v; the previous install is back", cause)
	}
	if err := renameRetry(filepath.Join(st, "node_modules"), live); err != nil {
		return "", restore(fmt.Errorf("moving the new tree into place failed: %w", err))
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkg, 0o644); err != nil {
		return "", restore(fmt.Errorf("writing package.json: %w", err))
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

// rollback puts the previous tree and package.json back after a swapped-in tree failed
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
	cmd := winexec.CommandContext(ctx, name, args...)
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
