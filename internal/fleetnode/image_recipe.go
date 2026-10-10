package fleetnode

// The node's half of image identity (P0 plan, S1; ADR 0082).
//
// A delegator that overflows an image job to this node needs to know the node would render what its own lane
// would have. /fleet/health therefore publishes what each of the node's ComfyUI image families IS (mediacap.Recipe:
// the weight files with their sizes, the resolved sampling, the license a result is tagged with, and its digest),
// and an image-gen dispatch may carry the digest it matched: the node recomputes the digest of the family the
// payload names and refuses a dispatch whose digest is not that one, with 412. The digest is the whole contract; the
// check below and the delegator's match read the same mediacap.Recipe.Digest, so they cannot disagree. That is also its
// limit: the digest covers file names and byte sizes, not contents, so the 412 check cannot catch what the published
// digest cannot (two same-named, same-size files with different bytes; mediacap/recipe.go says so).
//
// Health stays cheap (server.go: only cached or cheap reads): the rows come from a memo of mediaRoutesTTL, the same
// freshness a route verdict has. The admission check does not use the memo. It stats the three or four weight files of
// the one family a dispatch names, once per dispatch, and a node whose files changed in size a second ago must not admit
// a job on a recipe it no longer holds.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// ImageRecipeHealth is one row of /fleet/health image_recipes: a binding a payload's `family` can select, and what it is.
// Name is the family's request-facing name (the default binding's own family name, which may be empty on a node whose
// default is the generic graph); Default marks the default binding.
type ImageRecipeHealth struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Digest  string `json:"digest"`
	mediacap.Recipe
}

// ImageRecipes lists the recipes of this node's ComfyUI image bindings, the default first, then every named family in
// name order (config.ImageFamilies' order). A binding is listed only when its recipe names a checkpoint: a binding that
// leaves the file to the runner's own default has nothing to identify, and a node must not be matched on a name alone.
// An sd.cpp binding has no recipe. Missing files are listed (with bytes -1) so a delegator can say which file a node
// lacks; such a recipe never matches.
func ImageRecipes(cfg config.Config) []ImageRecipeHealth {
	var out []ImageRecipeHealth
	for _, fi := range cfg.ImageFamilies() {
		// The default row is skipped on a node that has no default image binding (the same rule as ImageFamilies).
		if fi.Default && !cfg.ImageRouteConfigured() {
			continue
		}
		eff := cfg
		if !fi.Default {
			var err error
			if eff, _, err = cfg.ResolveImageFamily(fi.Name); err != nil {
				continue
			}
		}
		r, ok := mediacap.ImageRecipe(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil)
		if !ok || !namesACheckpoint(r) {
			continue
		}
		out = append(out, ImageRecipeHealth{Name: fi.Name, Default: fi.Default, Digest: r.Digest(), Recipe: r})
	}
	return out
}

func namesACheckpoint(r mediacap.Recipe) bool {
	for _, f := range r.Files {
		if f.Role == mediacap.RecipeRoleCkpt {
			return true
		}
	}
	return false
}

// imageRecipeRows is the memoised ImageRecipes of this server's config.
func (s *Server) imageRecipeRows() []ImageRecipeHealth {
	s.recipeMu.Lock()
	defer s.recipeMu.Unlock()
	now := mediaClock()
	if !s.recipeRead || now.Sub(s.recipeAt) >= mediaRoutesTTL {
		s.recipeRows, s.recipeAt, s.recipeRead = ImageRecipes(s.opts.Cfg), now, true
	}
	return s.recipeRows
}

// recipeMismatchError is an image-gen dispatch whose recipe_digest is not the digest of the family it names on this
// node: the node's weights or sampling are not what the delegator matched, or the node no longer serves that family.
// Admission answers 412 (Precondition Failed). It is not 409, which already means "this job failed here before", nor a
// 503, which means "this node is busy": a re-read of /fleet/health is what a 412 asks for, not a retry.
type recipeMismatchError struct {
	family string
	want   string
	why    string
}

func (e *recipeMismatchError) Error() string {
	name := e.family
	if name == "" {
		name = "(the default binding)"
	}
	return fmt.Sprintf("recipe_digest %s does not match image family %s on this node: %s; read /fleet/health image_recipes again and place the job from that",
		shortDigest(e.want), name, e.why)
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12] + "…"
	}
	return d
}

// checkRecipe admits an image-gen payload that named recipe_digest want for family only if this node's family digests
// to it, computed now from the files on disk.
func checkRecipe(cfg config.Config, family, want string) error {
	eff, fi, err := cfg.ResolveImageFamily(family)
	if err != nil {
		return &recipeMismatchError{family: family, want: want, why: err.Error()}
	}
	rec, ok := mediacap.ImageRecipe(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil)
	if !ok {
		return &recipeMismatchError{family: family, want: want, why: "that binding is not a ComfyUI one, which has no recipe to match"}
	}
	if have := rec.Digest(); have != strings.TrimSpace(want) {
		why := fmt.Sprintf("this node's family digests to %s", shortDigest(have))
		if missing := rec.Missing(); len(missing) > 0 {
			sort.Strings(missing)
			why += " and is missing " + strings.Join(missing, ", ")
		}
		return &recipeMismatchError{family: family, want: want, why: why}
	}
	return nil
}
