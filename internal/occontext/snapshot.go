package occontext

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite: no cgo, same driver the vendored printed CLIs use
)

// Snap is an open, private copy of an opencode db. The source is never opened by SQLite,
// so nothing here can take a lock on it, checkpoint its WAL or write its -shm.
type Snap struct {
	Path string
	DB   *sql.DB
	Info SourceInfo
	dir  string
}

// Close closes the copy and removes its temp dir.
func (s *Snap) Close() {
	if s == nil {
		return
	}
	if s.DB != nil {
		_ = s.DB.Close()
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

// maxCopyAttempts bounds the copy loop against a source that never stops changing.
const maxCopyAttempts = 5

// Snapshot copies src and its -wal and -shm into a new temp dir and opens the copy.
//
// Why copy all three: a live opencode keeps recent rows in the WAL until a checkpoint,
// so the main file alone is missing them. Why the copy is consistent: a torn tail in the
// copied WAL is harmless (SQLite applies only frames up to the last valid commit frame),
// the copied -shm is discarded (the first connection to a WAL db with no other
// connections rebuilds the index from the WAL), and the one real tear — a checkpoint or
// write landing between copying the db and copying its WAL — changes the size or mtime
// of one of them, which the before/after comparison catches and answers with a fresh copy.
// The copy then has to pass PRAGMA quick_check before anything reads it.
func Snapshot(src string) (*Snap, error) { return snapshot(src, nil) }

type fileState struct {
	exists bool
	size   int64
	mod    time.Time
}

func stateOf(p string) (fileState, error) {
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	return fileState{exists: true, size: st.Size(), mod: st.ModTime()}, nil
}

// sourceState is what a writer landing mid-copy changes: the db and its WAL.
func sourceState(src string) ([2]fileState, error) {
	var s [2]fileState
	var err error
	if s[0], err = stateOf(src); err != nil {
		return s, err
	}
	s[1], err = stateOf(src + "-wal")
	return s, err
}

// snapshot is Snapshot with a hook that runs between copying the db and its WAL on each
// attempt, so tests can land a write exactly where a tear would happen.
func snapshot(src string, midCopy func(attempt int)) (*Snap, error) {
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("opencode db: %w", err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("opencode db: %s is a directory", src)
	}
	dir, err := os.MkdirTemp("", "occontext-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Snap, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	base := filepath.Base(src)
	dst := filepath.Join(dir, base)
	var copied []string
	attempt := 0
	for {
		attempt++
		before, err := sourceState(src)
		if err != nil {
			return fail(err)
		}
		copied = copied[:0]
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(dst + suffix) // a previous attempt's files
		}
		if _, err := copyFile(src, dst); err != nil {
			return fail(fmt.Errorf("copy %s: %w", base, err))
		}
		copied = append(copied, base)
		if midCopy != nil {
			midCopy(attempt)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			ok, err := copyFile(src+suffix, dst+suffix)
			if err != nil {
				return fail(fmt.Errorf("copy %s: %w", base+suffix, err))
			}
			if ok {
				copied = append(copied, base+suffix)
			}
		}
		after, err := sourceState(src)
		if err != nil {
			return fail(err)
		}
		if before == after {
			break
		}
		if attempt >= maxCopyAttempts {
			return fail(fmt.Errorf("opencode db kept changing during %d copy attempts; run again when the session is idle", attempt))
		}
	}
	// query_only: the copy is private, but nothing here has any business writing it.
	db, err := sql.Open("sqlite", dst+"?_pragma=query_only(1)")
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		_ = db.Close()
		if err == nil {
			err = fmt.Errorf("quick_check: %s", check)
		}
		return fail(fmt.Errorf("the copy of %s is not a readable SQLite db: %w", base, err))
	}
	return &Snap{
		Path: dst,
		DB:   db,
		dir:  dir,
		Info: SourceInfo{DB: src, Copied: append([]string(nil), copied...), Attempts: attempt, QuickCheckOK: true},
	}, nil
}

// copyFile copies src to dst; a missing src is (false, nil).
func copyFile(src, dst string) (bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, err
	}
	return true, out.Close()
}
