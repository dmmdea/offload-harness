//go:build !windows

package update

import (
	"fmt"
	"os"
	"path/filepath"
)

func readLink(path string) (string, bool) {
	t, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	return t, true
}

func makeLink(link, target string) error { return os.Symlink(filepath.Clean(target), link) }

// flipLink points link at target atomically: a new symlink is made as <link>.tmp and renamed over the old one.
func flipLink(link, target string) error {
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := makeLink(tmp, target); err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s over %s: %w", tmp, link, err)
	}
	return nil
}
