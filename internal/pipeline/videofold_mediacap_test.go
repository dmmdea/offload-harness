package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// The video family a call renders is folded in three places that cannot import each other: this package (resolveVideoFamily, over
// canonicalVideoFamily), fleetnode.familyFor (pinned to this package by TestVideoFootprintFamilyMatchesTheAdvertisedFamily) and
// mediacap (videoRunnerFamily, behind mediacap.VideoBinding), which a delegator that names a node reads to decide which row of
// the node's media_recipes the call would render (internal/mediaremote/named.go). The third is a copy of the runner's closed set,
// and a family added to this package's set and not to mediacap's would send a named call to the wrong row. This pins the copy to
// the fold the pipeline renders with, over the recognized names and the split-prone spellings (wrong case, padding, a typo, the
// request-level "wan"), as a request's model and as a box's videogen_family.
func TestMediacapFoldsAVideoFamilyTheWayThePipelineRendersIt(t *testing.T) {
	spellings := []string{"wan", "wan22", "ltx25", "h3", "hunyuan", "ace", "LTX25", " ltx25 ", "ltx2.5", "Wan22", "something-else"}
	for _, in := range spellings {
		_, render := resolveVideoFamily(config.Config{}, in)
		if got, _ := mediacap.VideoBinding(config.Config{}, in); got != render {
			t.Errorf("model %q: the pipeline renders %q, mediacap folds it to %q", in, render, got)
		}
	}
	for _, in := range append([]string{""}, spellings...) {
		cfg := config.Config{VideoGenFamily: in}
		_, render := resolveVideoFamily(cfg, "")
		if render == "" {
			render = videoFamilyWanSentinel // nothing bound: the runner's own default, which is Wan 2.2
		}
		if got, _ := mediacap.VideoBinding(cfg, ""); got != render {
			t.Errorf("videogen_family %q: the pipeline renders %q, mediacap folds it to %q", in, render, got)
		}
	}
}
