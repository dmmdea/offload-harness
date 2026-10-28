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

// flipLink points link at target. A junction cannot be replaced in one step, so the step that can fail for reasons
// outside this process (mklink: an antivirus block, a path cmd.exe cannot carry) runs first, beside the live link,
// while it is still intact: the new junction is made as <link>.new, the old one is renamed aside to <link>.old, the
// new one renamed into place, and only then is the old one removed. Every step after the first is a rename of a
// junction (the directories they name are never touched), and a failed rename puts the old junction back by rename,
// so a failure never leaves the live path missing.
func flipLink(link, target string) error {
	link = filepath.Clean(link)
	tmp, parked := link+".new", link+".old"
	for _, p := range []string{tmp, parked} {
		if _, isLink := readLink(p); isLink {
			_ = os.Remove(p) // a leftover junction from an interrupted flip (removes the link only)
		} else if exists(p) {
			return fmt.Errorf("%s exists and is not a junction; move it away and retry (the live link is untouched)", p)
		}
	}
	if err := makeLink(tmp, target); err != nil {
		return fmt.Errorf("%w (the live link is untouched)", err)
	}
	_, had := readLink(link)
	if had {
		if err := os.Rename(link, parked); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("moving the old junction %s aside: %w (the live link is untouched)", link, err)
		}
	} else if exists(link) {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s exists and is not a junction (the live link is untouched)", link)
	}
	if err := os.Rename(tmp, link); err != nil {
		if had {
			if rerr := os.Rename(parked, link); rerr != nil {
				return fmt.Errorf("renaming %s into place: %v; putting the old junction back from %s ALSO failed: %v", tmp, err, parked, rerr)
			}
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s into place: %w (the old junction is back)", tmp, err)
	}
	if had {
		_ = os.Remove(parked)
	}
	return nil
}
