package fleetnode

// Honest media advertisement (ADR 0076). config.Default() ships non-empty render scripts, so a box
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
	"fmt"
	"strings"
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

// mediaCache memoizes the derivation per config. Its seams read the two vars above at call time, so a test
// that swaps either sees the swap.
var mediaCache = mediacap.NewCache(mediaRoutesTTL,
	func() time.Time { return mediaClock() },
	func(cfg config.Config) []mediacap.Route { return mediaRoutesFn(cfg) })

// ResetMediaRoutesCache drops every cached verdict, so the next read goes to the disk again.
func ResetMediaRoutesCache() { mediaCache.Reset() }

// mediaView is ONE request's reading of the route verdicts: the first predicate that needs them takes the
// reading and every later one of the same health request or admission reuses it, so a request asks the
// cache once however many tasks it judges. A node that holds one config for its life hands it a key
// computed once (Server.mediaView), so the hot path never encodes the config; a caller with a bare config
// pays one encoding per request, on the first read. A view belongs to one goroutine.
type mediaView struct {
	cfg     config.Config
	key     mediacap.Key
	keyOK   bool
	haveKey bool
	routes  []mediacap.Route
	read    bool
}

// newMediaView is a view over a bare config: the key is computed on the first read.
func newMediaView(cfg config.Config) *mediaView { return &mediaView{cfg: cfg} }

// get returns the routes, at most mediaRoutesTTL old, reading the cache at most once per view.
func (v *mediaView) get() []mediacap.Route {
	if v.read {
		return v.routes
	}
	if !v.haveKey {
		v.key, v.keyOK = mediacap.KeyOf(v.cfg)
		v.haveKey = true
	}
	if v.keyOK {
		v.routes = mediaCache.RoutesKeyed(v.key, v.cfg)
	} else {
		v.routes = mediaRoutesFn(v.cfg) // cannot key it: never cache what cannot be told apart
	}
	v.read = true
	return v.routes
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

// mediaTaskBound reports whether cfg binds a script or endpoint for taskType at all (the "bound" half of
// BOUND-BUT-MISSING). It is false for a task with no mapped route.
func mediaTaskBound(cfg config.Config, taskType string) bool {
	switch taskType {
	case "video-gen":
		return cfg.VideoGenScript != ""
	case "animate":
		return cfg.AnimateGenScript != ""
	case "audio-gen":
		return cfg.VoiceGenScript != "" || cfg.MusicGenScript != "" || cfg.TTSEndpoint != ""
	case "run-graph":
		return cfg.RunGraphScript != ""
	}
	return false
}

// mediaTaskRouteReady reports whether the mediacap route behind taskType is CONFIGURED right now. A task
// with no mapped route is not judged here.
func mediaTaskRouteReady(v *mediaView, taskType string) bool {
	names := taskMediaRoutes[taskType]
	if len(names) == 0 {
		return true
	}
	for _, r := range v.get() {
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

// routeNotReadyError is the refusal of a media task this node BINDS but whose route mediacap does not read
// as CONFIGURED (a weight or custom node went missing under a running node, or never arrived). It is not a
// malformed request: another node may well serve the job, so admission answers 503, which every delegator
// re-places, instead of the 400 that means "this job is wrong". A task that is not bound at all keeps the
// plain unsupported-task 400.
type routeNotReadyError struct {
	task   string
	routes string // "generate_video BOUND-BUT-MISSING", one entry per derived route of the task
}

func (e *routeNotReadyError) Error() string {
	return fmt.Sprintf("task_type %q is bound on this node but its route is not ready: %s (the route is not CONFIGURED, so this node cannot run it now; place the job on another node, or restore the files the route needs and retry)",
		e.task, e.routes)
}

// mediaRouteNotReady returns the refusal for taskType when the node binds it but the derivation does not
// call its route CONFIGURED, else nil. The media-job door counts as bound-but-not-ready when it is open
// (opted in, tokened) and every media task it carries that the node binds is not ready.
func mediaRouteNotReady(v *mediaView, taskType string) *routeNotReadyError {
	if taskType == MediaJobTask {
		if !v.cfg.MediaInputsAdmissible() {
			return nil
		}
		var parts []string
		for _, t := range mediaJobTasks {
			if t == "image-gen" {
				if v.cfg.ImageGenAdvertisable() {
					return nil // an image job needs no route verdict: the door has something to carry
				}
				continue
			}
			if !mediaTaskBound(v.cfg, t) {
				continue
			}
			if mediaTaskRouteReady(v, t) {
				return nil
			}
			parts = append(parts, t+": "+routeStates(v, t))
		}
		if len(parts) == 0 {
			return nil
		}
		return &routeNotReadyError{task: taskType, routes: strings.Join(parts, "; ")}
	}
	if !mediaTaskBound(v.cfg, taskType) || mediaTaskRouteReady(v, taskType) {
		return nil
	}
	return &routeNotReadyError{task: taskType, routes: routeStates(v, taskType)}
}

// routeStates names the derived verdict of each route behind taskType ("generate_video BOUND-BUT-MISSING").
func routeStates(v *mediaView, taskType string) string {
	var parts []string
	for _, name := range taskMediaRoutes[taskType] {
		state := "not reported"
		for _, r := range v.get() {
			if r.Name == name {
				state = string(r.State)
				break
			}
		}
		parts = append(parts, name+" "+state)
	}
	return strings.Join(parts, ", ")
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
	return mediaRoutesHealthIn(newMediaView(cfg))
}

func mediaRoutesHealthIn(v *mediaView) []MediaRouteHealth {
	var out []MediaRouteHealth
	for _, r := range v.get() {
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
