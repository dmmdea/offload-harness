package update

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// buildDir matches a path that ends in a build-named llama.cpp directory (the install tree names one per build:
// llama.cpp-b<N>, llamacpp-b<N>, llama-b<N>), with the directory's own suffix (a zip's "-bin-win-..." name).
var buildDir = regexp.MustCompile(`((?:[A-Za-z]:)?(?:[^\s"'=;,]*[\\/])?)(llama\.cpp-b\d+|llamacpp-b\d+|llama-b\d+)[A-Za-z0-9._-]*`)

var ldPath = regexp.MustCompile(`LD_LIBRARY_PATH=[^\s"']*`)

// RepointResult is what a repoint changed.
type RepointResult struct {
	Backup   string
	Replaced int
	LDLines  int
	Diff     []string
}

// Repoint rewrites a hand-kept llama-swap yaml so it names the live path instead of a build directory: it backs
// the file up (<yaml>.bak-<yyyyMMdd-HHmmss>), replaces every occurrence of a build-named directory, and of each
// directory in from (a build kept under a name with no build number, such as /opt/llama-vulkan), with live, and on
// Linux sets any LD_LIBRARY_PATH= in a block that was repointed to the live path's bin directory. A "bin" segment
// right after a replaced directory is dropped when the live build has no bin folder: the publisher's archives are
// flat, a source tree keeps its programs in build/bin. It never re-renders the file. Nothing is written (and no
// backup made) when nothing matches.
func Repoint(yamlPath, live string, from []string, now time.Time) (*RepointResult, error) {
	fromREs, err := fromPatterns(from)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, err
	}
	flat := true
	if st, serr := os.Stat(filepath.Join(live, "bin")); serr == nil && st.IsDir() {
		flat = false
	}
	lines := strings.Split(string(raw), "\n")
	res := &RepointResult{}
	changed := make([]bool, len(lines))
	orig := append([]string(nil), lines...)
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue // a comment keeps its history
		}
		out, n := replaceDirs(ln, buildDir, live, flat, false)
		for _, re := range fromREs {
			var k int
			out, k = replaceDirs(out, re, live, flat, true)
			n += k
		}
		if n > 0 {
			lines[i], changed[i] = out, true
			res.Replaced += n
		}
	}
	if runtime.GOOS != "windows" {
		bin := filepath.Join(live, "bin")
		if st, err := os.Stat(bin); err != nil || !st.IsDir() {
			bin = live
		}
		keyIndent := minIndent(lines)
		for i, ln := range lines {
			// A line the directory pass rewrote already names the live path (bin segment settled) and keeps the rest
			// of its list, such as :${LD_LIBRARY_PATH:-}.
			if changed[i] || !ldPath.MatchString(ln) {
				continue
			}
			lo, hi := blockOf(lines, i, keyIndent)
			any := false
			for j := lo; j < hi; j++ {
				if changed[j] {
					any = true
					break
				}
			}
			if !any {
				continue
			}
			out := ldPath.ReplaceAllString(ln, "LD_LIBRARY_PATH="+filepath.ToSlash(bin))
			if out != ln {
				lines[i], changed[i] = out, true
				res.LDLines++
			}
		}
	}
	if res.Replaced == 0 && res.LDLines == 0 {
		return res, nil
	}
	// The yaml may carry secrets (an HF_TOKEN in env, an --api-key): the backup gets the original's mode, never wider.
	st, _ := os.Stat(yamlPath)
	mode := os.FileMode(0o600)
	if st != nil {
		mode = st.Mode().Perm()
	}
	res.Backup = yamlPath + ".bak-" + now.Format("20060102-150405")
	if err := os.WriteFile(res.Backup, raw, mode); err != nil {
		return nil, fmt.Errorf("backing up %s: %w", yamlPath, err)
	}
	_ = os.Chmod(res.Backup, mode) // WriteFile applies the umask; an existing backup keeps its old mode
	tmp := yamlPath + ".tmp-repoint"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), mode); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, yamlPath); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	for i := range lines {
		if changed[i] {
			res.Diff = append(res.Diff, fmt.Sprintf("line %d:\n  - %s\n  + %s", i+1, strings.TrimSpace(orig[i]), strings.TrimSpace(lines[i])))
		}
	}
	return res, nil
}

// replaceDirs replaces each match of re in ln with live, written with the separators the match used. whole
// (a --from directory) counts a match only on whole path components: the character before it starts a value and
// the one after it ends the directory, so /opt/llama-vulkan never rewrites /opt/llama-vulkan-b9934. flat drops a
// "/bin" segment that follows the directory.
func replaceDirs(ln string, re *regexp.Regexp, live string, flat, whole bool) (string, int) {
	idx := re.FindAllStringIndex(ln, -1)
	if idx == nil {
		return ln, 0
	}
	var b strings.Builder
	last, n := 0, 0
	for _, m := range idx {
		s, e := m[0], m[1]
		if s < last || (whole && (!startsValue(ln, s) || !endsDir(ln, e))) {
			continue
		}
		b.WriteString(ln[last:s])
		if strings.Contains(ln[s:e], `\`) {
			b.WriteString(filepath.FromSlash(live))
		} else {
			b.WriteString(filepath.ToSlash(live))
		}
		if flat && e+4 <= len(ln) && (ln[e:e+4] == "/bin" || ln[e:e+4] == `\bin`) && endsDir(ln, e+4) {
			e += 4
		}
		last = e
		n++
	}
	b.WriteString(ln[last:])
	return b.String(), n
}

func startsValue(ln string, i int) bool {
	return i == 0 || strings.ContainsRune(" \t\"'=;,:([{", rune(ln[i-1]))
}

func endsDir(ln string, i int) bool {
	return i == len(ln) || strings.ContainsRune(`/\"' `+"\t\r;,:)]}", rune(ln[i]))
}

// fromPatterns turns each --from directory into a pattern that matches it written with either separator
// (case-insensitively on Windows, where paths are).
func fromPatterns(from []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, f := range from {
		f = strings.TrimRight(strings.TrimSpace(f), `/\`)
		if f == "" {
			return nil, fmt.Errorf("--from names an empty directory")
		}
		var b strings.Builder
		if runtime.GOOS == "windows" {
			b.WriteString("(?i)")
		}
		for _, r := range f {
			if r == '/' || r == '\\' {
				b.WriteString(`[\\/]`)
				continue
			}
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("--from %s: %w", f, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// minIndent is the indentation of the entries under a top-level key (the smallest positive indent of a real line).
func minIndent(lines []string) int {
	best := 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if n := indentOf(ln); n > 0 && (best == 0 || n < best) {
			best = n
		}
	}
	return best
}

// blockOf is the run of lines [lo, hi) of the entry (a model, a macro) that line i sits in.
func blockOf(lines []string, i, keyIndent int) (int, int) {
	lo := i
	for lo > 0 {
		t := strings.TrimSpace(lines[lo])
		if t != "" && !strings.HasPrefix(t, "#") && indentOf(lines[lo]) <= keyIndent {
			break
		}
		lo--
	}
	hi := i + 1
	for hi < len(lines) {
		t := strings.TrimSpace(lines[hi])
		if t != "" && !strings.HasPrefix(t, "#") && indentOf(lines[hi]) <= keyIndent {
			break
		}
		hi++
	}
	return lo, hi
}
