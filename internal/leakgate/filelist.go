package leakgate

import (
	"fmt"
	"io/fs"
	"strings"
)

// The -files list restricts a scan to the files a chunk of work owns. One entry
// per line; a line whose first non-blank character is # is a comment.
//
//	path          a tracked file; it must exist, so a stale list is an error and
//	              never a vacuous pass
//	?path         optional: scanned when present, no error when absent (a new
//	              file the chunk adds, or a file a generator may touch)
//	old=>new      a rename the chunk performs: exactly one of the two paths must
//	              exist (old at the base, new in the work tree after the chunk) and
//	              whichever exists is scanned
//
// A path listed twice, in any form, is an error. Paths are repo-relative with /.

// FileSpec is one parsed entry: either Path (Optional or not), or Old and New.
type FileSpec struct {
	Path     string
	Old, New string
	Optional bool
}

func validListPath(p string) bool {
	return p != "." && fs.ValidPath(p) && !strings.Contains(p, "\\")
}

// ParseFileList parses a -files list.
func ParseFileList(text string) ([]FileSpec, error) {
	var specs []FileSpec
	seen := map[string]int{}
	note := func(n int, p string) error {
		if first, dup := seen[p]; dup {
			return fmt.Errorf("line %d: %q is already listed on line %d", n, p, first)
		}
		seen[p] = n
		return nil
	}
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		optional := strings.HasPrefix(line, "?")
		if optional {
			line = strings.TrimSpace(line[1:])
		}
		if strings.Contains(line, "=>") {
			if optional {
				return nil, fmt.Errorf("line %d: a rename cannot be optional", n)
			}
			parts := strings.Split(line, "=>")
			if len(parts) != 2 {
				return nil, fmt.Errorf("line %d: a rename is old=>new", n)
			}
			old, nw := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if !validListPath(old) || !validListPath(nw) {
				return nil, fmt.Errorf("line %d: a rename needs two repo-relative paths with /", n)
			}
			if old == nw {
				return nil, fmt.Errorf("line %d: a rename names the same path twice", n)
			}
			if err := note(n, old); err != nil {
				return nil, err
			}
			if err := note(n, nw); err != nil {
				return nil, err
			}
			specs = append(specs, FileSpec{Old: old, New: nw})
			continue
		}
		if !validListPath(line) {
			return nil, fmt.Errorf("line %d: not a repo-relative path with /", n)
		}
		if err := note(n, line); err != nil {
			return nil, err
		}
		specs = append(specs, FileSpec{Path: line, Optional: optional})
	}
	return specs, nil
}

// ResolveFileList turns a parsed list into the set of tracked paths to scan.
// A path that is listed (not optional) and not tracked is an error, and so is a
// rename with both sides or neither side tracked.
func ResolveFileList(specs []FileSpec, tracked map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	for _, s := range specs {
		if s.Path != "" {
			switch {
			case tracked[s.Path]:
				out[s.Path] = true
			case !s.Optional:
				return nil, fmt.Errorf("listed path is not tracked: %s (a stale list is never a vacuous pass)", s.Path)
			}
			continue
		}
		o, n := tracked[s.Old], tracked[s.New]
		switch {
		case o && n:
			return nil, fmt.Errorf("rename %s=>%s: both paths exist", s.Old, s.New)
		case !o && !n:
			return nil, fmt.Errorf("rename %s=>%s: neither path exists", s.Old, s.New)
		case o:
			out[s.Old] = true
		default:
			out[s.New] = true
		}
	}
	return out, nil
}
