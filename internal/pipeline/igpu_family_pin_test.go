package pipeline

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// TestEngineLaneFamiliesMatchTheAdvertisedOnes extends the footprint pin
// (TestVideoFootprintFamilyMatchesTheAdvertisedFamily) to the CT-49 engine lanes: the family an
// sdcpp video render, the sdcpp animate lane and the audio.cpp lanes RECORD in the footprint
// store is the family fleetnode ADVERTISES on /fleet/health, so a dispatcher admitting against the
// advertised family finds the measurements the lane wrote. Every writer-side value comes from the
// pipeline's own helpers (the ones runGenerateVideoSdcpp / runGenerateAudioAudiocpp call), never from
// a literal restated here; the animate leg runs the lane itself and reads back what it recorded.
func TestEngineLaneFamiliesMatchTheAdvertisedOnes(t *testing.T) {
	defer fleetnode.SetMediaRoutesSourceForTest(func(config.Config) []mediacap.Route {
		return []mediacap.Route{
			{Name: "generate_video", Engine: "sdcpp", State: mediacap.Configured},
			{Name: "animate_character", Engine: "sdcpp", State: mediacap.Configured},
			{Name: "generate_audio:voice", Engine: "audiocpp", State: mediacap.Configured},
			{Name: "generate_audio:music", Engine: "audiocpp", State: mediacap.Configured},
		}
	})()

	t.Run("sdcpp video default family", func(t *testing.T) {
		for _, fam := range []string{
			"fastwan", "wan22", "ltx25", "hunyuan", "ace", "h3", // an sdcpp family may carry a ComfyUI family's name
			"LTX25", "wan", "wan2.2", "my-wan", // names outside the runner's closed set
		} {
			cfg := config.Config{VideoGenFamily: fam, VideoGenFamilies: map[string]config.VideoFamilyBinding{
				fam: {Engine: config.EngineSdcpp, SdcppBackend: "vulkan0"}}}
			if !cfg.VideoGenBound() {
				t.Fatalf("%q: fixture does not bind the video lane", fam)
			}
			_, render := resolveVideoFamily(cfg, "") // what a request naming no model resolves to
			writes := videoFootprintFamily(sdcppRenderFamily(cfg, render))
			if adv := fleetnode.Families(cfg); len(adv) != 1 || adv[0] != writes {
				t.Errorf("videogen_family=%q: the sdcpp lane WRITES footprint family %q but fleetnode ADVERTISES %v", fam, writes, adv)
			}
		}
	})

	t.Run("sdcpp video default family named by the default binding alone", func(t *testing.T) {
		// videogen_family unset, the box's flat/default binding is the sdcpp one: the render family
		// arrives empty and the writer asks config.DefaultVideoSdcppFamily, as the advertiser does
		cfg := config.Config{VideoGenFamilies: map[string]config.VideoFamilyBinding{
			"wan22": {Engine: config.EngineSdcpp, SdcppBackend: "vulkan0"}}}
		def, ok := cfg.DefaultVideoSdcppFamily()
		if !ok {
			t.Fatal("no sdcpp default derivable from this fixture")
		}
		writes := videoFootprintFamily(sdcppRenderFamily(cfg, ""))
		if adv := fleetnode.Families(cfg); len(adv) != 1 || adv[0] != writes {
			t.Errorf("default %q: WRITES %q, ADVERTISES %v", def, writes, adv)
		}
	})

	t.Run("sdcpp animate, as the lane records it", func(t *testing.T) {
		// Run the real lane (a stub runner, an injected sampler) and read back the family it recorded the
		// footprint under, which is what runAnimateCharacterSdcpp passes as fpFamily. The advertiser's family is
		// compared with THAT, not with the config constant both sides happen to read today, so a lane that
		// records under another name fails here (release 0.173.0 review, REL5).
		requireNodePipeline(t)
		dir := t.TempDir()
		cfg := animateCfg(t, dir)
		p := footprintTestPipeline(t, cfg, 2.0)
		if res := p.Run(context.Background(), animateReq(dir, nil)); !res.OK {
			t.Fatalf("the sdcpp animate lane deferred: %s", res.Reason)
		}
		var wrote []string
		for _, e := range p.FootprintStore().Entries() {
			if e.TaskType == "animate" {
				wrote = append(wrote, e.ModelFamily)
			}
		}
		// the family a node whose animate lane is the sd.cpp engine advertises (a config that binds only that lane,
		// as the other legs do: Families lists every lane the config binds)
		adv := fleetnode.Families(config.Config{AnimateGenEngine: cfg.AnimateGenEngine})
		if len(wrote) != 1 || len(adv) != 1 || adv[0] != wrote[0] {
			t.Errorf("animate: the lane WROTE footprint family %v but fleetnode ADVERTISES %v", wrote, adv)
		}
	})

	t.Run("audiocpp voice and music", func(t *testing.T) {
		for _, c := range []struct{ voiceFam, musicFam string }{{"", ""}, {"chatterbox-x", "ace-x"}} {
			cfg := config.Config{VoiceGenEngine: config.EngineAudiocpp, MusicGenEngine: config.EngineAudiocpp,
				AudiocppVoiceFamily: c.voiceFam, AudiocppMusicFamily: c.musicFam}
			adv := fleetnode.Families(cfg)
			want := []string{audiocppFootprintFamily(cfg, "music"), audiocppFootprintFamily(cfg, "voice")}
			if len(adv) != 2 || adv[0] != want[0] || adv[1] != want[1] {
				t.Errorf("voice=%q music=%q: ADVERTISES %v, the lanes WRITE %v", c.voiceFam, c.musicFam, adv, want)
			}
		}
	})
}
