package pipeline

import (
	"fmt"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// CT-54: a bound media route whose weight is missing defers TYPED, before the lease, on the local path.
//
// A fleet node already refuses such a job at its door (routeNotReadyError, answered 503): mediacap derives the route as
// BOUND-BUT-MISSING and the node drops the task. The local lanes never asked. A local `generate-video` on a box whose
// fastwan sdcpp_model pointed at a missing file took the media lease, spawned the runner, and deferred err_class "other"
// ("--model not found") 31 ms later (0.173.0 live gates, 2026-10-08): fail-closed, but untyped, and after a lease that
// other callers had waited behind. The five engine lanes (sdcpp video, sdcpp animate, audio.cpp voice and music, the sdcpp
// image lane) now read the same derivation before they take the lease and refuse with errClassRouteNotReady.

// errClassRouteNotReady is the err_class of a lane whose route mediacap derives as BOUND-BUT-MISSING: the config binds it,
// and a file it names is not there. It is not a fault of the request and not a seat's ill health (no breaker counts it:
// breakerFailure judges the llama tiers, and a media lane never reaches it); another node may well serve the job.
const errClassRouteNotReady = "route_not_ready"

// mediaRoutes derives the media routes of a config against the real filesystem. A var so a test can inject a derivation;
// production is mediacap.Routes, the one the node door, `doctor` and `offload_status` read.
var mediaRoutes = mediacap.Routes

// routeNotReady returns the derived route named `route` for cfg when its verdict is BOUND-BUT-MISSING. A route that is
// CONFIGURED, one that is NOT CONFIGURED (never bound: the lane's own "is not configured" defer keeps speaking for it) and one
// mediacap does not list all return false.
func routeNotReady(cfg config.Config, route string) (mediacap.Route, bool) {
	for _, r := range mediaRoutes(cfg) {
		if r.Name == route && r.State == mediacap.BoundButMissing {
			return r, true
		}
	}
	return mediacap.Route{}, false
}

// routeReadyOrDefer is the pre-lease gate of an engine lane. It returns (result, true) when the lane's route is
// BOUND-BUT-MISSING: a typed, non-retryable defer whose reason names the route, the config key and the missing path (mediacap's
// own Detail) and says another node may serve it. Nothing has been leased, spawned or created when it answers.
func (p *Pipeline) routeReadyOrDefer(req core.Request, meta core.Meta, start time.Time, cfg config.Config, route, what string) (core.Result, bool) {
	r, bad := routeNotReady(cfg, route)
	if !bad {
		return core.Result{}, false
	}
	err := fmt.Errorf("route %s is %s: %s (this node cannot run it now: restore the file, or place the job on another node)", route, r.State, r.Detail)
	return p.deferRefused(req, meta, start, errClassRouteNotReady, what, err), true
}

// videoRouteName is the mediacap route a sdcpp video family is derived under: the default family is "generate_video", any other
// family is "generate_video:<family>" (mediacap.igpuRoutes).
func videoRouteName(cfg config.Config, family string) string {
	if def, ok := cfg.DefaultVideoSdcppFamily(); ok && def == family {
		return "generate_video"
	}
	return mediacap.VideoFamilyRoute(family)
}
