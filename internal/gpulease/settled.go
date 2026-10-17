package gpulease

// What "not there" means for a file its writer replaces by a rename over the live name.
//
// A lease file is written beside and renamed over the live name (renameReplacing). On Windows a rename that succeeded can leave the
// live name absent for tens of milliseconds, with the staged file still beside it; measured 11 to 75 ms, one 119 ms, about once in 74,000
// renames on one host. A reader that meets that gets ENOENT, and ENOENT then means two things: the file is not there (a lease that was
// released, a holder that never renewed) or it is being installed. Only the first is an answer. A caller that acts on the absence of
// the file (the reclaim rule, on the heartbeat) must tell them apart, and this is the one place that does.
//
// An absence is SETTLED when no fresh staged file stands beside the name and a look at the name itself, taken after the look at the
// staged file, still finds nothing. The second look matters: the rename can end between the first look at the name and the look at the
// staged file, which then reads as gone while the name is back.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagingFresh is how young a staged file must be to count as a write that may still be installing: the longest a writer keeps
// retrying its rename (renameAttempts x renamePause, 2 s, renamesafe.go) plus a second, because a retry chain was seen to take 2.29 s.
// An older staged file is a leftover of a writer that died, and the name beside it is simply absent.
const stagingFresh = 3 * time.Second

// errRenameInFlight is what a look at a lease file answers when the live name is absent and a fresh staged file stands beside it: the
// file is being installed, so it is neither there nor gone. It wraps in an fs.PathError that is not ErrNotExist, so isIOFailure is true
// for it and no caller reads it as "the file does not exist".
var errRenameInFlight = errors.New("the live name is absent and a staged replacement is beside it: a rename is in flight")

// stagingPath is the name a writer stages a lease file under before it renames it over path, and "" for a file that is never replaced
// by a rename (a card claim is created once, exclusively, and removed). Every writer of a lease file takes its staged name from here,
// so the reader that asks whether a rename is in flight cannot look for a name the writer does not use.
func stagingPath(path string) string {
	base := filepath.Base(path)
	switch {
	case base == metaFileName:
		return path + ".restamp" // restampWholeLocked: ".tmp" is the epoch counter's and the other writers'
	case strings.HasSuffix(base, ".claim") && filepath.Base(filepath.Dir(path)) == cardsDirName:
		return ""
	}
	return path + ".tmp"
}

// stagedInFlight says whether a staged file stands beside path that its writer may still be installing: it exists and is younger
// than stagingFresh. A negative age (a clock that stepped back, a timestamp from the future) counts as fresh, and so does a staged file
// that cannot be examined for any reason but being absent. None of those is evidence that nothing is in flight.
func stagedInFlight(path string) bool {
	staged := stagingPath(path)
	if staged == "" {
		return false
	}
	fi, err := statFile(staged)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	return time.Since(fi.ModTime()) < stagingFresh
}

// statFile is os.Stat, as a variable so a test can make the rename end between two looks at a name (settled_test.go).
var statFile = os.Stat

// statSettled is os.Stat of a lease file whose absence is an answer only when it is settled (see above). A name that is there is
// returned as os.Stat returns it. A name that is absent with no fresh staged file beside it returns the not-exist error. A name that is
// absent with a fresh staged file beside it returns a *fs.PathError wrapping errRenameInFlight, which is not a not-exist error. Any
// other failure is returned as it is. The test seam of statRecord (UseStatFault) is consulted first, as statRecord does.
func statSettled(path string) (os.FileInfo, error) {
	if statFaultHook != nil {
		if err := statFaultHook(path); err != nil {
			return nil, err
		}
	}
	fi, err := statFile(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return fi, err
	}
	if stagedInFlight(path) {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: errRenameInFlight}
	}
	return statFile(path) // the rename may have ended between the first look and the one at the staged file
}
