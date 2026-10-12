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
const Version = "0.183.0"

var (
	buildOnce sync.Once
	buildSHA  string

	// executable is the seam for the path of the running image: os.Executable in production, a fixture file in a
	// test (what os.Executable answers for a process whose image was renamed differs by OS, so the test chooses it).
	executable = os.Executable
)

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

var (
	startupMu sync.Mutex
	startup   *Startup
)

// CaptureStartup records the startup identity (STUB, red-first).
func CaptureStartup() Startup { return Startup{Version: Version} }

// StartupIdentity returns the identity CaptureStartup recorded (STUB, red-first).
func StartupIdentity() (Startup, bool) { return Startup{}, false }

// BuildSHA256 returns the hex SHA-256 of the running executable, computed
// once per process. It returns "" when the executable cannot be resolved or
// read — the caller's omitempty then leaves the field ABSENT, which readers
// must treat as "unknown, refuse to pair", never as a value. (Absence ≠
// provably-unpinned: a failed self-read is a could-not-determine, and
// inventing a sentinel hash for it would let two failures pair with each
// other.)
func BuildSHA256() string {
	buildOnce.Do(func() {
		// Failures are LOUD-once (the delegate corpus-loss posture): sync.Once
		// latches whatever happens here for the process lifetime, so a
		// transient failure — an AV lock on the exe at startup, a slow mount —
		// silently disables build-hash pinning on EVERY row a long-lived
		// process ever writes. Absent-with-a-logged-why is honest; absent
		// indistinguishable from "pre-0.81 binary" is a lost diagnostic.
		fail := func(stage string, err error) {
			log.Printf("buildinfo: executable self-hash failed (%s: %v); harness_build_sha256 will be ABSENT on every row this process writes — restart to retry", stage, err)
		}
		exe, err := executable()
		if err != nil {
			fail("resolve", err)
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			fail("open", err)
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			fail("read", err)
			return
		}
		buildSHA = hex.EncodeToString(h.Sum(nil))
	})
	return buildSHA
}
