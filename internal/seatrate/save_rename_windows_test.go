//go:build windows

package seatrate

import (
	"syscall"
	"testing"
	"time"
)

// Windows reports a rename blocked by another handle as ERROR_ACCESS_DENIED (5), which Go maps to a
// permission error, but also as ERROR_SHARING_VIOLATION (32) and ERROR_LOCK_VIOLATION (33), which it
// does not. All three clear when the other handle closes.
func TestUpdateRetriesTheSharingAndLockViolationsOfARename(t *testing.T) {
	for name, errno := range map[string]syscall.Errno{
		"sharing violation": 32,
		"lock violation":    33,
	} {
		t.Run(name, func(t *testing.T) {
			path := Path(t.TempDir())
			calls := renameDenials(t, 2, errno)
			err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) })
			if err != nil {
				t.Fatalf("a rename refused with %v must be retried, got %v", errno, err)
			}
			if n := calls.Load(); n != 3 {
				t.Fatalf("the rename was tried %d times, want 3", n)
			}
			s, lerr := Load(path)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if got := s.Get("agent-pool").Samples; got != 1 {
				t.Fatalf("samples = %d, want the one observation stored", got)
			}
		})
	}
}
