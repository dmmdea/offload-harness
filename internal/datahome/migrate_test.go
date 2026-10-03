package datahome

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/volumes"
)

// tree writes files (rel path -> content) under dir, each stamped an hour in the past
// so a copy that stamps "now" instead of preserving the mtime is visible.
func tree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot is every file under dir with its content hash: what "untouched" means.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, p)
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = string(sum[:])
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

func statuses(r Result) string {
	var rows []string
	for _, it := range r.Items {
		rows = append(rows, it.Rel+"="+string(it.Status))
	}
	sort.Strings(rows)
	return strings.Join(rows, " ")
}

func live() map[string]string {
	return map[string]string{
		"config.json":                     `{"home":"x"}`,
		"media/stt/a.srt":                 "1\n00:00 hello\n",
		"media/stt/a.segments.json":       `{"audio":"a.wav"}`,
		"delegation-log/2026-10-01.jsonl": strings.Repeat("{\"job\":1}\n", 50),
		"ledger.jsonl":                    "{\"task\":\"x\"}\n",
		"footprints.json":                 `{"entries":[]}`,
		"cache.db":                        "BOLTDATA",
	}
}

// TestRunCopiesAndNeverTouchesTheSource is the contract of the whole verb: the new home
// gets byte-identical copies with their original mtimes, and the old tree is exactly
// as it was. Moving was never an option (cached transcribe results carry absolute
// paths into the old media tree), so nothing here may delete or rewrite a source.
func TestRunCopiesAndNeverTouchesTheSource(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, live())
	before := snapshot(t, from)

	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Incomplete() {
		t.Fatalf("every file was readable and stable: %s", statuses(res))
	}
	if after := snapshot(t, from); !equalMaps(before, after) {
		t.Fatalf("the source tree changed: before %d files, after %d", len(before), len(after))
	}
	got := snapshot(t, to)
	for rel, sum := range before {
		if rel == "config.json" {
			if _, ok := got[rel]; ok {
				t.Errorf("config.json is where the harness finds its config; it must not be copied")
			}
			continue
		}
		if got[rel] != sum {
			t.Errorf("%s: destination bytes differ from the source", rel)
		}
	}
	src, _ := os.Stat(filepath.Join(from, "ledger.jsonl"))
	dst, err := os.Stat(filepath.Join(to, "ledger.jsonl"))
	if err != nil || !dst.ModTime().Equal(src.ModTime()) {
		t.Errorf("the copy must keep the source mtime so a later refresh can tell stale from new: %v vs %v (%v)", dst, src.ModTime(), err)
	}
	for _, name := range []string{".part"} {
		filepath.WalkDir(to, func(p string, d fs.DirEntry, _ error) error {
			if strings.HasSuffix(p, name) {
				t.Errorf("a half-written %s was left behind: %s", name, p)
			}
			return nil
		})
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestRunDryRunWritesNothing: without Apply the verb is a plan, and the destination is
// not even created.
func TestRunDryRunWritesNothing(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, live())
	res, err := Run(Options{From: from, To: to, Stopped: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, serr := os.Stat(to); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("a dry run must not create the destination (stat err %v)", serr)
	}
	if res.Applied || res.Count(StatusCopy) == 0 {
		t.Fatalf("a dry run must list what it would copy: %s", statuses(res))
	}
}

// TestRunHoldsBoltStoresUntilTheDoorsAreStopped: copying a bbolt file another process
// holds open gives a torn store (or, on Windows, a lock violation). The operator says
// the doors are stopped, or the store is held back and the run is not complete.
func TestRunHoldsBoltStoresUntilTheDoorsAreStopped(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, live())
	res, err := Run(Options{From: from, To: to, Apply: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(statuses(res), "cache.db=held") || !res.Incomplete() {
		t.Fatalf("cache.db must be held and the run reported incomplete: %s", statuses(res))
	}
	if _, serr := os.Stat(filepath.Join(to, "cache.db")); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("a held store must not be copied")
	}
	res, err = Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil || res.Incomplete() {
		t.Fatalf("with the doors stopped the second pass finishes the job: %v %s", err, statuses(res))
	}
	if _, serr := os.Stat(filepath.Join(to, "cache.db")); serr != nil {
		t.Fatalf("cache.db was not copied on the stopped pass: %v", serr)
	}
}

// TestRunIsIdempotentAndRefreshesWhatGrew: the usual order is a first pass with the
// doors up, then a second after stopping them. The second pass must be a cheap no-op
// for what did not change and a refresh for what did (the ledger grows while it runs).
func TestRunIsIdempotentAndRefreshesWhatGrew(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, live())
	if _, err := Run(Options{From: from, To: to, Apply: true, Stopped: true}); err != nil {
		t.Fatal(err)
	}
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil || res.Count(StatusCopy) != 0 || res.Count(StatusSame) == 0 {
		t.Fatalf("an immediate re-run copies nothing: %v %s", err, statuses(res))
	}
	grown := filepath.Join(from, "ledger.jsonl")
	if err := os.WriteFile(grown, []byte("{\"task\":\"x\"}\n{\"task\":\"y\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil || !strings.Contains(statuses(res), "ledger.jsonl=copy") {
		t.Fatalf("a grown ledger must be refreshed: %v %s", err, statuses(res))
	}
	want, _ := os.ReadFile(grown)
	got, _ := os.ReadFile(filepath.Join(to, "ledger.jsonl"))
	if !bytes.Equal(want, got) {
		t.Fatalf("the refreshed ledger differs from the source")
	}
}

// TestRunRefreshesAFileWhoseSizeDidNotChange: equal size is not equal content. A
// rewritten file of the same length (a counter file, a fixed-width record) must be
// recopied, which only the content hash can tell.
func TestRunRefreshesAFileWhoseSizeDidNotChange(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"thresholds.json": `{"t":0.1}`})
	if _, err := Run(Options{From: from, To: to, Apply: true, Stopped: true}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(from, "thresholds.json")
	if err := os.WriteFile(src, []byte(`{"t":0.9}`), 0o644); err != nil { // same length, newer mtime
		t.Fatal(err)
	}
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil || !strings.Contains(statuses(res), "thresholds.json=copy") {
		t.Fatalf("a same-size rewrite must be refreshed: %v %s", err, statuses(res))
	}
	if got, _ := os.ReadFile(filepath.Join(to, "thresholds.json")); string(got) != `{"t":0.9}` {
		t.Fatalf("the destination still holds the old content: %q", got)
	}
}

// TestRunNeverOverwritesANewerDestination: if the destination file was written after
// the source (the node already runs from the new home), the old tree must not clobber
// it. This is the guard against re-running the verb after the switch.
func TestRunNeverOverwritesANewerDestination(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"ledger.jsonl": "old line\n"})
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(to, "ledger.jsonl")
	if err := os.WriteFile(newer, []byte("written at the new home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(newer); string(b) != "written at the new home\n" {
		t.Fatalf("a newer destination was overwritten with %q", b)
	}
	if !strings.Contains(statuses(res), "ledger.jsonl=kept") {
		t.Fatalf("the report must say the destination was kept: %s", statuses(res))
	}
}

// TestRunRefusesUnsafeTargets: every shape of target that could lose or loop data
// refuses before anything is written.
func TestRunRefusesUnsafeTargets(t *testing.T) {
	base := t.TempDir()
	from := filepath.Join(base, "old")
	tree(t, from, live())
	cases := map[string]Options{
		"same path":               {From: from, To: from},
		"target inside source":    {From: from, To: filepath.Join(from, "media", "new")},
		"source inside target":    {From: from, To: base},
		"missing source":          {From: filepath.Join(base, "nope"), To: filepath.Join(t.TempDir(), "new")},
		"source is a file":        {From: filepath.Join(from, "ledger.jsonl"), To: filepath.Join(t.TempDir(), "new")},
		"empty target":            {From: from, To: ""},
		"target on the OS drive":  {From: from, To: `C:\local-offload`, OSDrive: `C:\`},
		"target on the OS drive/": {From: from, To: `c:/local-offload`, OSDrive: `C:\`},
	}
	for name, opt := range cases {
		opt.Apply = true
		before := snapshot(t, base)
		if _, err := Run(opt); err == nil {
			t.Errorf("%s: Run must refuse", name)
		}
		if after := snapshot(t, base); !equalMaps(before, after) {
			t.Errorf("%s: a refused run changed the tree", name)
		}
	}
}

// TestRunDoesNotFollowSymlinks: a link inside the tree (a junction the operator made
// by hand, which is what the rule forbids as a fix) is reported and skipped, never
// followed into somewhere else.
func TestRunDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	from, to := filepath.Join(base, "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"ledger.jsonl": "x\n"})
	outside := filepath.Join(base, "elsewhere")
	tree(t, outside, map[string]string{"secret.txt": "not part of the harness"})
	makeLink(t, outside, filepath.Join(from, "link"))
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(filepath.Join(to, "link", "secret.txt")); serr == nil {
		t.Fatalf("a symlink was followed")
	}
	if !strings.Contains(statuses(res), "link=skipped") {
		t.Fatalf("the link must be reported as skipped: %s", statuses(res))
	}
}

// TestRunRefusesALinkAsTheSource: a source that is itself a junction or symlink (what an
// operator does when told to move a directory and told not to) walks as an empty tree,
// which would read as a clean run that copied nothing. It is refused by name instead.
func TestRunRefusesALinkAsTheSource(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	tree(t, real, map[string]string{"ledger.jsonl": "row\n"})
	link := filepath.Join(base, "old")
	makeLink(t, real, link)
	_, err := Run(Options{From: link, To: filepath.Join(t.TempDir(), "new"), Apply: true, Stopped: true})
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a link as the source must be refused and say so, got %v", err)
	}
}

// makeLink links link -> target: a symlink where the platform allows one, else (an
// unprivileged Windows user) a directory junction, the very shape the data-drive rule
// forbids as a fix.
func makeLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a symlink or a junction here: %v %s", err, out)
		}
	}
	t.Skip("cannot create a symlink here")
}

// TestRunReportsAFileThatChangesWhileCopying: a copy of a file that grew under the
// reader is a torn snapshot. It is not accepted, nothing partial is left behind, and
// the run says what to do.
func TestRunReportsAFileThatChangesWhileCopying(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"delegation-log/a.jsonl": strings.Repeat("row\n", 100)})
	src := filepath.Join(from, "delegation-log", "a.jsonl")
	midCopyHook = func(path, _ string) {
		if path == src {
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString("appended while the copy ran\n")
			f.Close()
		}
	}
	t.Cleanup(func() { midCopyHook = nil })
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statuses(res), "delegation-log/a.jsonl=changed") || !res.Incomplete() {
		t.Fatalf("a file that moved under the copy must be reported changed and incomplete: %s", statuses(res))
	}
	if _, serr := os.Stat(filepath.Join(to, "delegation-log", "a.jsonl")); serr == nil {
		t.Fatalf("a torn copy was left in place")
	}
	if _, serr := os.Stat(filepath.Join(to, "delegation-log", "a.jsonl.part")); serr == nil {
		t.Fatalf("the temp file was left behind")
	}
}

// TestRunReportsAnUnreadableSource: a file another process holds (on Windows a bbolt
// store raises a lock violation) is reported with the remedy, not skipped silently.
func TestRunReportsAnUnreadableSource(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"ledger.jsonl": "x\n", "media/a.png": "png"})
	locked := filepath.Join(from, "media", "a.png")
	openSource = func(p string) (*os.File, error) {
		if p == locked {
			return nil, errors.New("the process cannot access the file because another process has locked a portion of the file")
		}
		return os.Open(p)
	}
	t.Cleanup(func() { openSource = os.Open })
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statuses(res), "media/a.png=unreadable") || !res.Incomplete() {
		t.Fatalf("an unreadable source must be reported and the run incomplete: %s", statuses(res))
	}
	if !strings.Contains(whyOf(res, "media/a.png"), "stop fleet-serve") {
		t.Fatalf("the report must carry the remedy, got %q", whyOf(res, "media/a.png"))
	}
	if b, rerr := os.ReadFile(filepath.Join(to, "ledger.jsonl")); rerr != nil || string(b) != "x\n" {
		t.Fatalf("the readable file must still be copied: %v %q", rerr, b)
	}
}

func whyOf(r Result, rel string) string {
	for _, it := range r.Items {
		if it.Rel == rel {
			return it.Why
		}
	}
	return ""
}

// TestRunRetriesAFileThatSettles: a live ledger moves between a stat and a read now and
// then. One such change is not a failure; the second attempt copies the settled file.
func TestRunRetriesAFileThatSettles(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	src := filepath.Join(from, "ledger.jsonl")
	appended := false
	midCopyHook = func(path, _ string) {
		if path == src && !appended {
			appended = true
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString("one late row\n")
			f.Close()
		}
	}
	t.Cleanup(func() { midCopyHook = nil })
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil || res.Incomplete() {
		t.Fatalf("a file that settles on the second attempt must be copied: %v %s", err, statuses(res))
	}
	want, _ := os.ReadFile(src)
	if got, _ := os.ReadFile(filepath.Join(to, "ledger.jsonl")); !bytes.Equal(got, want) {
		t.Fatalf("the copy must be the settled file, got %q want %q", got, want)
	}
}

// TestRunRefusesAWrittenCopyThatDoesNotMatch: every copy is verified by re-reading it.
// A destination that goes bad between the write and the check (a failing disk, a
// scanner rewriting the file) is removed and reported, never moved into place.
func TestRunRefusesAWrittenCopyThatDoesNotMatch(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	tree(t, from, map[string]string{"ledger.jsonl": "row\n"})
	midCopyHook = func(_, tmp string) {
		if err := os.WriteFile(tmp, []byte("garbage"), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { midCopyHook = nil })
	res, err := Run(Options{From: from, To: to, Apply: true, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statuses(res), "ledger.jsonl=failed") || !res.Incomplete() {
		t.Fatalf("a copy that fails verification must be reported failed: %s", statuses(res))
	}
	for _, name := range []string{"ledger.jsonl", "ledger.jsonl.part"} {
		if _, serr := os.Stat(filepath.Join(to, name)); serr == nil {
			t.Fatalf("%s was left behind after a failed verification", name)
		}
	}
}

// TestRunResolvesARelativeTargetBeforeTheOSDriveCheck: DriveOf answers "" for a path
// with no drive in it, so a relative --to (or a rooted one with no drive letter) used to
// count as "not on the OS drive" while resolving, against the working directory, right
// onto it. The guard reads the path as the filesystem will, not as typed. absPath stands
// in for the working directory so the case runs on every platform.
func TestRunResolvesARelativeTargetBeforeTheOSDriveCheck(t *testing.T) {
	oldAbs := absPath
	absPath = func(p string) (string, error) {
		switch p {
		case "relhome":
			return `C:\Windows\relhome`, nil
		case `\data`:
			return `C:\data`, nil
		}
		return oldAbs(p)
	}
	t.Cleanup(func() { absPath = oldAbs })
	base := t.TempDir()
	from := filepath.Join(base, "old")
	tree(t, from, live())
	// Run from a scratch directory: with the guard missing (which is what this test
	// exists to catch), a relative target is written under the working directory, and
	// that must be a temp directory, not the package. The rooted spelling is only ever
	// planned: unguarded, it would resolve to the root of the working directory's drive.
	t.Chdir(t.TempDir())
	for _, c := range []struct {
		to, resolved string
		apply        bool
	}{
		{"relhome", `C:\Windows\relhome`, false},
		{"relhome", `C:\Windows\relhome`, true},
		{`\data`, `C:\data`, false},
	} {
		before := snapshot(t, base)
		_, err := Run(Options{From: from, To: c.to, OSDrive: `C:\`, Apply: c.apply})
		if err == nil || !strings.Contains(err.Error(), "OS drive") {
			t.Errorf("To=%q apply=%v: a target that resolves onto the OS drive must be refused, got %v", c.to, c.apply, err)
			continue
		}
		if !strings.Contains(err.Error(), c.resolved) || !strings.Contains(err.Error(), "typed as "+c.to) {
			t.Errorf("To=%q: the refusal must name where it resolves (%s) and what was typed: %v", c.to, c.resolved, err)
		}
		if after := snapshot(t, base); !equalMaps(before, after) {
			t.Errorf("To=%q apply=%v: a refused run changed the tree", c.to, c.apply)
		}
	}
}

// TestRunRefusesARelativeTargetUnderTheRealWorkingDirectory is the same defect through the
// real filepath.Abs: the process sits on the OS drive and names a directory relative to it.
func TestRunRefusesARelativeTargetUnderTheRealWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	drive := volumes.DriveOf(cwd)
	if runtime.GOOS != "windows" || drive == "" {
		t.Skip("a working directory only sits on a drive letter on Windows")
	}
	base := t.TempDir()
	from := filepath.Join(base, "old")
	tree(t, from, live())
	t.Chdir(cwd)
	osDrive := drive + `\`
	// Applied: a relative directory under the working directory, which a refusal must leave absent.
	if _, err := Run(Options{From: from, To: "relhome", OSDrive: osDrive, Apply: true}); err == nil || !strings.Contains(err.Error(), "OS drive") {
		t.Errorf("a relative target under a working directory on the OS drive must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "relhome")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused run created relhome under the working directory (%v)", err)
	}
	// Planned only: a rooted path with no drive resolves to the working directory's drive,
	// and a plan must not create anything at a drive root even if the guard were missing.
	if _, err := Run(Options{From: from, To: `\c92-no-such-data-dir`, OSDrive: osDrive}); err == nil || !strings.Contains(err.Error(), "OS drive") {
		t.Errorf("a rooted target with no drive letter must be refused, got %v", err)
	}
	// A relative target is fine once the working directory is on another drive: the
	// check follows where the path lands, not whether it was typed with a letter.
	if _, err := Run(Options{From: from, To: "relhome", OSDrive: `Z:\`}); err != nil {
		t.Errorf("a relative target that lands off the OS drive must plan fine: %v", err)
	}
}

// TestRunReportsTheResolvedTarget: Result.To is the directory the copy goes to, absolute,
// because the caller prints it as the `home` line to put in config.json and a relative
// value there resolves against whatever directory the node next starts in.
func TestRunReportsTheResolvedTarget(t *testing.T) {
	base := t.TempDir()
	from := filepath.Join(base, "old")
	tree(t, from, live())
	t.Chdir(base)
	res, err := Run(Options{From: "old", To: "newhome"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(res.To) || filepath.Base(res.To) != "newhome" {
		t.Errorf("Result.To = %q, want the absolute path of newhome", res.To)
	}
	if !filepath.IsAbs(res.From) || filepath.Base(res.From) != "old" {
		t.Errorf("Result.From = %q, want the absolute path of old", res.From)
	}
}
