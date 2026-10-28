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

// staleLockAfter is longer than the longest run (the gate's lease wait, two seat loads and the timed completions
// come to about 70 minutes at the defaults).
const staleLockAfter = 3 * time.Hour

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
			// A lock older than any run can last is stale even when its pid answers: after a crash or a reboot the
			// pid may belong to an unrelated process, and honouring it would report "busy" forever.
			old := false
			if len(fields) > 1 {
				if at, terr := time.Parse(time.RFC3339, fields[1]); terr == nil && time.Since(at) > staleLockAfter {
					old = true
				}
			}
			if pid, perr := strconv.Atoi(fields[0]); perr == nil && pidAlive(pid) && !old {
				return nil, true, strings.TrimSpace(string(b)), nil
			}
		}
		_ = os.Remove(p) // stale: its process is gone, or it is older than staleLockAfter
	}
	return nil, true, "unreadable lock", nil
}

// Release removes the lock file.
func (l *Lock) Release() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}
