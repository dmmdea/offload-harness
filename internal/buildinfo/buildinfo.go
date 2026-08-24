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
)

// Version is the single authoritative compiled-in version. main.go's
// `version` const aliases it (keeping the VERSION-file agreement test in
// main_test.go binding), and fleet/pipeline stampers read it directly so no
// plumbing carries it through config.
const Version = "0.90.0"

var (
	buildOnce sync.Once
	buildSHA  string
)

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
		exe, err := os.Executable()
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
