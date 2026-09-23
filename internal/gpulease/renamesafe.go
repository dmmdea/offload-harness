package gpulease

import (
	"os"
	"time"
)

// renameAttempts x renamePause bound the retry on the Windows-only ephemeral
// rename failure below. 2s total, the same ceiling epochLockStale treats as
// "wedged, not contention" — a rename that cannot complete inside that window
// is not an ordinary reader race any more.
const (
	renameAttempts = 400
	renamePause    = 5 * time.Millisecond
)

// renameReplacing renames oldpath onto newpath, retrying the transient
// Windows failure a concurrent reader's open handle produces.
//
// WHY THIS EXISTS: os.Rename on Windows is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which must delete the destination's directory
// entry to replace it. Deleting a file requires the FILE_SHARE_DELETE bit on
// every handle already open against it, and Go's os.Open grants only
// FILE_SHARE_READ|FILE_SHARE_WRITE (see syscall.Open on windows) — never
// delete. So a plain os.ReadFile of newpath (Inspect(), readMeta(), `gpu
// status`, any prober) that is mid-flight at the exact instant a writer
// renames over the same path makes the rename fail with ERROR_ACCESS_DENIED
// or ERROR_SHARING_VIOLATION, even though nothing is actually wrong: the
// reader releases its handle microseconds later and an identical rename
// would succeed immediately after.
//
// removeClaim (same package) already retries the analogous case for
// os.Remove; this is the rename-side twin, needed wherever a lease record is
// updated in place (Restamp, the epoch counter, the heartbeat) while another
// goroutine or process is free to read it concurrently — which on a MACHINE-
// WIDE lease is always true: `gpu status`, offload_status, and any other
// process's Inspect() have no coordination with a writer's rename.
//
// On every platform but Windows this is exactly one os.Rename: POSIX
// rename(2) is atomic and is never blocked by a concurrent reader's open file
// descriptor, so isEphemeralRenameError is unconditionally false there and
// Linux/macOS behaviour is unchanged.
//
// Pattern: cmd/internal/robustio in the Go toolchain retries the identical
// Windows errno set around os.Rename/os.ReadFile/os.RemoveAll for the same
// reason (gopls test fixtures racing an indexer). This mirrors that, sized to
// this package's existing retry idiom (removeClaim/createClaim/the epoch
// lock) rather than robustio's jittered backoff, since the window here is a
// plain file read — microseconds, not an antivirus scan.
func renameReplacing(oldpath, newpath string) error {
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		err = os.Rename(oldpath, newpath)
		if err == nil || !isEphemeralRenameError(err) {
			return err
		}
		time.Sleep(renamePause)
	}
	return err
}
