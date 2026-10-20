// Package buildinfo carries the compiled-in version and the running binary's
// self-hash.
//
// Both exist for experiment admission rule A1 (config pinning, Tier 2 of the
// 2026-08-21 Phase 2 re-aim): a paired cross-seat measurement is only
// pairable when BOTH sides of the serving stack are pinned. The seat's
// llama-server state is pinned by internal/agent.ProbeSeatPin (server side);
// the harness's own request construction — per-call temperature, the
// re-pack's chat_template_kwargs enable_thinking:false, profile toolsets —
// is CODE, so it is pinned by naming the exact binary that ran. A version
// string alone cannot do that: two checkouts can both say "0.81.0" while one
// carries uncommitted changes (observed on the fleet the day this was
// written), and the enable_thinking defect that invalidated the 2026-08-17
// corpus was exactly a request-side code change a server-side hash would
// never have seen.
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// Version is the single authoritative compiled-in version. main.go's
// `version` const aliases it (keeping the VERSION-file agreement test in
// main_test.go binding), and fleet/pipeline stampers read it directly so no
// plumbing carries it through config.
const Version = "0.196.0"

var (
	buildOnce sync.Once
	buildSHA  string
	// buildImage is what the one hash read saw of the image: where it was, its stat, and the hash. It is written inside
	// buildOnce and read after it, so CaptureStartup can report the file the hash is OF.
	buildImage image

	// executable is the seam for the path of the running image: os.Executable in production, a fixture file in a
	// test (what os.Executable answers for a process whose image was renamed differs by OS, so the test chooses it).
	executable = os.Executable

	// openImage is the seam the executable is opened through for the self-hash: os.Open in production, a refusal in a
	// test (an antivirus lock on the file at start-up is the case that matters, and a test cannot make the OS lock one).
	openImage = os.Open

	startupMu sync.Mutex
	startup   *Startup
)

// image is one read of an executable: the path it was opened by, the stat of the handle it was hashed through (so
// the stat and the hash are of the same file even if a deploy renames another file into the path meanwhile), and
// the hash ("" = could not be read).
type image struct {
	path string
	size int64
	mod  time.Time
	sha  string
}

// Startup is the identity of the image a process started from: the version compiled into it, the SHA-256 of the
// executable as it was when the process began, and where that executable was (its stat, so a later reader can tell
// whether the file at the path is still the one the process started from without hashing it again).
type Startup struct {
	Version string
	// SHA256 is "" when the self-hash failed (see BuildSHA256: absent means unknown, never a value).
	SHA256  string
	Path    string
	Size    int64
	ModTime time.Time
}

// BuildSHA256 returns the hex SHA-256 of the running executable, computed
// once per process. It returns "" when the executable cannot be resolved or
// read — the caller's omitempty then leaves the field ABSENT, which readers
// must treat as "unknown, refuse to pair", never as a value. (Absence ≠
// provably-unpinned: a failed self-read is a could-not-determine, and
// inventing a sentinel hash for it would let two failures pair with each
// other.)
//
// The hash is taken at the first call, so a process that wants the hash of the image it STARTED from calls
// CaptureStartup at start: a long-lived process whose first call came after a rename-swap deploy used to hash the
// NEW file at the path os.Executable names and stamp its rows with a build it was not running (D-36). A short-lived
// CLI process never captures anything and keeps this lazy path, so a hook invocation does not hash the whole
// executable.
func BuildSHA256() string {
	buildOnce.Do(func() {
		buildImage = hashSelf()
		buildSHA = buildImage.sha
	})
	return buildSHA
}

// CaptureStartup records the identity of the image this process started from, once: the first call hashes the
// executable (the same one latch BuildSHA256 uses, taken now instead of at first use) and every later call returns
// that first answer. The MCP server calls it as it starts; the hash is the one every row the process writes carries.
func CaptureStartup() Startup {
	startupMu.Lock()
	defer startupMu.Unlock()
	if startup != nil {
		return *startup
	}
	BuildSHA256() // takes the latch now
	st := Startup{Version: Version, SHA256: buildSHA, Path: buildImage.path, Size: buildImage.size, ModTime: buildImage.mod}
	startup = &st
	return st
}

// StartupIdentity returns the identity CaptureStartup recorded, and false in a process that never captured one.
func StartupIdentity() (Startup, bool) {
	startupMu.Lock()
	defer startupMu.Unlock()
	if startup == nil {
		return Startup{}, false
	}
	return *startup, true
}

// statPath is the size and modification time of the file at path, or zero values when it cannot be stat'd: a capture
// that holds no stamp says so to its reader (the stale-binary guard goes inactive and logs why).
func statPath(path string) (int64, time.Time) {
	fi, err := os.Stat(path)
	if err != nil {
		log.Printf("buildinfo: stat of the executable at its path failed (%v); this process's start-up capture holds no stamp of it", err)
		return 0, time.Time{}
	}
	return fi.Size(), fi.ModTime()
}

// hashSelf reads the running executable once. Failures are LOUD-once (the delegate corpus-loss posture): the caller
// latches whatever happens here for the process lifetime, so a transient failure — an AV lock on the exe at startup,
// a slow mount — silently disables build-hash pinning on EVERY row a long-lived process ever writes. Absent-with-a-
// logged-why is honest; absent indistinguishable from "pre-0.81 binary" is a lost diagnostic.
func hashSelf() image {
	var img image
	fail := func(stage string, err error) {
		log.Printf("buildinfo: executable self-hash failed (%s: %v); harness_build_sha256 will be ABSENT on every row this process writes — restart to retry", stage, err)
	}
	exe, err := executable()
	if err != nil {
		fail("resolve", err)
		return img
	}
	img.path = exe
	f, err := openImage(exe)
	if err != nil {
		fail("open", err)
		// The hash is lost, the stamp is not: the stale-binary guard seeds from this capture, and a capture with a path and no
		// stamp would leave it to read the file later, after the start-up window this capture exists to close.
		img.size, img.mod = statPath(exe)
		return img
	}
	defer f.Close()
	if fi, serr := f.Stat(); serr == nil {
		img.size, img.mod = fi.Size(), fi.ModTime()
	} else {
		// Not fail(): its text says the hash will be absent, and the hash goes on. What is lost is the stamp.
		log.Printf("buildinfo: stat of the executable through its open handle failed (%v); the stamp is read from the path instead", serr)
		img.size, img.mod = statPath(exe)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fail("read", err)
		return img
	}
	img.sha = hex.EncodeToString(h.Sum(nil))
	return img
}
