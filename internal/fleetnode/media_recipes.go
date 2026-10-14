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

// MediaRecipes lists the rows of this node's video, animation and audio bindings (mediacap.LaneRows): the family each binding
// names, its weight files and their sizes here, -1 for a file the node does not hold. No file is read: a row carries no content
// fingerprint and no digest in this release, so it is a statement of what the binding names and what is on disk, never an
// identity, and building it costs a stat per file.
func MediaRecipes(cfg config.Config) []MediaRecipeHealth { return mediacap.LaneRows(cfg) }

// mediaRecipeRows is the memoised MediaRecipes of this server's config: re-read at most every mediaRoutesTTL, the freshness a
// route verdict has, so polling health does not stat the weight files each time and a file that appears or goes shows within a
// minute. The config cannot change under a running server, so the memo is keyed on time alone.
func (s *Server) mediaRecipeRows() []MediaRecipeHealth {
	s.mediaRecipeMu.Lock()
	defer s.mediaRecipeMu.Unlock()
	now := mediaClock()
	if !s.mediaRecipeRead || now.Sub(s.mediaRecipeAt) >= mediaRoutesTTL {
		s.mediaRecipeMemo, s.mediaRecipeAt, s.mediaRecipeRead = MediaRecipes(s.opts.Cfg), now, true
	}
	return s.mediaRecipeMemo
}
