package mediacap

// The per-lane family rows a fleet node publishes for video, animation and audio (/fleet/health media_recipes, ADR 0082
// Amendment 3). A row says which family the node's binding NAMES for a lane, the weight files it loads and how big each one is
// on the node (-1: not there), so a delegator that names the node can refuse before a POST when the weights are missing.
//
// A row is NOT an identity in this release: it carries no content fingerprint and no digest, so two nodes' rows never match
// each other, and a reader that wants to match must treat an empty digest as "cannot be matched". The image Recipe (recipe.go)
// is untouched.

import (
	"github.com/dmmdea/offload-harness/internal/config"
)

// The lanes of a row: the fleet task types they belong to.
const (
	LaneVideo   = "video-gen"
	LaneAnimate = "animate"
	LaneAudio   = "audio-gen"
)

// LaneRow is one row of /fleet/health media_recipes.
type LaneRow struct {
	// Lane is the fleet task type (LaneVideo, LaneAnimate, LaneAudio).
	Lane string `json:"lane"`
	// Name is the name a caller reaches the family by: a video row carries the value of the `model` parameter (wan22, ltx25,
	// h3, hunyuan, or an sd.cpp family's own name), an animation row the family the node advertises for it (wan-animate2), an
	// audio row the family of its music lane (acestep).
	Name    string `json:"name"`
	Default bool   `json:"default"`
	// Engine is comfyui or sdcpp.
	Engine string `json:"engine"`
	// Digest is empty in this release: a row built from sizes is never an identity.
	Digest string `json:"digest"`
	// Files are the weight files, in the order the family's graph loads them. Name is the ComfyUI-relative name (subfolders
	// included) for a ComfyUI row and the bare file name for an sd.cpp row; never an absolute path.
	Files         []RecipeFile `json:"files"`
	License       string       `json:"license,omitempty"`
	CommercialUse *bool        `json:"commercial_use,omitempty"`
}

// LaneRows lists the rows of this machine's video, animation and audio bindings. Seam: the builder lands with its tests.
func LaneRows(cfg config.Config) []LaneRow { return nil }
