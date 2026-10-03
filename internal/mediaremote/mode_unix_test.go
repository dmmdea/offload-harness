//go:build unix

package mediaremote

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ---- R2: fetched files land 0644 less the umask, not the temp file's 0600 ---------------------------

func TestFetchedOutputsLandAs0644LessTheUmask(t *testing.T) {
	for _, tc := range []struct {
		umask int
		want  os.FileMode
	}{{0o022, 0o644}, {0o077, 0o600}} {
		prev := syscall.Umask(tc.umask)
		n := startNode(t, nodeOpts{})
		cfg := clientCfg(t, n)
		out := filepath.Join(t.TempDir(), "main.png")
		res := Run(context.Background(), cfg, &recordingRunner{}, graphJob(t, map[string]any{"out": out}), "remote", nil)
		syscall.Umask(prev)
		if !res.OK {
			t.Fatalf("%+v", res)
		}
		paths := []string{out}
		for _, name := range dirNames(t, cfg.MediaDir) {
			paths = append(paths, filepath.Join(cfg.MediaDir, name))
		}
		if len(paths) != 2 {
			t.Fatalf("want out and one secondary, got %v", paths)
		}
		for _, p := range paths {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("umask %04o: %s has mode %04o, want %04o", tc.umask, filepath.Base(p), got, tc.want)
			}
		}
	}
}
