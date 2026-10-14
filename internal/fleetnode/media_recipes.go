package fleetnode

// The node's half of the video, animation and audio family rows (ADR 0082 Amendment 3): /fleet/health media_recipes says,
// per lane, which family each of the node's bindings names and how big each weight file is on this node (-1: not there).
// A delegator that NAMES this node reads it to refuse a call for a weight the node lacks before it sends anything.

import (
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// MediaRecipeHealth is one row of /fleet/health media_recipes (mediacap.LaneRow).
type MediaRecipeHealth = mediacap.LaneRow

// MediaRecipes lists the rows of this node's video, animation and audio bindings. Seam: the build lands with its tests.
func MediaRecipes(cfg config.Config) []MediaRecipeHealth { return nil }

// mediaRecipeRows is the media_recipes advertisement of this server's config.
func (s *Server) mediaRecipeRows() []MediaRecipeHealth { return MediaRecipes(s.opts.Cfg) }
