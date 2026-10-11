package gpulease

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The render helpers a command RUNS, not only the ones it names (F8 follow-up, 2026-10-10).
//
// HeartbeatHelperIn answers for a helper that is itself an argument (`node render/comfy-generate.mjs ...`).
// The long renders of this workspace are not launched that way: they run `cmd /c render-both.cmd` or
// `bash render-card-v3.sh`, and the helper's path sits inside the script (`set "R=...\comfy-generate.mjs"`,
// `GEN="...comfy-generate.mjs"`). Declaring a progress contract only for the direct form left exactly
// those leases labelled "expired ... open to a takeover" at the end of their first term (8 of the 9
// orphaned lease-wrapper cards in the 2026-10-09 C02 evidence ran `render-card-v3.sh`). So the wrapper also
// reads the script it was asked to run, and the scripts that script names, a few hundred lines at most,
// and looks for a helper path in them.
//
// What it is NOT: a parser of shell. It reads text, splits it at quotes, `=`, separators and white space,
// and applies the same file-name test the direct form uses. It reads only regular files that the command
// line names (or that a script it read names), never more than scriptReadMax bytes of one, at most
// scriptFilesMax files, scriptDepthMax scripts deep. A script it cannot find or read is "nothing found",
// which is what the direct form says for a command it does not recognise: no contract is declared, and the
// lease behaves as it always did. A script that names a helper but also runs long steps that are not
// render helpers (a CPU bake, a model download) reads stalled during them; the doc says so and a caller
// that knows better passes its own --progress-file.
const (
	scriptReadMax  = 64 << 10
	scriptFilesMax = 8
	scriptDepthMax = 2
)

// scriptExts are the file types read as scripts: a shell, a batch file, PowerShell, Python, and the
// Node/JS drivers that spawn the render helpers.
var scriptExts = map[string]bool{
	".cmd": true, ".bat": true, ".ps1": true, ".sh": true, ".bash": true, ".py": true,
	".mjs": true, ".js": true, ".cjs": true,
}

// inlineFlags are the interpreter flags whose NEXT argument is a command line to run, not a file:
// `cmd /c "..."`, `bash -c "..."`, `pwsh -Command "..."`.
var inlineFlags = map[string]bool{"/c": true, "/k": true, "-c": true, "-lc": true, "-command": true, "-cmd": true}

// HeartbeatHelperBehind finds the render helper that heartbeats which the command runs: named among its
// arguments (HeartbeatHelperIn), named in the inline command line of an interpreter, or named in a script
// the command runs. It returns the helper's file name and where it was found: "" when the helper is an
// argument, "the inline command", or the base name of the script that names it.
func HeartbeatHelperBehind(args []string) (helper, where string, ok bool) {
	if h, found := HeartbeatHelperIn(args); found {
		return h, "", true
	}
	s := &scriptScan{seen: map[string]bool{}}
	for i, a := range args {
		if i > 0 && inlineFlags[strings.ToLower(a)] && i+1 < len(args) {
			toks := scriptTokens(args[i+1])
			if h, found := HeartbeatHelperIn(toks); found {
				return h, "the inline command", true
			}
			// The whole argument first: a script path with a space in it is one token of the command line.
			if h, via, found := s.scripts(append([]string{args[i+1]}, toks...), "", 1); found {
				return h, via, true
			}
			continue
		}
		if h, via, found := s.scripts([]string{a}, "", 1); found {
			return h, via, true
		}
	}
	return "", "", false
}

// scriptScan carries the bounds of one HeartbeatHelperBehind call.
type scriptScan struct {
	seen map[string]bool // absolute paths already read: a script that names itself is read once
	read int
}

// scripts reads, in order, each token that names a script file, looking for a helper in its text and in
// the scripts it names in turn. beside is the directory of the script the tokens came from ("" = the
// command line): a script that names another by its file name alone is found next to it.
func (s *scriptScan) scripts(toks []string, beside string, depth int) (helper, where string, ok bool) {
	for _, tok := range toks {
		path, found := resolveScript(tok, beside)
		if !found {
			continue
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		key := strings.ToLower(path)
		if s.seen[key] || s.read >= scriptFilesMax {
			continue
		}
		s.seen[key] = true
		s.read++
		text, err := readScriptPrefix(path)
		if err != nil {
			continue
		}
		words := scriptTokens(text)
		if h, hit := HeartbeatHelperIn(words); hit {
			return h, filepath.Base(path), true
		}
		if depth < scriptDepthMax {
			if h, via, hit := s.scripts(words, filepath.Dir(path), depth+1); hit {
				return h, via, true
			}
		}
	}
	return "", "", false
}

// resolveScript turns a token into the path of a script file it names: the token with its environment
// references expanded, then, when that is no file, its file name beside the script that named it. A token
// that does not end in a script extension, or names a directory, a device or nothing, is not a script.
func resolveScript(tok, beside string) (string, bool) {
	tok = strings.TrimSpace(strings.Trim(tok, `"'`))
	if tok == "" || !scriptExts[strings.ToLower(filepath.Ext(strings.ReplaceAll(tok, `\`, "/")))] {
		return "", false
	}
	if p := expandPath(tok); p != "" && isRegularFile(p) {
		return p, true
	}
	if beside != "" {
		base := filepath.Base(strings.ReplaceAll(tok, `\`, "/"))
		if p := filepath.Join(beside, base); isRegularFile(p) {
			return p, true
		}
	}
	return "", false
}

// expandPath expands `%NAME%` and `$NAME` / `${NAME}` in a path with the wrapper's own environment. A
// reference to a variable that is not set makes the path unusable (""), never a path with a hole in it.
func expandPath(p string) string {
	for n := 0; n < maxExpansions; n++ {
		i := strings.IndexByte(p, '%')
		if i < 0 {
			break
		}
		j := strings.IndexByte(p[i+1:], '%')
		if j < 0 {
			break
		}
		name := p[i+1 : i+1+j]
		v := os.Getenv(name)
		if name == "" || v == "" {
			return ""
		}
		p = p[:i] + v + p[i+1+j+1:]
	}
	missing := false
	p = os.Expand(p, func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			missing = true
		}
		return v
	})
	if missing {
		return ""
	}
	return p
}

// maxExpansions bounds the `%NAME%` passes of expandPath: a variable whose value holds another reference
// must not loop.
const maxExpansions = 8

func isRegularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// readScriptPrefix reads at most scriptReadMax bytes of a script, with its comment lines dropped (a line
// that only mentions a helper in prose is not a use of it) and the batch-file `%~dp0` (the script's own
// directory) removed so that `%~dp0comfy-generate.mjs` reads as a file name.
func readScriptPrefix(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, scriptReadMax))
	if err != nil {
		return "", err
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(t, "rem ") || t == "rem" || strings.HasPrefix(t, "::") ||
			strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//"):
			continue
		}
		keep = append(keep, line)
	}
	return strings.ReplaceAll(strings.Join(keep, "\n"), "%~dp0", ""), nil
}

// scriptTokens splits text at white space, quotes and the separators of the common shells, so that
// `set "R=C:\x\comfy-generate.mjs"` and `GEN="/x/comfy-generate.mjs"` both yield the path.
func scriptTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\r', '\n', '"', '\'', '`', '=', ';', '&', '|', '(', ')', '<', '>', ',', '[', ']', '{', '}':
			return true
		}
		return false
	})
}
