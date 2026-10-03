package fleetnode

// Honest media advertisement (ADR 0072). config.Default() ships non-empty render scripts, so a box
// advertised video-gen, animate and run-graph it had never set up: the script path was bound, the
// weights or custom nodes it loads were not, and the first job failed on the node. A media task is
// advertised, and admitted, only when internal/mediacap — the same derivation offload_status and
// doctor print — reads its route as CONFIGURED. One predicate (taskConfiguredFor) answers both
// questions, so health can never promise what dispatch would refuse.
//
// mediacap reads the disk (script files, model files, custom-node directories), so the answer is cached
// for mediaRoutesTTL per config: health is polled every few seconds by every delegator and admission
// consults it per job. A weight that goes missing therefore stops being advertised within a minute, and
// returns within a minute of being restored.

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// mediaRoutesTTL bounds how stale an advertised media route may be.
const mediaRoutesTTL = 60 * time.Second

// mediaRoutesFn derives the routes. A var so tests can stand in a snapshot; production is mediacap.Routes,
// which is a filesystem read (no network call, no process started).
var mediaRoutesFn = mediacap.Routes

// mediaClock is the cache's clock; tests move it.
var mediaClock = time.Now

type mediaRoutesEntry struct {
	at     time.Time
	routes []mediacap.Route
}

var mediaRoutesCache = struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]mediaRoutesEntry
}{entries: map[[sha256.Size]byte]mediaRoutesEntry{}}

// mediaRoutesMaxEntries bounds the cache: a serving process holds one config, so more than a few
// entries only ever come from tests that build many.
const mediaRoutesMaxEntries = 32

// ResetMediaRoutesCache drops every cached verdict, so the next read goes to the disk again.
func ResetMediaRoutesCache() {
	mediaRoutesCache.mu.Lock()
	mediaRoutesCache.entries = map[[sha256.Size]byte]mediaRoutesEntry{}
	mediaRoutesCache.mu.Unlock()
}

// mediaRoutes returns this config's routes, at most mediaRoutesTTL old. The lock is held across the
// derivation on purpose: concurrent health polls wait for one read instead of each stat-ing the disk.
func mediaRoutes(cfg config.Config) []mediacap.Route {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return mediaRoutesFn(cfg) // cannot key it: never cache what cannot be told apart
	}
	key := sha256.Sum256(raw)
	now := mediaClock()
	mediaRoutesCache.mu.Lock()
	defer mediaRoutesCache.mu.Unlock()
	if e, ok := mediaRoutesCache.entries[key]; ok && now.Sub(e.at) < mediaRoutesTTL && now.Sub(e.at) >= 0 {
		return e.routes
	}
	routes := mediaRoutesFn(cfg)
	if len(mediaRoutesCache.entries) >= mediaRoutesMaxEntries {
		for k, e := range mediaRoutesCache.entries {
			if now.Sub(e.at) >= mediaRoutesTTL {
				delete(mediaRoutesCache.entries, k)
			}
		}
		if len(mediaRoutesCache.entries) >= mediaRoutesMaxEntries {
			mediaRoutesCache.entries = map[[sha256.Size]byte]mediaRoutesEntry{}
		}
	}
	mediaRoutesCache.entries[key] = mediaRoutesEntry{at: now, routes: routes}
	return routes
}

// taskMediaRoutes maps a fleet task to the mediacap routes that can run it: the task is ready when ANY of
// them is CONFIGURED. image-gen is absent on purpose: it keeps config.ImageGenAdvertisable, which also
// covers the sdcpp engine and named families that have routes of their own.
var taskMediaRoutes = map[string][]string{
	"video-gen": {"generate_video"},
	"animate":   {"animate_character"},
	"run-graph": {"run_graph"},
	"audio-gen": {"generate_audio:voice", "generate_audio:voice:endpoint", "generate_audio:music"},
}

// mediaTaskRouteReady reports whether the mediacap route behind taskType is CONFIGURED right now. A task
// with no mapped route is not judged here.
func mediaTaskRouteReady(cfg config.Config, taskType string) bool {
	names := taskMediaRoutes[taskType]
	if len(names) == 0 {
		return true
	}
	for _, r := range mediaRoutes(cfg) {
		if r.State != mediacap.Configured {
			continue
		}
		for _, n := range names {
			if r.Name == n {
				return true
			}
		}
	}
	return false
}

// MediaRouteHealth is one row of /fleet/health's media_routes: a task route this node derives and its
// verdict. Detail is deliberately not published (it holds local paths).
type MediaRouteHealth struct {
	Route  string `json:"route"`
	Engine string `json:"engine"`
	State  string `json:"state"`
}

// MediaRoutesHealth lists the node's task routes (shared prerequisites such as the node runtime and the
// ComfyUI install are left out) from the same cached derivation the admission predicate reads.
func MediaRoutesHealth(cfg config.Config) []MediaRouteHealth {
	var out []MediaRouteHealth
	for _, r := range mediaRoutes(cfg) {
		if r.Prereq {
			continue
		}
		out = append(out, MediaRouteHealth{Route: r.Name, Engine: r.Engine, State: string(r.State)})
	}
	return out
}

// SetMediaRoutesSourceForTest replaces the route derivation and returns the function that restores it.
// It is for tests in OTHER packages that drive the real advertiser (Families, SupportedTasks) with a
// fixture config binding scripts that do not exist on the test machine: such a config is BOUND-BUT-MISSING
// to the real derivation, and the advertisement honestly drops it. Production never calls it.
func SetMediaRoutesSourceForTest(fn func(config.Config) []mediacap.Route) (restore func()) {
	prev := mediaRoutesFn
	mediaRoutesFn = fn
	ResetMediaRoutesCache()
	return func() {
		mediaRoutesFn = prev
		ResetMediaRoutesCache()
	}
}
