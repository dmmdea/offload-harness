package fleetnode

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// Every fixture in this package binds its media scripts to paths that do not exist ("render/comfy-video.mjs",
// "C:/x/comfy-run-graph.mjs"): they exercise admission, dispatch and the wire, not the disk. Production
// reads the disk through mediacap (media_ready.go), so by default the tests stand in a derivation that
// calls a route CONFIGURED exactly when the config binds it. The tests of the honest advertisement itself
// swap the real derivation back with useRealMediaRoutes.
func init() { mediaRoutesFn = bindingOnlyRoutes }

func bindingOnlyRoutes(cfg config.Config) []mediacap.Route {
	row := func(name, engine string, bound bool) mediacap.Route {
		st := mediacap.NotConfigured
		if bound {
			st = mediacap.Configured
		}
		return mediacap.Route{Name: name, Engine: engine, State: st}
	}
	return []mediacap.Route{
		row("generate_video", "comfyui", cfg.VideoGenScript != ""),
		row("animate_character", "comfyui", cfg.AnimateGenScript != ""),
		row("generate_audio:voice", "chatterbox-tts", cfg.VoiceGenScript != ""),
		row("generate_audio:music", "acestep", cfg.MusicGenScript != ""),
		row("run_graph", "comfyui", cfg.RunGraphScript != ""),
		row("generate_audio:voice:endpoint", "openai-compatible-tts", cfg.TTSEndpoint != ""),
	}
}

// useRealMediaRoutes makes the test read the real disk through mediacap for its duration.
func useRealMediaRoutes(t *testing.T) {
	t.Helper()
	prev := mediaRoutesFn
	mediaRoutesFn = mediacap.Routes
	ResetMediaRoutesCache()
	t.Cleanup(func() {
		mediaRoutesFn = prev
		ResetMediaRoutesCache()
	})
}
