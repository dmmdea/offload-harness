//go:build !windows

package fleetnode

import (
	"os"
	"path/filepath"
	"testing"
)

// blockRemoval makes os.RemoveAll of jobDir fail: its context dir loses write
// permission, so the doc inside it cannot be unlinked. A process with root's
// rights ignores permissions, so the test is skipped there.
func blockRemoval(t *testing.T, jobDir string) (release func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root from removing files")
	}
	ctx := filepath.Join(jobDir, "context")
	if err := os.Chmod(ctx, 0o555); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chmod(ctx, 0o755) }
}
