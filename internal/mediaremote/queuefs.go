package mediaremote

// File primitives of the ticket queue (ticket.go): an atomic write, a retried remove, and the directory lock that the
// node claims are taken under.
//
// The retry is for Windows. os.Rename there is MoveFileEx, which must delete the destination's directory entry, and
// that needs FILE_SHARE_DELETE on every handle already open against it; Go's os.Open grants only read and write
// sharing. So a reader that is mid-read of a ticket at the instant its owner renames a new version over it makes the
// rename (or an os.Remove of it) fail with a sharing violation, though nothing is wrong and the reader lets go
// microseconds later (the same reason internal/gpulease/renamesafe.go exists, whose helpers are not exported).

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

// lockWait bounds how long a claim waits for the directory lock, and lockStale is how old a lock must be before it is
// a crashed holder's and is broken. The lock is held for a few file operations (microseconds), so both are generous.
// Chosen, not measured.
var (
	lockWait  = 3 * time.Second
	lockStale = 10 * time.Second
)

var errLockBusy = errors.New("the ticket queue's directory lock is busy")

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

// withLock runs fn holding the queue directory's lock: an O_EXCL file created for the length of fn. A caller that cannot
// get it inside lockWait gets errLockBusy and does not do what it wanted (a claim that cannot be taken is a node left
// for the next scan, never a job sent without it).
func (s *store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.dir, lockName)
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			defer removeFile(path)
			return fn()
		}
		if !os.IsExist(err) && !errors.Is(err, os.ErrPermission) {
			return err
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > lockStale {
			removeFile(path) // a crashed holder's
			continue
		}
		if time.Now().After(deadline) {
			return errLockBusy
		}
		time.Sleep(fsPause)
	}
}
