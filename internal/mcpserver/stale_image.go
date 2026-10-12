package mcpserver

// stale_image.go: offload_status's process block (D-36).
//
// A rename-swap deploy replaces the installed binary while every MCP server already running keeps the image it
// started from, so a session can write rows with old behaviour for days and nothing says so (the register counted 13
// of 18 MCP processes older than the deployed build). The block names the image this process runs - the version and
// the build hash it stamps on its rows - and compares it with the file now at the path it started from:
//
//	stale_image true     the installed bytes differ from the image this process started from
//	stale_image false    they are equal (or the file is the very one the process started from)
//	no stale_image key   the installed file cannot be read: a note says why, and "fresh" is never the answer to a failure
//
// The block is built from the startup identity the Server holds (Run takes it from buildinfo.CaptureStartup), not from
// the process global, so a test injects a fixed identity and the test binary's own hash never reaches a pinned answer.
//
// A status call must not hash a 60 MB executable every time (245 to 335 ms under load, measured for 0.183.0). The
// installed file's stat is compared with the one the process saw at start: unchanged means it is the same file and
// nothing is read; a moved stat is the only reason to hash, and the result is cached per stat.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// holdStartup takes the startup identity Run serves under: the image this process began from, hashed once at start.
func (s *Server) holdStartup() {
	st := buildinfo.CaptureStartup()
	s.startup = &st
}

// hashInstalledImage is the seam the installed file is hashed through: a test counts how often a status call reads
// the whole executable.
var hashInstalledImage = func(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readInstalledVersion is the seam the installed file's build marker is read through (the audit's own reader), so a
// test makes one read fail.
var readInstalledVersion = gpulease.ReadBuildMarkerVersion

// installedImages caches what the last moved installed file hashed and stated, keyed by its stat. Only a read that
// succeeded is kept: a failed one (an antivirus or indexer lock on the freshly renamed file, a sharing violation) is a
// moment, and the file's stat does not change afterwards, so a failure cached against the stat would read "unknown" for
// the life of the process. The hash and the marker are kept apart, so a retry repeats only the read that failed.
type installedImages struct {
	mu      sync.Mutex
	path    string
	size    int64
	mod     time.Time
	shaOK   bool
	sha     string
	verOK   bool
	version string
}

// imageCheck is one comparison of the installed file with the startup identity.
type imageCheck struct {
	stale            *bool
	installedVersion string
	note             string
}

// statusProcess builds the process block, or nil (absent from the full answer, `{}` when asked for by name, as pair
// does) when the Server holds no startup identity.
func (s *Server) statusProcess() any {
	st := s.startup
	if st == nil {
		return nil
	}
	block := map[string]any{"version": st.Version}
	if st.SHA256 != "" {
		block["build_sha256"] = st.SHA256
	}
	chk := s.installed.check(*st)
	if chk.installedVersion != "" {
		block["installed_version"] = chk.installedVersion
	}
	if chk.stale != nil {
		block["stale_image"] = *chk.stale
	}
	note := chk.note
	if st.SHA256 == "" {
		// Said on every path, the stat-equal one included: the self-hash failure was only logged (to an MCP server's
		// stderr), and without this the block and every ledger row of the process just lack the hash with no reason.
		note = joinNotes(note, "this process's self-hash failed at start, so build_sha256 is absent and its ledger rows carry no harness_build_sha256 (restart to retry)")
	}
	if note != "" {
		block["note"] = note
	}
	return block
}

// check compares the installed file at the startup path with the startup identity.
func (c *installedImages) check(st buildinfo.Startup) imageCheck {
	if st.Path == "" {
		return imageCheck{note: "the path this process started from is unknown, so the installed build cannot be compared with it"}
	}
	fi, err := os.Stat(st.Path)
	if err != nil {
		return imageCheck{note: fmt.Sprintf("the installed image %s cannot be read (%v), so whether this process is stale is unknown", st.Path, err)}
	}
	if fi.Size() == st.Size && fi.ModTime().Equal(st.ModTime) {
		// The same file the process started from: nothing was replaced, nothing to read.
		fresh := false
		return imageCheck{stale: &fresh, installedVersion: st.Version}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != st.Path || c.size != fi.Size() || !c.mod.Equal(fi.ModTime()) {
		c.path, c.size, c.mod = st.Path, fi.Size(), fi.ModTime()
		c.shaOK, c.sha, c.verOK, c.version = false, "", false, ""
	}
	var hashErr, markErr error
	if !c.shaOK {
		if sha, err := hashInstalledImage(st.Path); err != nil {
			hashErr = err
		} else {
			c.sha, c.shaOK = sha, true
		}
	}
	if !c.verOK {
		if v, err := readInstalledVersion(st.Path); err != nil {
			markErr = err
		} else {
			c.version, c.verOK = v, true
		}
	}
	out := imageCheck{}
	if c.verOK {
		out.installedVersion = c.version
	}
	switch {
	case hashErr != nil:
		out.note = fmt.Sprintf("the installed image %s cannot be read (%v), so whether this process is stale is unknown", st.Path, hashErr)
	case st.SHA256 == "":
		out.note = "the hash of the image this process started from is unknown (its self-hash failed at start), so it cannot be compared with the installed build"
	default:
		stale := c.sha != st.SHA256
		out.stale = &stale
	}
	if markErr != nil && hashErr == nil {
		out.note = joinNotes(out.note, fmt.Sprintf("the build marker of the installed image %s cannot be read (%v), so installed_version is not given", st.Path, markErr))
	}
	return out
}

// joinNotes joins two notes with "; ", either of which may be empty.
func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}
