// Package callscope decides WHOSE work a harness process is doing: a business's or the operator's own
// (2026-11-09: a business session's speech-to-text and image work was placed on a personal node).
//
// Why: an operator can run work for a business and his own work from the same box, and some fleet nodes may
// only ever take the second kind (a personal server that must never hold a business's data). The placement
// code cannot tell the two apart from a request; it can tell from the caller. One Claude Code session is one
// MCP server process, and Claude Code exports the session id to every child (CLAUDE_CODE_SESSION_ID) and keeps
// one small JSON file per running session that names its working directory. The directory says whose work it
// is, by the roots the operator lists in config (caller_scope_roots).
//
// The rule is fail closed: a process is "personal" or "business" only when it is positively identified as
// such, and everything else (no session file, an unlisted directory, a directory under roots of both kinds at
// the same depth, a misspelled override) is "unknown". A node that admits only listed scopes never admits
// "unknown" (config.Config.RemoteAdmitsScope), so a gap in the identification can cost a personal session a
// node, but can never send a business's work to a node that must not hold it.
//
// This package imports only the standard library, so config, ledger and pairworkloads can all depend on it.
package callscope

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The scope names. Business and Personal are the only values a policy may list; Unknown is what an
// unidentified caller resolves to, and no policy can admit it.
const (
	Business = "business"
	Personal = "personal"
	Unknown  = "unknown"
)

// EnvScope is the explicit override: an operator or a driver that knows whose work it does names it, and the
// resolver then reads nothing else. Any value other than business or personal resolves to unknown.
const EnvScope = "LOCAL_OFFLOAD_SCOPE"

// Header is the HTTP header an asker sends its scope in (core.ScopeHeader is the same name). It is sent only for
// a known scope, so an absent header means unknown.
const Header = "X-Offload-Scope"

// Scope is one resolution: the scope, where it came from, and the directory it was judged by.
type Scope struct {
	Name    string `json:"scope"`
	Source  string `json:"source"`
	Dir     string `json:"dir,omitempty"`
	Session string `json:"session,omitempty"`
	Why     string `json:"why"`
}

// Known reports whether the scope is one a policy can admit (business or personal).
func (s Scope) Known() bool { return s.Name == Business || s.Name == Personal }

// Inputs is everything Resolve reads, so a test can hand it a fake environment and file system.
type Inputs struct {
	Getenv   func(string) string
	PPID     int
	Home     string              // the user's home directory; the session files live under <Home>/.claude/sessions
	Cwd      string              // this process's working directory
	Roots    map[string][]string // caller_scope_roots: scope name -> directory prefixes
	ReadFile func(string) ([]byte, error)
	Glob     func(string) ([]string, error)
	Sleep    func(time.Duration)
	// Wait is how long a missing session file is waited for (Claude Code may write it a moment after it
	// starts the MCP server). Zero waits not at all.
	Wait time.Duration
}

// sessionFile is the part of a Claude Code session file this package reads.
type sessionFile struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// Resolve decides the scope of the process the inputs describe.
//
// Order: the explicit override (LOCAL_OFFLOAD_SCOPE); else, when the process serves a Claude Code session
// (CLAUDE_CODE_SESSION_ID), that session's working directory, read from its session file (the parent's file
// first, since an MCP server's parent is the Claude Code process, then any file naming the session, since a
// CLI run from the session's shell has a shell for a parent); else this process's own working directory. A
// session whose file cannot be found resolves to unknown and does NOT fall back to the process's directory:
// a shell child of a business session can stand in any directory, so its own directory says nothing about
// whose work it does.
func Resolve(in Inputs) Scope {
	getenv := in.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if v := strings.TrimSpace(getenv(EnvScope)); v != "" {
		switch strings.ToLower(v) {
		case Business, Personal:
			name := strings.ToLower(v)
			return Scope{Name: name, Source: "env " + EnvScope, Why: fmt.Sprintf("%s=%s names it", EnvScope, name)}
		default:
			return Scope{Name: Unknown, Source: "env " + EnvScope, Why: fmt.Sprintf("%s=%q is neither %q nor %q", EnvScope, v, Business, Personal)}
		}
	}
	if session := strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID")); session != "" {
		dir, src, ok := sessionDir(in, session)
		if !ok {
			return Scope{Name: Unknown, Source: "claude session", Session: session,
				Why: fmt.Sprintf("no Claude Code session file under %s names session %s, so its working directory is unknown", sessionsDir(in.Home), short(session))}
		}
		s := classify(dir, in.Roots)
		s.Source, s.Session = src, session
		return s
	}
	if strings.TrimSpace(in.Cwd) == "" {
		return Scope{Name: Unknown, Source: "none", Why: "no Claude Code session and no working directory"}
	}
	s := classify(in.Cwd, in.Roots)
	s.Source = "process cwd"
	return s
}

// sessionDir finds the working directory of the named session: the parent's session file when it names the
// session, else any session file that does. It waits up to in.Wait for a file that is not there yet.
func sessionDir(in Inputs, session string) (dir, source string, ok bool) {
	readFile, glob, sleep := in.ReadFile, in.Glob, in.Sleep
	if readFile == nil {
		readFile = os.ReadFile
	}
	if glob == nil {
		glob = filepath.Glob
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	root := sessionsDir(in.Home)
	if root == "" {
		return "", "", false
	}
	const step = 150 * time.Millisecond
	for waited := time.Duration(0); ; waited += step {
		if in.PPID > 0 {
			if f, ok := readSession(readFile, filepath.Join(root, strconv.Itoa(in.PPID)+".json")); ok && f.SessionID == session && strings.TrimSpace(f.Cwd) != "" {
				return f.Cwd, "claude session file of the parent process", true
			}
		}
		if paths, err := glob(filepath.Join(root, "*.json")); err == nil {
			for _, p := range paths {
				if f, ok := readSession(readFile, p); ok && f.SessionID == session && strings.TrimSpace(f.Cwd) != "" {
					return f.Cwd, "claude session file naming the session", true
				}
			}
		}
		if waited >= in.Wait {
			return "", "", false
		}
		sleep(step)
	}
}

func readSession(readFile func(string) ([]byte, error), path string) (sessionFile, bool) {
	b, err := readFile(path)
	if err != nil {
		return sessionFile{}, false
	}
	var f sessionFile
	if json.Unmarshal(b, &f) != nil {
		return sessionFile{}, false
	}
	return f, true
}

func sessionsDir(home string) string {
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// classify maps a directory to the scope whose root is its longest prefix. A directory under no root, or
// under a business root and a personal root of the same length, is unknown.
func classify(dir string, roots map[string][]string) Scope {
	d := Normalize(dir)
	best, bestLen, tie := "", -1, false
	for _, name := range []string{Business, Personal} {
		for _, root := range roots[name] {
			n := matchLen(d, Normalize(root))
			switch {
			case n < 0:
			case n > bestLen:
				best, bestLen, tie = name, n, false
			case n == bestLen && name != best:
				tie = true
			}
		}
	}
	switch {
	case bestLen < 0:
		return Scope{Name: Unknown, Dir: dir, Why: fmt.Sprintf("%s is under no caller_scope_roots entry", dir)}
	case tie:
		return Scope{Name: Unknown, Dir: dir, Why: fmt.Sprintf("%s is under a business root and a personal root of the same depth", dir)}
	}
	return Scope{Name: best, Dir: dir, Why: fmt.Sprintf("%s is under a %s root", dir, best)}
}

// Normalize is the comparison form of a path: forward slashes, no trailing slash, lower case (the delegating
// boxes run Windows, whose file systems ignore case, and a session file may spell the drive letter either way;
// two roots that differ only in case are not a distinction a root list needs to make).
func Normalize(p string) string {
	p = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(p, `\`, "/")))
	for len(p) > 1 && strings.HasSuffix(p, "/") && !strings.HasSuffix(p, ":/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// matchLen is the length of root when dir is root or lies under it, else -1. A root that starts with "*:" matches
// a directory on any drive letter (the operator's cloud-drive mount moves between letters).
func matchLen(dir, root string) int {
	if root == "" {
		return -1
	}
	if strings.HasPrefix(root, "*:") {
		if len(dir) < 2 || dir[1] != ':' {
			return -1
		}
		dir = "*" + dir[1:]
	}
	if dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/") {
		return len(root)
	}
	return -1
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// process is the scope this process resolved, once, when its config loaded (config.Load).
var process atomic.Pointer[Scope]

// SetProcess records the scope this process resolved. The first call wins: a process serves one caller, and a
// second config load (a CLI verb that loads twice) must not move it.
func SetProcess(s Scope) {
	process.CompareAndSwap(nil, &s)
}

// Process is the scope SetProcess recorded, and false when none was (a config without scope keys never
// resolves one).
func Process() (Scope, bool) {
	if p := process.Load(); p != nil {
		return *p, true
	}
	return Scope{}, false
}

// ResetForTest clears the recorded scope. Tests only.
func ResetForTest() { process.Store(nil) }
