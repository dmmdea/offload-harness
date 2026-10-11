package fleetnode

// The node's half of image identity (P0 plan, S1; ADR 0082).
//
// A delegator that overflows an image job to this node needs to know the node would render what its own lane
// would have. /fleet/health therefore publishes what each of the node's ComfyUI image families IS (mediacap.Recipe:
// the weight files with their sizes and content fingerprints, the resolved sampling, the license a result is tagged
// with, and its digest), and an image-gen dispatch may carry the digest it matched: the node recomputes the digest of
// the family the payload names and refuses a dispatch whose digest is not that one, with 412. The digest is the whole
// contract; the check below and the delegator's match read the same mediacap.Recipe.Digest, so they cannot disagree.
// The fingerprint (mediacap/fingerprint.go) is a sample of each file, so the check sees a file replaced by one of the
// same name and size and does not see a change confined to the stretch between two sample blocks.
//
// Health stays cheap (server.go: only cached or cheap reads) and NEVER waits for a hash: the rows come from a memo of
// mediaRoutesTTL, the same freshness a route verdict has, built with the fingerprinter's Peek, which answers from its
// cache and starts the read of a file it has not hashed in the background. A family with a file still being hashed is
// published with that file "pending" and with NO digest: a digest of unhashed files would be one that two nodes in the
// same state share, which is the match by size this exists to end. The memo is rebuilt as soon as a read settles
// (Generation), so a pending answer does not outlive its hash by the minute. The admission check does not use the memo.
// It stats the three or four weight files of the one family a dispatch names, once per dispatch, and WAITS for any
// fingerprint it does not have (AwaitContext): a node whose files changed a second ago must not admit a job on a recipe it
// no longer holds, and a dispatch is not health. The wait ends when the request's context does and, whatever the context,
// after the fingerprinter's own bound (mediacap fingerprintAwaitMax); a file still unread then is named "pending" in the
// refusal, so a stalled disk costs a 412 that the delegator answers by reading health again, not a handler that hangs.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// recipeFingerprints is the cache the node's health and its 412 check read weight-file fingerprints through: the
// process-wide one (a fingerprint belongs to the file, not to a server), a var so a test can hold or fail a read.
var recipeFingerprints = mediacap.SharedFingerprints()

// ImageRecipeHealth is one row of /fleet/health image_recipes: a binding a payload's `family` can select, and what it is.
// Name is the family's request-facing name (the default binding's own family name, which may be empty on a node whose
// default is the generic graph); Default marks the default binding. Digest is empty while any of the family's files has
// no content fingerprint yet (their `fingerprint` says "pending" or "unreadable"): such a row names what it will be and
// is not an identity.
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
// lacks; such a recipe never matches. It never waits for a hash (see the file header).
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
		r, ok := mediacap.ImageRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, recipeFingerprints.Peek)
		if !ok || !namesACheckpoint(r) {
			continue
		}
		row := ImageRecipeHealth{Name: fi.Name, Default: fi.Default, Recipe: r}
		if len(r.Unfingerprinted()) == 0 {
			row.Digest = r.Digest()
		}
		out = append(out, row)
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

// imageRecipeRows is the memoised ImageRecipes of this server's config. The memo is also dropped when a fingerprint
// read has settled since it was built, so a row published pending turns into its digest on the next health read and not
// a minute later. The generation is read BEFORE the rows are built: a read that settles during the build then shows as
// a newer generation on the next call and the rows are built again.
func (s *Server) imageRecipeRows() []ImageRecipeHealth {
	s.recipeMu.Lock()
	defer s.recipeMu.Unlock()
	now := mediaClock()
	if gen := recipeFingerprints.Generation(); !s.recipeRead || now.Sub(s.recipeAt) >= mediaRoutesTTL || gen != s.recipeGen {
		s.recipeRows, s.recipeAt, s.recipeRead, s.recipeGen = ImageRecipes(s.opts.Cfg), now, true, gen
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
// to it, computed now from the files on disk. It waits for the fingerprints it needs for as long as ctx and the
// fingerprinter's bound allow.
func checkRecipe(ctx context.Context, cfg config.Config, family, want string) error {
	eff, fi, err := cfg.ResolveImageFamily(family)
	if err != nil {
		return &recipeMismatchError{family: family, want: want, why: err.Error()}
	}
	await := func(path string, size int64) string { return recipeFingerprints.AwaitContext(ctx, path, size) }
	rec, ok := mediacap.ImageRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, await)
	if !ok {
		return &recipeMismatchError{family: family, want: want, why: "that binding is not a ComfyUI one, which has no recipe to match"}
	}
	// A family with a file this node could not fingerprint is not an identity, whatever it would digest to.
	if gaps := rec.Unfingerprinted(); len(gaps) > 0 {
		return &recipeMismatchError{family: family, want: want, why: "this node cannot fingerprint " + strings.Join(gaps, ", ")}
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
