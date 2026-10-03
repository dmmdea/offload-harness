package agent

import (
	"fmt"
	"log"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ActRead (register SF-07) is a file READ by an agent tool. Reads are not mutations
// and never go through Policy.Decide; they pass the read gate below, which every tool
// that reads file content consults: read_file, summarize_file, search_files,
// edit_file (it reads the old content) and github_upload_file. A rule table may name
// the kind (tighten-only, like every other kind).
//
// Not covered, and said so in the docs: the run/shell cage (an allowlisted python,
// node or git can read any file it can reach, and on native Windows reads are not
// confined), hard links (a second name for the same file content), and the media
// tools that read an absolute image path.
const ActRead ActionKind = "read"

// Warn is the audit-trail decision of a read the floor would deny under enforce but
// lets through under warn. It is a record, never a verdict a tool acts on.
const Warn Decision = "warn"

// readFloorGlobs is the built-in read floor: secret-material file names no agent run
// has any business reading, matched against the basename after the write floor's
// folding (normalizeSubject: case, trailing dots and spaces).
var readFloorGlobs = []string{".env*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
	".npmrc", ".netrc", ".pypirc", ".git-credentials", ".claude.json"}

// readFloorDirs are directory names whose whole contents are secret material.
var readFloorDirs = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".env": true}

// readFloorExempt are the env templates that hold no secrets by convention.
var readFloorExempt = map[string]bool{".env.example": true, ".env.sample": true}

// shortNameSeg is a Windows 8.3 alias segment ("ENV~1", "SSH~1.PRO"): another name
// for a long-named file, which a lexical check cannot see through.
var shortNameSeg = regexp.MustCompile(`~[0-9]`)

// readFloorHit reports whether a root-relative path is secret material under the
// built-in read floor, and which glob caught it. It is purely lexical; readGate
// also checks the path's resolved form.
func readFloorHit(rel string) (string, bool) {
	segs := strings.Split(normalizeSubject(ActRead, rel), "/")
	for _, s := range segs[:len(segs)-1] {
		if readFloorDirs[s] {
			return s + "/", true
		}
	}
	base := segs[len(segs)-1]
	if readFloorDirs[base] && base != ".env" {
		return base + "/", true // the directory itself (a search rooted there)
	}
	if readFloorExempt[base] {
		return "", false
	}
	for _, g := range readFloorGlobs {
		if ok, _ := path.Match(g, base); ok {
			return g, true
		}
	}
	return "", false
}

// readGate is the one chokepoint every content-reading tool consults (SF-07).
//
//   - off: nothing is checked or recorded (also what --rules off sets, with a note).
//   - warn (the default): a read the floor or a read rule catches goes through
//     unchanged and leaves a Warn row on the audit trail (when one is attached) and a
//     log line. On an ENFORCING trail (a browse grant, or audit_all_doors=enforce) a
//     read the trail cannot record is refused, as an unrecordable allow is in Decide.
//   - enforce: the read is refused (NotPerformed, the broker's refusal convention)
//     and a Deny row is recorded.
//
// Every path is checked as given AND in its resolved form (symlinks followed, Windows
// 8.3 short names expanded), so a second name for a secret file is still that file.
// The audit log, the strictness and the table's read rules are bound after the policy
// is built (Build), which is why the tools hold a pointer.
type readGate struct {
	mode   string
	audit  *AuditLog
	strict bool // the attached trail is enforcing: a read it cannot record is refused
	rules  []Rule

	logOnce sync.Once
}

// validReadFloor resolves an agent_read_floor value; "" is warn.
func validReadFloor(mode string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "":
		return "warn", nil
	case "off", "warn", "enforce":
		return m, nil
	}
	return "", fmt.Errorf("agent_read_floor: unknown mode %q (valid: off, warn, enforce)", mode)
}

// resolvedRel is rel resolved against root: symlinks followed and Windows short names
// expanded, then made relative to the resolved root. ok is false when the path does
// not resolve (it does not exist), which leaves the lexical check as the only one.
func resolvedRel(root, rel string) (string, bool) {
	real, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	r, err := filepath.Rel(realRoot, real)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(r), true
}

// hit decides whether a read of rel under root is caught, and by what, without
// recording anything.
func (g *readGate) hit(root, rel string) (Rule, string, bool) {
	names := []string{filepath.ToSlash(filepath.Clean(rel))}
	if root != "" {
		if r, ok := resolvedRel(root, rel); ok && r != names[0] {
			names = append(names, r)
		}
	}
	for _, n := range names {
		if glob, ok := readFloorHit(n); ok {
			return Rule{Kind: ActRead, Glob: glob, Decision: Deny, Severity: SevCritical},
				"secret-material path (" + glob + ") — never read by an agent run", true
		}
	}
	for _, n := range names {
		for _, r := range g.rules {
			if r.matches(Action{Kind: ActRead, Path: n}) {
				return r, "[" + string(r.Severity) + "] " + r.Reason, true
			}
		}
	}
	if g.mode == "enforce" && shortNameSeg.MatchString(rel) {
		return Rule{Kind: ActRead, Glob: "~N", Decision: Deny, Severity: SevHigh},
			"a Windows 8.3 short name (" + rel + ") — use the file's long name", true
	}
	return Rule{}, "", false
}

// refuses reports whether enforce would refuse a read of rel, recording nothing: the
// search walk uses it to skip a file BEFORE reading a byte of it.
func (g *readGate) refuses(root, rel string) bool {
	if g == nil || g.mode != "enforce" {
		return false
	}
	_, _, caught := g.hit(root, rel)
	return caught
}

// check is called with the root-relative path a tool is about to read. A nil gate
// checks nothing (tool sets built outside Build).
func (g *readGate) check(root, rel string) error {
	if g == nil || g.mode == "off" {
		return nil
	}
	fired, reason, caught := g.hit(root, rel)
	if !caught {
		return nil
	}
	a := Action{Kind: ActRead, Path: filepath.ToSlash(filepath.Clean(rel)), Exists: true}
	if g.mode == "enforce" {
		if err := g.audit.record(a, Deny, reason, fired); err != nil {
			g.logAuditFailure(err)
		}
		return NotPerformed("read refused: " + reason)
	}
	if err := g.audit.record(a, Warn, "would deny under agent_read_floor=enforce: "+reason, fired); err != nil {
		g.logAuditFailure(err)
		if g.strict {
			return NotPerformed("read refused: the audit trail could not record this read of secret material (" + err.Error() + ") and the trail is enforcing")
		}
	}
	log.Printf("agent read floor (warn): read of %s allowed; enforce would refuse it: %s", a.Path, reason)
	return nil
}

func (g *readGate) logAuditFailure(err error) {
	g.logOnce.Do(func() {
		log.Printf("agent read floor: audit write failed (further failures in this run are not logged): %v", err)
	})
}

// policyReadGate is the read floor a tool built from a policy shares (nil when the
// policy has none: tool sets built outside Build check nothing).
func policyReadGate(pol *Policy) *readGate {
	if pol == nil {
		return nil
	}
	return pol.readGate
}
