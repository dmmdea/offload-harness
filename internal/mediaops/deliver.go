package mediaops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// deliver.go: how a media op delivers its output. The same rule as render/atomic-out.mjs and
// render/atomic_out.py, for the engine this package runs itself.
//
// ffmpeg opens the output path it is given (and, with -y, truncates what is there) before it writes a
// byte. On 2026-10-09 a full data drive left 21 zero-byte pictures at the output paths of a render
// batch, and a zero-byte file passes every exists() check; a media op that ran out of disk did the same
// to a clip, and one killed at its timeout left a half-written file that looked finished. So the engine
// is pointed at a hidden staged sibling in the SAME directory (a rename never crosses a volume, which is
// what makes it atomic) with the extension last (ffmpeg picks its muxer from it), and only a run that
// exited 0 and left a non-empty file is renamed over the destination. Any other ending removes the
// staged file: a failed op leaves nothing at the output path and a good file already there is untouched.
// Not fsynced: this is protection against a failed write, not against a power cut.
//
// The other engine of the package never writes a destination: GIMP exports to a private temp raster
// that the PIL worker (render/edit_image.py, which delivers through render/atomic_out.py) then turns
// into the output. TestEveryDirectWriteInMediaopsIsAccountedFor lists every write left in the package.

var stageSeq atomic.Uint64

// The seams a test replaces; production always runs the defaults.
var (
	renameFile = os.Rename
	retryPause = time.Sleep
)

// A rename onto a file can fail for a moment on Windows while an antivirus scanner holds the staged
// file just written (or a viewer holds the destination); a delivery that fails there throws away an op
// that took minutes. A few short, bounded retries; anything else, and anything that outlasts them, is a
// real failure. The schedule is render/atomic-out.mjs's.
var renameBackoff = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

// errNoOutput is what a tool that exited 0 reports when it left nothing, or 0 bytes, at the staged
// path: an empty file at the output path would look finished to anything that checks it exists.
var errNoOutput = errors.New("no output")

// stagedSibling is where an op writes before it is delivered: hidden, beside out, unique per process
// and call, the extension last.
func stagedSibling(out string) string {
	dir, name := filepath.Split(out)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return filepath.Join(dir, fmt.Sprintf(".%s.partial-%d-%d%s", stem, os.Getpid(), stageSeq.Add(1), ext))
}

// discard removes a staged file or directory. It never reports: the failure being returned matters
// more than the litter, and on a full disk the removal is what gives the partial bytes back.
func discard(path string) { _ = os.RemoveAll(path) }

// deliverFile has produce write the complete result at a staged sibling of out, then renames it over
// out. produce's error, an empty result and a failed rename all remove the staged file and leave out as
// it was.
func deliverFile(out string, produce func(staged string) error) error {
	staged := stagedSibling(out)
	if err := produce(staged); err != nil {
		discard(staged)
		return namedOutput(err, staged, out)
	}
	if fi, err := os.Stat(staged); err != nil || fi.IsDir() || fi.Size() == 0 {
		discard(staged)
		return errNoOutput
	}
	if err := renameWithRetry(staged, out); err != nil {
		discard(staged)
		return fmt.Errorf("delivering %s: %w", out, err)
	}
	return nil
}

// deliverFrames is deliverFile for an op that writes a numbered series of files (extract_frames):
// pattern is the destination, `dir/frame_%05d.png`. produce writes the series into a hidden staging
// directory INSIDE the destination directory (the same volume), and only a run that exited 0, produced
// at least one frame and no empty one has its frames moved into place, each by a rename. A failed run
// leaves the destination exactly as it was (the frames of an earlier run included). The moves are
// individually atomic but the set is not: they are renames within one volume, near-instant, and a
// rename that still fails reports how many frames had landed.
func deliverFrames(pattern string, produce func(stagedPattern string) error) error {
	dir, base := filepath.Dir(pattern), filepath.Base(pattern)
	stem := "frames"
	if i := strings.IndexByte(base, '%'); i > 0 {
		stem = strings.Trim(base[:i], "_-. ")
	}
	staging := filepath.Join(dir, fmt.Sprintf(".%s.partial-%d-%d", stem, os.Getpid(), stageSeq.Add(1)))
	if err := os.Mkdir(staging, 0o755); err != nil {
		return err
	}
	defer discard(staging) // whatever is still in it after the moves, on every path out
	if err := produce(filepath.Join(staging, base)); err != nil {
		return namedOutput(err, staging, dir)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, ierr := e.Info()
		if ierr != nil || fi.Size() == 0 {
			return fmt.Errorf("an empty frame (%s) was written: %w", e.Name(), errNoOutput)
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return errNoOutput
	}
	for i, name := range names {
		if err := renameWithRetry(filepath.Join(staging, name), filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("delivering frame %s (%d of %d landed): %w", name, i, len(names), err)
		}
	}
	return nil
}

// renameWithRetry renames over the destination (os.Rename replaces an existing file on both POSIX and
// Windows), retrying a transient failure on the bounded schedule.
func renameWithRetry(from, to string) error {
	for attempt := 0; ; attempt++ {
		err := renameFile(from, to)
		if err == nil {
			return nil
		}
		if attempt >= len(renameBackoff) || !transientRename(err) {
			return err
		}
		retryPause(renameBackoff[attempt])
	}
}

func transientRename(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EBUSY) || platformTransientRename(err)
}

// outputError is a tool's failure with the staged name replaced by the output's in its text: ffmpeg's
// own message names the file it was given, and the staged sibling is an implementation detail.
type outputError struct {
	msg string
	err error
}

func (e *outputError) Error() string { return e.msg }
func (e *outputError) Unwrap() error { return e.err }

func namedOutput(err error, staged, out string) error {
	msg := err.Error()
	named := strings.ReplaceAll(strings.ReplaceAll(msg, staged, out), filepath.Base(staged), filepath.Base(out))
	if named == msg {
		return err
	}
	return &outputError{msg: named, err: err}
}

// samePath reports whether two paths name the same file: by identity when both exist, else by their
// cleaned absolute spelling (case-insensitive on Windows, where the file system is).
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if fa, err := os.Stat(a); err == nil {
		if fb, err := os.Stat(b); err == nil {
			return os.SameFile(fa, fb)
		}
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return pathsEqual(aa, bb)
}
