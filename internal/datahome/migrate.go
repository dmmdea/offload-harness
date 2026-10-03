package datahome

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dmmdea/offload-harness/internal/volumes"
)

// Status is what happened to one file of the migration.
type Status string

const (
	// StatusCopy: the file is (Apply) or would be (dry run) copied, new or refreshed.
	StatusCopy Status = "copy"
	// StatusSame: the destination already holds identical bytes.
	StatusSame Status = "same"
	// StatusKept: the destination differs but is NEWER than the source, so the node
	// has already written there; it is left alone. Informational, not incomplete.
	StatusKept Status = "kept"
	// StatusHeld: a bbolt store, held back until the operator says the doors are
	// stopped. Copying a live one tears it.
	StatusHeld Status = "held"
	// StatusSkipped: deliberately not carried (see skipReason).
	StatusSkipped Status = "skipped"
	// StatusUnreadable: the source could not be read, typically because a running
	// process holds it open.
	StatusUnreadable Status = "unreadable"
	// StatusChanged: the source changed while it was being copied, so the copy was
	// refused as a torn snapshot.
	StatusChanged Status = "changed"
	// StatusFailed: the destination side failed (disk full, permission, verify).
	StatusFailed Status = "failed"
)

// Options is one migration.
type Options struct {
	// From is the tree the node runs from today; To is the new home.
	From, To string
	// OSDrive refuses a To on that drive ("" = no check). The rule the verb exists
	// for is that data does not live there.
	OSDrive string
	// Apply performs the copy; without it Run only plans.
	Apply bool
	// Stopped is the operator's statement that fleet-serve and every MCP door are
	// stopped. Only then are bbolt stores (*.db) copied.
	Stopped bool
}

// Item is one file's outcome.
type Item struct {
	Rel    string `json:"rel"`
	Size   int64  `json:"size"`
	Status Status `json:"status"`
	Why    string `json:"why,omitempty"`
}

// Result is the whole run.
type Result struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Applied bool   `json:"applied"`
	Items   []Item `json:"items"`
}

// Count is how many items have status s.
func (r Result) Count(s Status) int {
	n := 0
	for _, it := range r.Items {
		if it.Status == s {
			n++
		}
	}
	return n
}

// Bytes sums the sizes of the items with status s.
func (r Result) Bytes(s Status) int64 {
	var n int64
	for _, it := range r.Items {
		if it.Status == s {
			n += it.Size
		}
	}
	return n
}

// Incomplete reports whether something still needs the operator before `home` can
// be switched: a held store, an unreadable or changed source, a failed write. A kept
// destination and a skipped file do not count; both are decisions, not gaps.
func (r Result) Incomplete() bool {
	return r.Count(StatusHeld)+r.Count(StatusUnreadable)+r.Count(StatusChanged)+r.Count(StatusFailed) > 0
}

// Test seams: midCopyHook runs after the bytes are read and written to tmp and before
// the source is re-stat'ed and the copy verified (a writer appending while the copy
// runs, or a destination that goes bad); openSource opens a source.
var (
	midCopyHook func(src, tmp string)
	openSource  = os.Open
)

// absPath resolves a relative path against the working directory; a test seam, so the
// OS-drive guard can be exercised for a relative target on any platform.
var absPath = filepath.Abs

// maxAttempts bounds how often a file that keeps changing under the reader is
// re-copied before the run reports it. A live ledger changes between a stat and a
// read now and then; one that never holds still belongs to a process that must stop.
const maxAttempts = 3

// Run plans (Apply false) or performs (Apply true) the copy of From into To.
//
// It never moves, deletes or rewrites anything under From, never follows a link, and
// never overwrites a destination file that is newer than its source. Every copy goes
// to a temp name, is verified by re-reading it, takes the source's mtime, and only
// then replaces the destination, so an interrupted run leaves whole files or none.
// config.json is not carried: the harness finds its config by a fixed path, and that
// file is how it learns the new home in the first place.
//
// From and To are resolved to absolute paths first, and everything below (the OS-drive
// guard, the same-tree checks, Result.To the caller prints as the `home` line) works on
// the resolved form. DriveOf answers "" for a relative path or a rooted one with no
// drive letter, so the guard reading the typed string let `--to relhome` through while
// it resolved against the working directory, onto the OS drive.
func Run(opt Options) (Result, error) {
	typedTo := opt.To
	var rerr error
	if opt.From, rerr = resolveDir(opt.From); rerr == nil {
		opt.To, rerr = resolveDir(opt.To)
	}
	res := Result{From: opt.From, To: opt.To, Applied: opt.Apply}
	if rerr != nil {
		return res, fmt.Errorf("resolving the paths: %w", rerr)
	}
	if err := checkTargets(opt, typedTo); err != nil {
		return res, err
	}
	from, to := filepath.Clean(opt.From), filepath.Clean(opt.To)
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(from, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
			return nil
		case d.IsDir():
			if opt.Apply {
				return os.MkdirAll(filepath.Join(to, filepath.FromSlash(rel)), 0o755)
			}
			return nil
		case !d.Type().IsRegular():
			// A symlink or a Windows junction: following it would copy something that
			// is not this tree (or loop). Reported so it is not mistaken for missing.
			res.Items = append(res.Items, Item{Rel: rel, Status: StatusSkipped, Why: "a link, not followed"})
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			res.Items = append(res.Items, Item{Rel: rel, Status: StatusUnreadable, Why: ierr.Error()})
			return nil
		}
		res.Items = append(res.Items, handleFile(opt, p, filepath.Join(to, filepath.FromSlash(rel)), rel, info))
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("walking %s: %w", from, err)
	}
	return res, nil
}

// resolveDir makes a directory the operator named absolute. A path that already names a
// drive (any spelling) is kept as written, so the answer does not depend on the host the
// code runs on; a relative or drive-less one resolves against the working directory.
func resolveDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || volumes.DriveOf(p) != "" {
		return p, nil
	}
	return absPath(p)
}

// checkTargets refuses every From/To pair that could lose or loop data, before any
// write: a missing or non-directory source, the same tree, one inside the other, a
// target on the OS drive. opt carries resolved paths; typedTo is what the operator wrote.
func checkTargets(opt Options, typedTo string) error {
	if strings.TrimSpace(opt.From) == "" || strings.TrimSpace(opt.To) == "" {
		return errors.New("both a source (--from) and a target (--to) are needed")
	}
	if opt.OSDrive != "" && volumes.OnDrive(opt.To, opt.OSDrive) {
		wrote := ""
		if t := strings.TrimSpace(typedTo); t != opt.To {
			wrote = " (typed as " + t + ")"
		}
		return fmt.Errorf("target %s%s is on the OS drive %s: that is the drive the data is leaving", opt.To, wrote, opt.OSDrive)
	}
	// Lstat, not Stat: a source that is itself a link (the junction an operator put
	// at the old path, which the data-drive rule forbids as a fix) would walk as an
	// empty tree and report a clean zero-file run. Name the real directory instead.
	info, err := os.Lstat(opt.From)
	if err != nil {
		return fmt.Errorf("source %s: %w", opt.From, err)
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("source %s is a link (a junction or symlink): pass the real directory it points at as --from", opt.From)
	}
	if !info.IsDir() {
		return fmt.Errorf("source %s is not a directory", opt.From)
	}
	a, b := normalize(opt.From), normalize(opt.To)
	switch {
	case a == b:
		return fmt.Errorf("source and target are the same directory (%s): the node already runs from there", opt.To)
	case strings.HasPrefix(b, a+"/"):
		return fmt.Errorf("target %s is inside the source %s: the copy would feed itself", opt.To, opt.From)
	case strings.HasPrefix(a, b+"/"):
		return fmt.Errorf("source %s is inside the target %s: the target would contain the tree it replaces", opt.From, opt.To)
	}
	return nil
}

// normalize makes a path comparable: absolute, cleaned, forward slashes, lower case on
// Windows (drive letters and NTFS names do not care about case).
func normalize(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = strings.TrimRight(filepath.ToSlash(filepath.Clean(p)), "/")
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// skipReason names files that are not carried, with the reason.
func skipReason(rel string) string {
	base := path.Base(rel)
	switch {
	case rel == "config.json":
		return "the config file the harness resolves by a fixed path; it stays where it is"
	case strings.HasPrefix(base, "cache.p") && strings.HasSuffix(base, ".db"):
		return "a stale per-process sibling of the result cache"
	}
	return ""
}

// isBolt reports whether rel is a bbolt store, which only a stopped door may copy.
func isBolt(rel string) bool { return strings.HasSuffix(strings.ToLower(rel), ".db") }

func handleFile(opt Options, src, dst, rel string, info fs.FileInfo) Item {
	it := Item{Rel: rel, Size: info.Size()}
	if why := skipReason(rel); why != "" {
		it.Status, it.Why = StatusSkipped, why
		return it
	}
	if isBolt(rel) && !opt.Stopped {
		it.Status = StatusHeld
		it.Why = "a bbolt store: copying one a running door holds open tears it; stop fleet-serve and the MCP doors, then re-run with --stopped"
		return it
	}
	if dinfo, err := os.Stat(dst); err == nil && dinfo.Mode().IsRegular() {
		same, herr := sameBytes(src, dst, info, dinfo)
		switch {
		case herr != nil:
			it.Status, it.Why = StatusUnreadable, unreadableWhy(herr)
			return it
		case same:
			it.Status = StatusSame
			return it
		case dinfo.ModTime().After(info.ModTime()):
			it.Status = StatusKept
			it.Why = "the destination is newer than the source (the node has written there); not overwritten"
			return it
		}
	}
	it.Status = StatusCopy
	if !opt.Apply {
		return it
	}
	for attempt := 1; ; attempt++ {
		status, why := copyOne(src, dst)
		if status == StatusChanged && attempt < maxAttempts {
			continue
		}
		it.Status, it.Why = status, why
		return it
	}
}

func unreadableWhy(err error) string {
	return fmt.Sprintf("cannot read the source: %v (if a running process holds it, stop fleet-serve and the MCP doors, then re-run)", err)
}

// sameBytes compares source and destination by size, then by content hash.
func sameBytes(src, dst string, sinfo, dinfo fs.FileInfo) (bool, error) {
	if sinfo.Size() != dinfo.Size() {
		return false, nil
	}
	a, err := hashFile(src, true)
	if err != nil {
		return false, err
	}
	b, err := hashFile(dst, false)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(a, b), nil
}

func hashFile(p string, isSource bool) ([]byte, error) {
	open := os.Open
	if isSource {
		open = openSource
	}
	f, err := open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// copyOne copies one file through a temp name, verifies it and moves it into place.
// The source is stat'ed afresh on every call, so a retry measures the file as it is
// now and not as the walk first saw it.
func copyOne(src, dst string) (Status, string) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return StatusFailed, err.Error()
	}
	before, err := os.Stat(src)
	if err != nil {
		return StatusUnreadable, unreadableWhy(err)
	}
	in, err := openSource(src)
	if err != nil {
		return StatusUnreadable, unreadableWhy(err)
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return StatusFailed, err.Error()
	}
	cleanup := func() { out.Close(); os.Remove(tmp) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		cleanup()
		// A failed READ is the source's problem (a lock violation), a failed WRITE is
		// the destination's; io.Copy does not say which, so re-read a byte to tell.
		if _, perr := readProbe(src); perr != nil {
			return StatusUnreadable, unreadableWhy(perr)
		}
		return StatusFailed, err.Error()
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return StatusFailed, err.Error()
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return StatusFailed, err.Error()
	}
	if midCopyHook != nil {
		midCopyHook(src, tmp)
	}
	after, err := os.Stat(src)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || n != before.Size() {
		os.Remove(tmp)
		return StatusChanged, "the source changed while it was being copied; stop fleet-serve and the MCP doors, then re-run"
	}
	got, err := hashFile(tmp, false)
	if err != nil || !bytes.Equal(got, h.Sum(nil)) {
		os.Remove(tmp)
		return StatusFailed, "the written copy does not match what was read; removed"
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return StatusFailed, err.Error()
	}
	if err := os.Chtimes(dst, after.ModTime(), after.ModTime()); err != nil {
		return StatusFailed, "copied, but the source mtime could not be kept: " + err.Error()
	}
	return StatusCopy, ""
}

func readProbe(p string) (int, error) {
	f, err := openSource(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var b [1]byte
	n, err := f.Read(b[:])
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}
