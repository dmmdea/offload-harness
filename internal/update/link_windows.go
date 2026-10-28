//go:build windows

package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// readLink returns where a junction or symlink at path points; ok is false for a real directory, a file or nothing.
func readLink(path string) (string, bool) {
	t, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	return strings.TrimPrefix(t, `\??\`), true
}

// makeLink creates a junction (no privilege needed) at link pointing at target.
func makeLink(link, target string) error {
	link, target = filepath.Clean(link), filepath.Clean(target)
	cmd := winexec.Command("cmd.exe")
	if cmd.SysProcAttr == nil {
		return fmt.Errorf("cannot build the mklink command line")
	}
	cmd.Args = []string{"cmd.exe"}
	cmd.SysProcAttr.CmdLine = fmt.Sprintf(`cmd.exe /d /s /c "mklink /J "%s" "%s""`, link, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J %s -> %s: %v (%s)", link, target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// flipLink points link at target. A junction cannot be replaced in one step, so the old one is removed (the
// directory it names is untouched) and the new one made at once; if that fails the old one is put back.
func flipLink(link, target string) error {
	old, had := readLink(link)
	if had {
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("removing the old junction %s: %w", link, err)
		}
	}
	if err := makeLink(link, target); err != nil {
		if had {
			_ = makeLink(link, old)
		}
		return err
	}
	return nil
}
