package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/leakgate"
)

// The leak gate (H-55). This repository is PUBLIC and the list of names it must
// never hold is a secret, so the committed form of the list is a file of keyed
// hashes (testdata/leak-gate-digests.json) and the key lives outside the
// repository (an Actions secret in CI, a file in the maintainer's user config
// directory locally). docs/systems/leak-gate.md is the system document; the
// library is internal/leakgate and the generator cmd/leakdigest.
//
// Five tests, split by what they need:
//
//	TestTrackedTreeCarriesNoDeniedNames       needs the key (skips visibly without it
//	                                          unless OFFLOAD_LEAK_GATE_REQUIRED=1)
//	TestTrackedTreeCarriesNoShapedIdentifiers keyless: the shape rules over the tree
//	TestLeakGateKeyResolution                 keyless: the behaviour table of the key rows
//	TestLeakGateScannerBehaviour              keyless: a blind scanner fails everywhere
//	TestLeakGateDigestFileIsWellFormed        keyless: the committed file is sound
//
// No test here spells a name from the list, not even to assert it absent: the
// gate scans test sources like every other file. The canary comes from the
// library (built from two literals there), and every key is built at run time.

const (
	leakGateDigestPath = "testdata/leak-gate-digests.json"

	// The ratchets: raising either number is a reviewed change to this file.
	leakGateMaxAllowRows  = 0
	leakGateMaxExemptRows = 3

	// leakGateEnvPlain names a plaintext list (local only): findings are then
	// followed by a legend that resolves each id to its entry.
	leakGateEnvPlain = "OFFLOAD_LEAK_GATE_PLAIN"

	// leakGateListed is how many findings a failure prints.
	leakGateListed = 60
)

// leakGateDefaultKeyFile is the maintainer's key file: <user config dir>/
// offload-harness/leak-gate.key. Never a synced drive, never in the repository.
func leakGateDefaultKeyFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "offload-harness", "leak-gate.key")
}

// leakGateSetup is what the environment says the gate must do.
type leakGateSetup struct {
	Keys     leakgate.Keys
	Required bool
	Decision leakgate.Decision
}

// resolveLeakGate is the whole key-and-requirement decision of section 5.7 as
// one pure function (the environment and the file system are arguments), so the
// behaviour table runs without a key, a secret or a disk.
func resolveLeakGate(getenv func(string) string, readFile func(string) ([]byte, error), defaultFile string) (leakGateSetup, error) {
	required, err := leakgate.ParseRequired(getenv(leakgate.EnvRequired))
	if err != nil {
		return leakGateSetup{}, err
	}
	keys, err := leakgate.ResolveKey(getenv, readFile, defaultFile)
	if err != nil {
		return leakGateSetup{}, err
	}
	dec, err := leakgate.Decide(keys, required)
	if err != nil {
		return leakGateSetup{}, err
	}
	return leakGateSetup{Keys: keys, Required: required, Decision: dec}, nil
}

// leakGateSkipLine and leakGateEnforcedLine are the two summary lines a
// non-verbose `go test` would otherwise hide.
func leakGateSkipLine(reason string) string { return "leak gate: skipped (" + reason + ")" }

func leakGateEnforcedLine(files, entries int) string {
	return fmt.Sprintf("leak gate: enforced (%d files, %d entries)", files, entries)
}

// leakGateSummary logs a line and, when GITHUB_STEP_SUMMARY names a file,
// appends it there.
func leakGateSummary(t *testing.T, line string) {
	t.Helper()
	t.Log(line)
	p := os.Getenv("GITHUB_STEP_SUMMARY")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("leak gate: the step summary is not writable")
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// leakGateGitFiles is `git ls-files -z -s`: mode, blob id and path of every
// tracked file (the existing trackedFiles helper hides the mode, so a symlink or
// a submodule would be invisible to it).
func leakGateGitFiles() ([]leakgate.TrackedFile, error) {
	out, err := exec.Command("git", "ls-files", "-z", "-s").Output()
	if err != nil {
		return nil, err
	}
	return leakgate.ParseLsFiles(out)
}

// leakGateWalkFiles is the fallback for a source tarball with no git: every file
// under the current directory, with the git blob id computed for the files an
// exempt row names (so a row can still match).
func leakGateWalkFiles(exempt []leakgate.ExemptRow) ([]leakgate.TrackedFile, error) {
	want := map[string]bool{}
	for _, x := range exempt {
		want[x.Path] = true
	}
	var files []leakgate.TrackedFile
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(p)
		tf := leakgate.TrackedFile{Mode: "100644", Path: slash}
		if d.Type()&fs.ModeSymlink != 0 {
			tf.Mode = "120000"
		} else if want[slash] {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha1.Sum(append([]byte("blob "+strconv.Itoa(len(b))+"\x00"), b...))
			tf.Blob = hex.EncodeToString(sum[:])
		}
		files = append(files, tf)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

// leakGateOpenDirs lists every directory that holds a tracked file. `go test`
// caches a pass keyed by the files and DIRECTORIES the test opened (a directory
// by its listing); the file set comes from git, which the cache cannot see, so
// without this a newly tracked file would not invalidate a cached pass.
func leakGateOpenDirs(files []leakgate.TrackedFile) {
	seen := map[string]bool{}
	for _, f := range files {
		d := path.Dir(f.Path)
		if seen[d] {
			continue
		}
		seen[d] = true
		_, _ = os.ReadDir(filepath.FromSlash(d))
	}
}

func leakGateReadDigest() (*leakgate.DigestFile, error) {
	b, err := os.ReadFile(filepath.FromSlash(leakGateDigestPath))
	if err != nil {
		return nil, fmt.Errorf("digest file %s: %v", leakGateDigestPath, err)
	}
	return leakgate.ParseDigest(b)
}

// leakGateFailure is the failure text: counts, then up to leakGateListed lines
// that carry a path, a line and an entry id, never matched text.
func leakGateFailure(rep leakgate.Report, legend map[string]string) string {
	var lines []string
	paths := map[string]bool{}
	for _, f := range rep.Findings {
		l := f.String()
		if f.Mode != "shape" {
			if name, ok := legend[f.ID]; ok {
				l += "  <- " + name
			}
		}
		lines = append(lines, l)
		paths[f.Path] = true
	}
	for _, f := range rep.Fatals {
		where := f.Path
		if f.Line > 0 {
			where += ":" + strconv.Itoa(f.Line)
		}
		lines = append(lines, strings.TrimSpace("FATAL "+where+" "+f.Reason))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "leak gate: %d findings in %d files, %d fatal (of %d files scanned)\n", len(rep.Findings), len(paths), len(rep.Fatals), rep.Scanned)
	for i, l := range lines {
		if i == leakGateListed {
			fmt.Fprintf(&b, "  ... and %d more\n", len(lines)-leakGateListed)
			break
		}
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// leakGatePlainLegend resolves entry ids to entries when OFFLOAD_LEAK_GATE_PLAIN
// names a plaintext list. It is for a maintainer's own terminal: the legend
// prints the names, so it is never used in CI (the variable is not set there).
func leakGatePlainLegend(key []byte) map[string]string {
	p := os.Getenv(leakGateEnvPlain)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	es, err := leakgate.ParseList(b)
	if err != nil {
		return nil
	}
	legend := map[string]string{}
	for _, e := range leakgate.WithCanary(es) {
		legend[leakgate.EntryDigest(key, e).D[:8]] = string(e.Mode) + " " + e.Text
	}
	return legend
}

// TestTrackedTreeCarriesNoDeniedNames is the keyed half of the gate: every
// tracked file, and every tracked file NAME, against the hashed list.
//
// Without a key it skips visibly (a fork's pull request, a contributor's
// machine, a source tarball) unless OFFLOAD_LEAK_GATE_REQUIRED=1, which CI sets
// on a push and on a same-repository pull request, where a missing key is a
// failure. Run it locally with
//
//	OFFLOAD_LEAK_GATE_KEY_FILE=<key file> go test -count=1 -run TestTrackedTreeCarriesNoDeniedNames .
//
// (-count=1: a key file is outside the module, so go's test cache cannot see it
// change.) The rows of the behaviour matrix are in docs/systems/leak-gate.md.
func TestTrackedTreeCarriesNoDeniedNames(t *testing.T) {
	setup, err := resolveLeakGate(os.Getenv, os.ReadFile, leakGateDefaultKeyFile())
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	if !setup.Decision.Enforce {
		leakGateSummary(t, leakGateSkipLine(setup.Decision.SkipReason))
		t.Skip(setup.Decision.SkipReason)
	}

	df, err := leakGateReadDigest()
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	key, err := leakgate.SelectKey(df, setup.Keys)
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	m, err := leakgate.NewDigestMatcher(df, key)
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	if err := m.SelfTest(); err != nil {
		t.Fatalf("leak gate: %v", err)
	}

	files, err := leakGateGitFiles()
	if err != nil || len(files) == 0 {
		if setup.Required {
			t.Fatalf("leak gate: %v: git ls-files gave no file set", leakgate.ErrBlind)
		}
		reason := "no git file set (a source tarball)"
		leakGateSummary(t, leakGateSkipLine(reason))
		t.Skip(reason)
	}
	leakGateOpenDirs(files)

	rep := leakgate.ScanTree(m, files, leakgate.DirFS{Root: "."}, leakgate.ScanOptions{
		Required: setup.Required,
		Exempt:   df.Exempt,
		Allow:    df.Allow,
	})
	if len(rep.Findings) > 0 || len(rep.Fatals) > 0 {
		t.Fatalf("%s", leakGateFailure(rep, leakGatePlainLegend(key)))
	}
	if rep.Missing > 0 {
		t.Logf("leak gate: %d tracked path(s) missing from the work tree (their names were scanned)", rep.Missing)
	}
	leakGateSummary(t, leakGateEnforcedLine(rep.Scanned, len(df.Entries)))
}

// TestTrackedTreeCarriesNoShapedIdentifiers is the keyless half: the shape rules
// (a GPU id, a WSL UNC path naming a real distro) over the same fail-closed file
// set, with no key, so a fork and a future card are covered too. It reads the
// exempt rows of the digest file (they need no key); the keyed test above runs
// the entries only, so nothing is reported twice.
func TestTrackedTreeCarriesNoShapedIdentifiers(t *testing.T) {
	required, err := leakgate.ParseRequired(os.Getenv(leakgate.EnvRequired))
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	df, err := leakGateReadDigest()
	if err != nil {
		t.Fatalf("leak gate: %v", err)
	}
	files, err := leakGateGitFiles()
	if err != nil || len(files) == 0 {
		if required {
			t.Fatalf("leak gate: %v: git ls-files gave no file set", leakgate.ErrBlind)
		}
		t.Logf("no git file set (a source tarball): walking the tree")
		if files, err = leakGateWalkFiles(df.Exempt); err != nil || len(files) == 0 {
			t.Fatalf("leak gate: %v: the tree walk gave no file set", leakgate.ErrBlind)
		}
	}
	leakGateOpenDirs(files)

	rep := leakgate.ScanTree(nil, files, leakgate.DirFS{Root: "."}, leakgate.ScanOptions{
		Required: required,
		Shapes:   true,
		Exempt:   df.Exempt,
	})
	if len(rep.Findings) > 0 || len(rep.Fatals) > 0 {
		t.Fatalf("%s", leakGateFailure(rep, nil))
	}
	if rep.Missing > 0 {
		t.Logf("leak gate: %d tracked path(s) missing from the work tree (their names were scanned)", rep.Missing)
	}
}

// keyTable drives resolveLeakGate: no row reads a real environment variable or a
// real file, so the table runs the same on a fork, a laptop and a runner.
type keyTable struct {
	env   map[string]string
	files map[string]string
	errs  map[string]error
	reads []string
}

func (k *keyTable) getenv(name string) string { return k.env[name] }

func (k *keyTable) readFile(p string) ([]byte, error) {
	k.reads = append(k.reads, p)
	if err, ok := k.errs[p]; ok {
		return nil, err
	}
	if s, ok := k.files[p]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

// TestLeakGateKeyResolution is the behaviour table of sections 5.7 and 6: what
// the gate does for every combination of key, previous key, key file, default
// file and OFFLOAD_LEAK_GATE_REQUIRED, including the empty values an unset
// Actions secret renders as. It needs no key.
func TestLeakGateKeyResolution(t *testing.T) {
	hex64 := func(pair string) string { return strings.Repeat(pair, 32) }
	good, other, prev := hex64("ab"), hex64("cd"), hex64("ef")
	const defPath = "cfg/offload-harness/leak-gate.key"

	type outcome int
	const (
		enforce outcome = iota
		skip
		fail
	)
	rows := []struct {
		name     string
		env      map[string]string
		files    map[string]string
		errs     map[string]error
		want     outcome
		wantErr  error
		wantPrev bool
		noFile   string // a path the reader must never be asked for
	}{
		// 1: a key and a valid digest file enforce, required or not.
		{name: "1 key, requirement off", env: map[string]string{leakgate.EnvKey: good}, want: enforce, noFile: defPath},
		{name: "1 key, required", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvRequired: "1"}, want: enforce},
		{name: "key with whitespace and a CRLF", env: map[string]string{leakgate.EnvKey: "  " + good + "\r\n"}, want: enforce},
		{name: "6a a key and an empty previous key (every normal CI run)", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvKeyPrev: "", leakgate.EnvRequired: "1"}, want: enforce},
		{name: "6a a key and a whitespace-only previous key", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvKeyPrev: " \t"}, want: enforce},
		{name: "a key and a valid previous key (rotation)", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvKeyPrev: prev}, want: enforce, wantPrev: true},

		// 4: no key where secrets exist is a failure, never a skip.
		{name: "4 no key, required", env: map[string]string{leakgate.EnvRequired: "1"}, want: fail, wantErr: leakgate.ErrKeyMissing},
		{name: "4 empty key (an unset secret), required", env: map[string]string{leakgate.EnvKey: "", leakgate.EnvRequired: "1"}, want: fail, wantErr: leakgate.ErrKeyMissing},
		{name: "4 whitespace-only key, required", env: map[string]string{leakgate.EnvKey: " \r\n", leakgate.EnvRequired: "1"}, want: fail, wantErr: leakgate.ErrKeyMissing},
		{name: "6c a previous key alone never counts, required", env: map[string]string{leakgate.EnvKeyPrev: prev, leakgate.EnvRequired: "1"}, want: fail, wantErr: leakgate.ErrKeyMissing},

		// 6: no key and no requirement skips, with a reason.
		{name: "6 no key, requirement off (a fork, a contributor, a tarball)", want: skip},
		{name: "6 empty key, requirement off (a fork's pull request)", env: map[string]string{leakgate.EnvKey: "", leakgate.EnvKeyPrev: "", leakgate.EnvRequired: ""}, want: skip},
		{name: "6 whitespace-only key, requirement off", env: map[string]string{leakgate.EnvKey: "  "}, want: skip},
		{name: "6c a valid previous key alone, requirement off", env: map[string]string{leakgate.EnvKeyPrev: prev}, want: skip},
		{name: "6c a garbage previous key without a key is ignored", env: map[string]string{leakgate.EnvKeyPrev: "garbage"}, want: skip},

		// 5: a non-empty key that is not 64 hex characters is malformed.
		{name: "5 key too short", env: map[string]string{leakgate.EnvKey: good[:63]}, want: fail, wantErr: leakgate.ErrMalformedKey},
		{name: "5 key too long", env: map[string]string{leakgate.EnvKey: good + "a"}, want: fail, wantErr: leakgate.ErrMalformedKey},
		{name: "5 key not hex, required", env: map[string]string{leakgate.EnvKey: strings.Repeat("zz", 32), leakgate.EnvRequired: "1"}, want: fail, wantErr: leakgate.ErrMalformedKey},

		// 6b: a non-empty previous key that is not 64 hex characters.
		{name: "6b malformed previous key", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvKeyPrev: "bad"}, want: fail, wantErr: leakgate.ErrMalformedPrevKey},

		// 10: a requirement that is neither empty nor exactly 1.
		{name: "10 required=true", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvRequired: "true"}, want: fail, wantErr: leakgate.ErrBadRequired},
		{name: "10 required=0", env: map[string]string{leakgate.EnvRequired: "0"}, want: fail, wantErr: leakgate.ErrBadRequired},
		{name: "10 required with a space", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvRequired: " 1"}, want: fail, wantErr: leakgate.ErrBadRequired},

		// 6d: the key file.
		{name: "6d empty key file value is not consulted", env: map[string]string{leakgate.EnvKeyFile: ""}, want: skip, noFile: "k.txt"},
		{name: "6d whitespace-only key file value is not consulted", env: map[string]string{leakgate.EnvKeyFile: "  "}, want: skip, noFile: "k.txt"},
		{name: "6d key file", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other}, want: enforce},
		{name: "6d key file, CRLF-terminated", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other + "\r\n"}, want: enforce},
		{name: "6d key file absent", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "6d key file absent, requirement off", env: map[string]string{leakgate.EnvKeyFile: "k.txt", leakgate.EnvRequired: ""}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "6d key file unreadable", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, errs: map[string]error{"k.txt": errors.New("denied")}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "6d key file malformed", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": "not a key"}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "6d empty key file", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": ""}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "a key beats a key file, which is never read", env: map[string]string{leakgate.EnvKey: good, leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other}, want: enforce, noFile: "k.txt"},
		{name: "an empty key falls through to the key file", env: map[string]string{leakgate.EnvKey: "", leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other}, want: enforce},
		{name: "a key file with a previous key", env: map[string]string{leakgate.EnvKeyFile: "k.txt", leakgate.EnvKeyPrev: prev}, files: map[string]string{"k.txt": other}, want: enforce, wantPrev: true},

		// The default file (a maintainer's machine).
		{name: "default file present", files: map[string]string{defPath: good}, want: enforce},
		{name: "default file present, CRLF and a blank line", files: map[string]string{defPath: good + "\r\n\r\n"}, want: enforce},
		{name: "default file malformed", files: map[string]string{defPath: "short"}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "default file unreadable", errs: map[string]error{defPath: errors.New("denied")}, want: fail, wantErr: leakgate.ErrKeyFile},
		{name: "the key file beats the default file", env: map[string]string{leakgate.EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other, defPath: good}, want: enforce},
		{name: "a key beats the default file, which is never read", env: map[string]string{leakgate.EnvKey: good}, files: map[string]string{defPath: other}, want: enforce, noFile: defPath},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			kt := &keyTable{env: r.env, files: r.files, errs: r.errs}
			setup, err := resolveLeakGate(kt.getenv, kt.readFile, defPath)
			if r.noFile != "" {
				for _, p := range kt.reads {
					if p == r.noFile {
						t.Errorf("the reader was asked for %q", p)
					}
				}
			}
			switch r.want {
			case fail:
				if err == nil || !errors.Is(err, r.wantErr) {
					t.Fatalf("err = %v, want %v", err, r.wantErr)
				}
				for _, k := range []string{good, other, prev} {
					if strings.Contains(err.Error(), k) || strings.Contains(err.Error(), k[:16]) {
						t.Errorf("the error carries key text: %q", err)
					}
				}
				for _, hint := range []string{"63", "64", "65", "length", "expected", "bytes"} {
					if strings.Contains(strings.ToLower(err.Error()), hint) {
						t.Errorf("the error hints at the key (%q): %q", hint, err)
					}
				}
				if r.wantErr == leakgate.ErrKeyMissing && strings.Contains(err.Error(), "malformed") {
					t.Errorf("an empty key is unset, never malformed: %q", err)
				}
			case skip:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if setup.Decision.Enforce || setup.Decision.SkipReason == "" {
					t.Errorf("decision = %+v, want a skip with a reason", setup.Decision)
				}
				if line := leakGateSkipLine(setup.Decision.SkipReason); !strings.HasPrefix(line, "leak gate: skipped (") || !strings.HasSuffix(line, ")") {
					t.Errorf("summary line = %q", line)
				}
			case enforce:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !setup.Decision.Enforce || setup.Keys.Key == nil {
					t.Errorf("decision = %+v, want enforce with a key", setup.Decision)
				}
				if (setup.Keys.Prev != nil) != r.wantPrev {
					t.Errorf("previous key present = %v, want %v", setup.Keys.Prev != nil, r.wantPrev)
				}
			}
		})
	}
}

// memTree is a tiny in-memory file system for the scanner behaviour test.
type memTree map[string]string

func (m memTree) Read(p string, max int64) ([]byte, int64, error) {
	s, ok := m[p]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return []byte(s), int64(len(s)), nil
}

// TestLeakGateScannerBehaviour runs everywhere, with a synthetic key and synthetic
// names (the full 108-case matrix is in internal/leakgate). It exists so that a
// blind scanner fails even where the keyed tree gate skips: the canary entry is
// always appended to a digest file, and a digest matcher built with the wrong key
// cannot flag it.
func TestLeakGateScannerBehaviour(t *testing.T) {
	keyBytes := func(pair string) []byte {
		k, err := leakgate.DecodeKey(strings.Repeat(pair, 32))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	entries, err := leakgate.ParseList([]byte("sub\tzorblax\nword\tquuxel\nphrase\tplugh xyzzy\n"))
	if err != nil {
		t.Fatal(err)
	}
	key, wrong := keyBytes("5a"), keyBytes("a5")
	df, err := leakgate.BuildDigest(key, entries, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(df.Entries), len(entries)+1; got != want {
		t.Fatalf("digest file holds %d entries, want %d (the canary is always appended)", got, want)
	}
	m, err := leakgate.NewDigestMatcher(df, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SelfTest(); err != nil {
		t.Fatalf("a matcher built with the right key must flag the canary: %v", err)
	}
	blind, err := leakgate.NewDigestMatcher(df, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if blind.SelfTest() == nil {
		t.Fatal("a matcher built with the wrong key flagged the canary: the self-test cannot see a blind gate")
	}
	if _, err := leakgate.SelectKey(df, leakgate.Keys{Key: wrong}); !errors.Is(err, leakgate.ErrMACMismatch) {
		t.Fatalf("SelectKey with the wrong key: %v, want the MAC mismatch", err)
	}

	canary := leakgate.CanaryText()
	tree := memTree{
		"docs/a.md":         "clean prose\n",
		"docs/b.md":         "a line\nnote " + canary + " here\n",
		"docs/zorblax-c.md": "clean body\n",
		"docs/d.md":         "the quuxel, and Plugh\nXyzzy on the next line\n",
		"docs/e.md":         "plugh xyzzy together\n",
		"docs/f.md":         "quuxels is not the word\n",
		"docs/" + canary:    "x\n",
	}
	var files []leakgate.TrackedFile
	for p := range tree {
		files = append(files, leakgate.TrackedFile{Mode: "100644", Blob: strings.Repeat("0", 40), Path: p})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	rep := leakgate.ScanTree(m, files, tree, leakgate.ScanOptions{})
	if len(rep.Fatals) != 0 {
		t.Fatalf("fatals: %+v", rep.Fatals)
	}
	got := map[string]int{}
	for _, f := range rep.Findings {
		got[f.DisplayPath()]++
		if f.Text != "" {
			t.Errorf("a digest finding carries matched text: %+v", f)
		}
		// The path is part of a finding line (a name finding's path is the name),
		// so the entry part is what must be free of matched text.
		if l := f.Label(); !strings.HasPrefix(l, "id=") || strings.Contains(l, "zorblax") || strings.Contains(l, "quuxel") || strings.Contains(strings.ToLower(l), canary) {
			t.Errorf("a finding label carries matched text: %q", l)
		}
	}
	want := map[string]int{
		"docs/b.md":                  1, // the canary in a body
		"docs/d.md":                  1, // a word entry; a phrase never crosses a line
		"docs/e.md":                  1, // a phrase entry
		"docs/zorblax-c.md (name)":   1, // a name finding in a clean body
		"docs/" + canary + " (name)": 1, // the canary in a file name
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("findings per file = %v, want %v", got, want)
	}
}

// TestLeakGateDigestFileIsWellFormed: the committed digest file is the gate's
// only input in the repository, so a malformed one must fail here, with no key,
// on every machine and every pull request, fork or not.
func TestLeakGateDigestFileIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(leakGateDigestPath))
	if err != nil {
		t.Fatalf("digest file: %v", err)
	}
	df, err := leakgate.ParseDigest(raw) // strict: version, algo, hex lengths, sorted unique entries, no unknown field
	if err != nil {
		t.Fatalf("digest file: %v", err)
	}
	if len(df.Entries) == 0 {
		t.Fatal("digest file has no entries")
	}
	for i, e := range df.Entries {
		if e.D == "" || e.Mode == "" {
			t.Errorf("entry %d is incomplete", i)
		}
	}
	// The committed form is the library's canonical one (LF, one row per line).
	canon, err := df.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != string(canon) {
		t.Error("digest file is not in the canonical form leakdigest gen writes: regenerate it, do not edit it")
	}
	// The ratchets: raising either number is a reviewed change to this file.
	if n := len(df.Allow); n > leakGateMaxAllowRows {
		t.Errorf("%d allow rows, the ratchet is %d: a finding is fixed in the file, never allowed", n, leakGateMaxAllowRows)
	}
	if n := len(df.Exempt); n > leakGateMaxExemptRows {
		t.Errorf("%d exempt rows, the ratchet is %d", n, leakGateMaxExemptRows)
	}
	// Every exempt row names a tracked file whose blob id it carries (a stale
	// row would turn the keyed test red anyway; this says so without a key).
	blobs := map[string]string{}
	if files, err := leakGateGitFiles(); err == nil {
		for _, f := range files {
			blobs[f.Path] = f.Blob
		}
	} else {
		t.Logf("no git file set (%v): exempt rows checked for presence only, not for their blob ids", err)
	}
	for _, x := range df.Exempt {
		if _, err := os.Stat(filepath.FromSlash(x.Path)); err != nil {
			t.Errorf("exempt path %s does not exist in the tree", x.Path)
			continue
		}
		if len(blobs) > 0 {
			if b, ok := blobs[x.Path]; !ok {
				t.Errorf("exempt path %s is not tracked", x.Path)
			} else if b != x.Blob {
				t.Errorf("exempt row for %s is stale: regenerate the digest file", x.Path)
			}
		}
	}
}
