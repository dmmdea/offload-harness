package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Lock is <home>/update.lock: one update at a time per node.
type Lock struct{ path string }

// AcquireLock takes the lock; busy is true (with who holds it) when another live run has it. A lock left by a
// process that is gone is taken over.
func AcquireLock(home string) (l *Lock, busy bool, holder string, err error) {
	p := filepath.Join(home, "update.lock")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, false, "", err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return &Lock{path: p}, false, "", nil
		}
		if !os.IsExist(err) {
			return nil, false, "", err
		}
		b, _ := os.ReadFile(p)
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			if pid, perr := strconv.Atoi(fields[0]); perr == nil && pidAlive(pid) {
				return nil, true, strings.TrimSpace(string(b)), nil
			}
		}
		_ = os.Remove(p) // stale: its process is gone
	}
	return nil, true, "unreadable lock", nil
}

// Release removes the lock file.
func (l *Lock) Release() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}
