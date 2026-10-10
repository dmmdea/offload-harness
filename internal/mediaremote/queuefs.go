package mediaremote

// File primitives of the ticket queue (ticket.go): an atomic write, a retried remove, and the directory lock that the
// node claims are taken under.
//
// The retry is for Windows. os.Rename there is MoveFileEx, which must delete the destination's directory entry, and
// that needs FILE_SHARE_DELETE on every handle already open against it; Go's os.Open grants only read and write
// sharing. So a reader that is mid-read of a ticket at the instant its owner renames a new version over it makes the
// rename (or an os.Remove of it) fail with a sharing violation, though nothing is wrong and the reader lets go
// microseconds later (the same reason internal/gpulease/renamesafe.go exists, whose helpers are not exported).
//
// THE DIRECTORY LOCK IS AN OPERATING-SYSTEM FILE LOCK, not a file whose existence is the lock. A lock that is a file made
// with O_EXCL has to be broken when its holder crashes, and every way of breaking it (judge it stale by its age, then
// remove it) has a gap between the judgement and the removal in which another contender can have removed the same stale
// file and made a fresh lock, which the slow contender then removes: two holders at once, and the claim of a node that
// "read the claims, then take one" is no longer one step (measured on the first design: 24 of 300 contended rounds had two
// holders inside). A flock / LockFileEx lock has no stale state to judge: the system frees it when its holder's process
// ends, however it ends, and a holder that is merely slow keeps it. The lock file is therefore NEVER removed: removing it
// would let a late contender open a new file while an earlier one still holds the old, and both would think they hold it.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	fsTries = 100
	fsPause = 5 * time.Millisecond
)

// lockWait bounds how long a claim waits for the directory lock. The lock is held for a few file operations
// (microseconds), so the bound is generous. Chosen, not measured.
var lockWait = 3 * time.Second

var (
	errLockBusy = errors.New("the ticket queue's directory lock is busy")
	// errLockHeld is what tryLockFile answers when another holder has the lock (the caller tries again until lockWait).
	errLockHeld = errors.New("the lock is held")
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())[:n*2]
	}
	return hex.EncodeToString(b)
}

// writeFileAtomic writes b to path through a uniquely named temp file beside it and a rename, so a reader never sees a
// half-written record and two writers never share a temp file.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), randHex(4))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	var err error
	for i := 0; i < fsTries; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(fsPause)
	}
	_ = os.Remove(tmp)
	return err
}

// removeFile deletes path, retrying a sharing violation. A path that is not there is not an error.
func removeFile(path string) {
	for i := 0; i < fsTries; i++ {
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(fsPause)
	}
}

// withLock runs fn holding the queue directory's lock. A caller that cannot get it inside lockWait gets errLockBusy and
// does not do what it wanted (a claim that cannot be taken is a node left for the next scan, never a job sent without it).
func (s *store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(lockWait)
	for {
		err := tryLockFile(f)
		if err == nil {
			break
		}
		if !errors.Is(err, errLockHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return errLockBusy
		}
		time.Sleep(fsPause)
	}
	defer unlockFile(f)
	return fn()
}
