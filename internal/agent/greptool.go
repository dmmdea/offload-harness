package agent

// greptool.go adds search_files: a ripgrep-style code search over the worktree,
// os.Root-confined. It exists because the most budget-hostile pattern on a small
// local model is reading whole files to find one line; search_files lets the
// model FIND code by matching lines instead, returning only the hits.
//
// Backend: ripgrep (rg) when it's on PATH (fast, .gitignore-aware, bounded by
// --max-count); otherwise a confined Go regexp walk rooted at the scope. Either
// way the grouping + hard 100-match cap are applied in Go so the output is
// byte-identical regardless of backend.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxGrepMatches is the PRIMARY bound on search_files output. The agent loop
// also caps every tool result centrally (contextbudget.Trim), but that is only
// a backstop — this cap keeps a broad pattern from ever producing a wall of
// hits that drowns the small model's context.
const maxGrepMatches = 100

const grepMoreMarker = "(more matches available — narrow the pattern)"

// grepBytesCap bounds how much of any single file the Go fallback scans, mirroring
// the read_file budget so one huge file can't stall the walk.
const grepBytesCap = maxReadBytes

// grepMatch is one hit: a workspace-relative file path, a 1-based line number,
// and the (trimmed) matched line text.
type grepMatch struct {
	path string
	line int
	text string
}

// SearchTool builds the search_files tool, scoped to root (the worktree). It is
// read-only and registered alongside list_dir / read_file.
func SearchTool(root string) (Tool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Tool{}, err
	}
	s := &scope{root: absRoot}
	return Tool{
		ToolSpec: ToolSpec{
			Name: "search_files",
			Description: "Search the workspace for lines matching a regular expression (like ripgrep/grep). " +
				"Use this to FIND code by matching lines instead of reading whole files. " +
				"Output is grouped per file with line numbers and is hard-capped at 100 matches. " +
				"pattern is a regular expression (required). path optionally restricts to a subdirectory (relative to the root). " +
				"glob optionally restricts by filename (e.g. \"*.go\"). mode is \"content\" (default, show matching lines) or \"files\" (show only file paths that contain a match).",
			Schema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"regular expression to search for"},"path":{"type":"string","description":"optional subdirectory (relative to the workspace root) to search within"},"glob":{"type":"string","description":"optional filename glob filter, e.g. \"*.go\""},"mode":{"type":"string","enum":["content","files"],"description":"\"content\" (default) shows matching lines; \"files\" shows only file paths with a match"}},"required":["pattern"]}`),
		},
		Exec: s.searchFiles,
	}, nil
}

func (s *scope) searchFiles(ctx context.Context, args string) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Mode    string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	if strings.TrimSpace(in.Pattern) == "" {
		return "", fmt.Errorf("search_files requires a pattern")
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression: %w", err)
	}

	// Confine the search directory through the scope's os.Root handle. This
	// rejects `..`, absolute/volume-qualified paths, and reparse-point escapes
	// (symlinks / Windows junctions) fail-closed — identical containment to
	// read_file / list_dir. We resolve the absolute base dir once and hand it to
	// whichever backend runs.
	r, rel, err := s.open(in.Path)
	if err != nil {
		return "", err
	}
	dir, err := r.OpenRoot(rel) // fails closed on any escape
	r.Close()
	if err != nil {
		return "", err
	}
	dir.Close()
	// rel is validated relative to s.root; the confined absolute base is safe to
	// build now that os.Root proved it resolves inside the root.
	baseAbs := s.root
	if rel != "." {
		baseAbs = filepath.Join(s.root, rel)
	}

	matches, capped, err := grepBackend(ctx, baseAbs, re, in.Glob)
	if err != nil {
		return "", err
	}
	return renderGrep(matches, capped, in.Pattern, in.Mode), nil
}

// grepBackend runs ripgrep when it is on PATH, else the confined Go walk. Both
// return matches already capped at maxGrepMatches (capped=true when more existed).
func grepBackend(ctx context.Context, baseAbs string, re *regexp.Regexp, glob string) ([]grepMatch, bool, error) {
	if rgPath, err := exec.LookPath("rg"); err == nil {
		if ms, capped, ok := grepRipgrep(ctx, rgPath, baseAbs, re, glob); ok {
			return ms, capped, nil
		}
		// rg failed for a non-match reason (e.g. exec error); fall through to Go.
	}
	return grepGoWalk(baseAbs, re, glob)
}

// grepRipgrep shells out to rg. ok=false means "couldn't use rg, fall back".
// We pass the ORIGINAL regex source to rg and re-cap in Go so output is
// identical to the Go path. --max-count per file plus an overall slice cap keep
// it bounded. rg respects .gitignore by default.
func grepRipgrep(ctx context.Context, rgPath, baseAbs string, re *regexp.Regexp, glob string) ([]grepMatch, bool, bool) {
	rgArgs := []string{
		"--line-number",
		"--no-heading",
		"--with-filename",
		"--color", "never",
		"--max-count", strconv.Itoa(maxGrepMatches + 1), // per-file bound; global cap applied below
	}
	if glob != "" {
		rgArgs = append(rgArgs, "--glob", glob)
	}
	rgArgs = append(rgArgs, "--regexp", re.String(), baseAbs)

	cmd := exec.CommandContext(ctx, rgPath, rgArgs...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	// rg exit codes: 0 = matches, 1 = no matches (clean), 2 = error.
	if err != nil {
		if ee, isExit := err.(*exec.ExitError); isExit {
			if ee.ExitCode() == 1 {
				return nil, false, true // clean no-match
			}
		}
		return nil, false, false // real error → fall back to Go walk
	}
	matches, capped := parseRgOutput(out.String(), baseAbs)
	return matches, capped, true
}

// parseRgOutput turns rg's "path:line:text" lines into grepMatch values with
// workspace-relative paths, applying the global 100-match cap.
func parseRgOutput(stdout, baseAbs string) ([]grepMatch, bool) {
	var matches []grepMatch
	capped := false
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), grepBytesCap)
	for sc.Scan() {
		line := sc.Text()
		m, ok := parseRgLine(line, baseAbs)
		if !ok {
			continue
		}
		if len(matches) >= maxGrepMatches {
			capped = true
			break
		}
		matches = append(matches, m)
	}
	return matches, capped
}

// parseRgLine splits one rg output line "path:line:text". The path may itself
// contain colons (Windows "C:\..."), so we find the ":line:" boundary by
// scanning for the first colon that is followed by digits then another colon.
func parseRgLine(line, baseAbs string) (grepMatch, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j == i+1 || j >= len(line) || line[j] != ':' {
			continue // no digits, or not the "path:NUM:text" shape
		}
		n, err := strconv.Atoi(line[i+1 : j])
		if err != nil {
			continue
		}
		path := line[:i]
		text := line[j+1:]
		return grepMatch{path: relPath(baseAbs, path), line: n, text: strings.TrimRight(text, "\r")}, true
	}
	return grepMatch{}, false
}

// grepGoWalk is the fallback: a confined regexp walk under baseAbs. It skips the
// .git directory and applies a minimal root-.gitignore filter, mirroring the
// spirit of rg's defaults. Output (relative paths, 100-cap) matches the rg path.
func grepGoWalk(baseAbs string, re *regexp.Regexp, glob string) ([]grepMatch, bool, error) {
	ignore := loadGitignore(baseAbs)
	var matches []grepMatch
	capped := false

	walkErr := filepath.WalkDir(baseAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the whole search
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" {
				return fs.SkipDir
			}
			if p != baseAbs && ignore.match(name, true) {
				return fs.SkipDir
			}
			return nil
		}
		if capped {
			return fs.SkipAll
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, name); !ok {
				return nil
			}
		}
		if ignore.match(name, false) {
			return nil
		}
		fileMatches, hitCap := grepFile(p, baseAbs, re, len(matches))
		matches = append(matches, fileMatches...)
		if hitCap || len(matches) >= maxGrepMatches {
			capped = true
			if len(matches) > maxGrepMatches {
				matches = matches[:maxGrepMatches]
			}
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	return matches, capped, nil
}

// grepFile scans one file line-by-line for the regex, bounded by grepBytesCap.
// already is how many global matches exist so far; hitCap reports whether adding
// this file's matches reaches the global cap.
func grepFile(p, baseAbs string, re *regexp.Regexp, already int) ([]grepMatch, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// Skip obvious binary files: peek a chunk and bail on a NUL byte.
	head := make([]byte, 512)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, false
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, false
	}

	rel := relPath(baseAbs, p)
	var out []grepMatch
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), grepBytesCap)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if re.MatchString(line) {
			out = append(out, grepMatch{path: rel, line: lineNo, text: strings.TrimRight(line, "\r")})
			if already+len(out) >= maxGrepMatches {
				return out, true
			}
		}
	}
	return out, false
}

// relPath returns p relative to baseAbs with forward slashes; on failure it
// returns the base name so a path is always shown.
func relPath(baseAbs, p string) string {
	rel, err := filepath.Rel(baseAbs, p)
	if err != nil {
		return filepath.ToSlash(filepath.Base(p))
	}
	return filepath.ToSlash(rel)
}

// renderGrep groups matches per file and formats them. No-match returns a clean
// string (never an error). When capped, the more-matches marker is appended.
func renderGrep(matches []grepMatch, capped bool, pattern, mode string) string {
	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q", pattern)
	}

	// Stable ordering: by path, then line — independent of backend/walk order.
	byFile := map[string][]grepMatch{}
	var files []string
	for _, m := range matches {
		if _, seen := byFile[m.path]; !seen {
			files = append(files, m.path)
		}
		byFile[m.path] = append(byFile[m.path], m)
	}
	sort.Strings(files)

	var b strings.Builder
	filesOnly := mode == "files"
	for _, fp := range files {
		ms := byFile[fp]
		sort.Slice(ms, func(i, j int) bool { return ms[i].line < ms[j].line })
		if filesOnly {
			b.WriteString(fp)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(fp)
		b.WriteString(":\n")
		for _, m := range ms {
			b.WriteString(fmt.Sprintf("  L%d: %s\n", m.line, strings.TrimRight(m.text, "\n")))
		}
	}
	if capped {
		b.WriteString(grepMoreMarker)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// gitignoreMatcher is a deliberately minimal .gitignore filter for the Go
// fallback: it handles the common literal-name and "*.ext" / "*substr" patterns
// from the base .gitignore. The primary backend (rg) does full gitignore
// semantics natively; this keeps the fallback reasonable without a new dep.
type gitignoreMatcher struct {
	names    map[string]struct{} // exact base-name matches (dirs or files)
	patterns []string            // glob patterns applied to the base name
}

func (g *gitignoreMatcher) match(name string, isDir bool) bool {
	if g == nil {
		return false
	}
	if _, ok := g.names[name]; ok {
		return true
	}
	for _, pat := range g.patterns {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

func loadGitignore(baseAbs string) *gitignoreMatcher {
	g := &gitignoreMatcher{names: map[string]struct{}{}}
	data, err := os.ReadFile(filepath.Join(baseAbs, ".gitignore"))
	if err != nil {
		return g
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		line = strings.TrimSuffix(line, "/") // dir marker → treat as a name
		// Only apply patterns that target a base name (no path separators);
		// path-anchored patterns are out of scope for the minimal fallback.
		if strings.ContainsAny(line, "/\\") {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			g.patterns = append(g.patterns, line)
		} else {
			g.names[line] = struct{}{}
		}
	}
	return g
}
