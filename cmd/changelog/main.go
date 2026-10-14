// Command changelog assembles the changelog fragments and checks them.
//
//	changelog assemble -title "<one line>" [-date YYYY-MM-DD] [-root DIR] [-dry-run]
//	changelog check    [-root DIR]
//
// A change adds one file under changes/unreleased/ and never edits CHANGELOG.md. assemble is the release step: it moves
// the fragments under a new `## [VERSION] - DATE - TITLE` heading of CHANGELOG.md, deletes them and prints the ready
// `Deletes:` line for the release commit. The version is the VERSION file's, so the order is bump first, assemble LAST
// (after the final merge of the main branch and the final renumbering of the version carriers). The rules live in
// internal/changelog; this file is the flags, the files and the exit codes. Format and recovery: changes/README.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/changelog"
)

const usage = `usage:
  changelog assemble -title "<one line>" [-date YYYY-MM-DD] [-root DIR] [-dry-run]
  changelog check    [-root DIR]

assemble moves the fragments of changes/unreleased/ (and any text left under [Unreleased]) under a new
"## [VERSION] - DATE - TITLE" heading of CHANGELOG.md, deletes the fragments and prints the "Deletes:" line for the
release commit. VERSION is read from the VERSION file, so bump it first and assemble last; DATE defaults to today.
-dry-run prints the section and writes and deletes nothing. check validates the fragments and CHANGELOG.md.
-root is the repository root (default ".").
Exit: 0 done, 1 refused or failed (CHANGELOG.md and the fragments are as they were, unless the output says a fragment
is still to be deleted), 2 a usage error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

// fileOps is every file operation the assembler retries or reports on, so a test can make one fail.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
	sleep  func(time.Duration)
}

func defaultOps() fileOps {
	return fileOps{rename: os.Rename, remove: os.Remove, sleep: time.Sleep}
}

// attempts is how often a rename or a remove is tried: on Windows another process (an editor, an indexer, an antivirus
// scan) can hold a file for a moment, and the same shape is retried in the rest of this repository.
const attempts = 5

// retry runs f up to attempts times, waiting a little longer after each failure.
func retry(ops fileOps, f func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = f(); err == nil {
			return nil
		}
		if i < attempts-1 {
			ops.sleep(time.Duration(i+1) * 100 * time.Millisecond)
		}
	}
	return err
}

// run is main with its streams and clock passed in. Exit codes: 0 done, 1 refused or failed, 2 a usage error.
func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	return runWith(args, stdout, stderr, now, defaultOps())
}

func runWith(args []string, stdout, stderr io.Writer, now func() time.Time, ops fileOps) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "assemble":
		return cmdAssemble(args[1:], stdout, stderr, now, ops)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "changelog: unknown command %q\n%s", args[0], usage)
	return 2
}

func newFlags(name string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	return fl
}

// parse reads the flags and refuses an argument that is not one.
func parse(fl *flag.FlagSet, args []string) error {
	if err := fl.Parse(args); err != nil {
		return fmt.Errorf("%s: %v", fl.Name(), err)
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("%s: unexpected argument %q", fl.Name(), fl.Arg(0))
	}
	return nil
}

func usageError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "changelog: %v\n%s", err, usage)
	return 2
}

func fragmentPath(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(changelog.FragmentDir), name)
}

func cmdAssemble(args []string, stdout, stderr io.Writer, now func() time.Time, ops fileOps) int {
	fl := newFlags("assemble")
	title := fl.String("title", "", "the one-line title of the release heading")
	date := fl.String("date", "", "the release date, YYYY-MM-DD (default: today)")
	root := fl.String("root", ".", "the repository root")
	dry := fl.Bool("dry-run", false, "print the section and write nothing")
	if err := parse(fl, args); err != nil {
		return usageError(stderr, err)
	}
	if *title == "" {
		return usageError(stderr, errors.New("assemble: -title is required"))
	}
	if *date == "" {
		*date = now().Format("2006-01-02")
	}

	// Everything is read, validated and spliced in memory before any file is touched.
	rawVersion, err := os.ReadFile(filepath.Join(*root, "VERSION"))
	if err != nil {
		fmt.Fprintln(stderr, "changelog:", err)
		return 1
	}
	if problems := changelog.CheckForRelease(*root); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, "changelog:", p)
		}
		return 1
	}
	frags, err := changelog.ReadFragments(filepath.Join(*root, filepath.FromSlash(changelog.FragmentDir)))
	if err != nil {
		fmt.Fprintln(stderr, "changelog:", err)
		return 1
	}
	changelogPath := filepath.Join(*root, changelog.ChangelogFile)
	raw, err := os.ReadFile(changelogPath)
	if err != nil {
		fmt.Fprintln(stderr, "changelog:", err)
		return 1
	}
	before := string(raw)
	rel := &changelog.Release{Version: strings.TrimSpace(string(rawVersion)), Date: *date, Title: *title}
	after, err := changelog.Splice(before, frags, rel)
	if err != nil {
		fmt.Fprintln(stderr, "changelog:", err)
		return 1
	}

	var deletes []string
	for _, f := range frags {
		deletes = append(deletes, changelog.FragmentDir+"/"+f.Name)
	}
	deletesLine := ""
	if len(deletes) > 0 {
		deletesLine = "Deletes: " + strings.Join(deletes, ", ") + "\n"
	}
	heading := fmt.Sprintf("## [%s] - %s - %s", rel.Version, rel.Date, rel.Title)

	if *dry {
		fmt.Fprintf(stdout, "%s\nchangelog: dry run, nothing written or deleted\n%s", releaseSection(before, after), deletesLine)
		return 0
	}

	if err := writeChangelog(changelogPath, after, ops); err != nil {
		fmt.Fprintf(stderr, "changelog: %s was not written, and nothing else changed: %v\n", changelog.ChangelogFile, err)
		return 1
	}
	var left []string
	for _, f := range frags {
		path := fragmentPath(*root, f.Name)
		err := retry(ops, func() error {
			if err := ops.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		})
		if err != nil {
			left = append(left, fmt.Sprintf("%s/%s (%v)", changelog.FragmentDir, f.Name, err))
		}
	}
	if len(left) > 0 {
		fmt.Fprintf(stderr, "changelog: %s is written, but %d fragment(s) could not be deleted. Delete them by hand "+
			"(until then `check` refuses them as already in %s):\n", changelog.ChangelogFile, len(left), changelog.ChangelogFile)
		for _, l := range left {
			fmt.Fprintln(stderr, "  "+l)
		}
		return 1
	}
	fmt.Fprintf(stdout, "changelog: assembled %d fragment(s) under %s\n%s", len(frags), heading, deletesLine)
	return 0
}

// releaseSection is the new section of after: from the new heading to the old top release heading.
func releaseSection(before, after string) string {
	_, oldEnd, ok := changelog.UnreleasedBlock(before)
	if !ok {
		return ""
	}
	_, newStart, ok := changelog.UnreleasedBlock(after)
	if !ok {
		return ""
	}
	oldRest := before[oldEnd:]
	return strings.TrimRight(after[newStart:len(after)-len(oldRest)], " \t\r\n")
}

// writeChangelog replaces path with text: a temp file in the same directory, then a rename over the file (which
// replaces it on Windows too), so a failure at any point leaves the file as it was. The mode of the file is kept.
func writeChangelog(path, text string, ops fileOps) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(text)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(name, info.Mode().Perm())); err != nil {
		_ = ops.remove(name)
		return err
	}
	if err := retry(ops, func() error { return ops.rename(name, path) }); err != nil {
		_ = ops.remove(name)
		return err
	}
	return nil
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fl := newFlags("check")
	root := fl.String("root", ".", "the repository root")
	if err := parse(fl, args); err != nil {
		return usageError(stderr, err)
	}
	if problems := changelog.CheckTree(*root); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, "changelog:", p)
		}
		return 1
	}
	frags, _ := changelog.ReadFragments(filepath.Join(*root, filepath.FromSlash(changelog.FragmentDir)))
	fmt.Fprintf(stdout, "changelog: ok (%d fragment(s) waiting)\n", len(frags))
	return 0
}
