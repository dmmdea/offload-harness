package mediaremote

// The delegator's half of the content fingerprint (F59, ADR 0082 Amendment 2). A node holds a family only when its files
// have the same CONTENT as this machine's, and a node (or this machine) that has not finished fingerprinting a file is a
// named miss, never a match by size.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// replaceInPlace overwrites one weight file of tree with other bytes of the same length at a later modification time: a
// same-named re-release landing under the same name and size.
func replaceInPlace(t *testing.T, tree, rel string, fill byte) {
	t.Helper()
	p := filepath.Join(tree, filepath.FromSlash(rel))
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, st.Size())
	for i := range b {
		b[i] = fill
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
}

// markPending rewrites a node's published rows the way a node still hashing its files publishes them: no digest, every
// file pending.
func markPending(keepDigest bool) func(map[string]any) {
	return func(m map[string]any) {
		rows, _ := m["image_recipes"].([]any)
		for _, r := range rows {
			row := r.(map[string]any)
			if !keepDigest {
				row["digest"] = ""
			}
			for _, f := range row["files"].([]any) {
				f.(map[string]any)["fingerprint"] = mediacap.FingerprintPending
			}
		}
	}
}

// F59 AT THE DOOR. The node holds the delegator's family by name and size, with other bytes in one file. It is not a
// candidate, nothing is sent to it, and the answer names the file and says it is the content that differs.
func TestSameNameAndSizeWithOtherBytesIsANamedMiss(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, n)
	replaceInPlace(t, cfg.ComfyDir, "models/diffusion_models/"+int8Ckpt, 0xEE)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := runDoor(cfg, lr, overflowReq(t, "qwen-image-2.1", nil), AtOnce)
	if _, runs := lr.counts(); runs != 0 {
		t.Fatalf("nothing matches: the call is a ticket that waits for a lane: runs %d", runs)
	}
	noPosts(t, "the node whose checkpoint has other bytes", n)
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "not-capable" {
		t.Fatalf("cluster = %+v", clusterOf(t, res))
	}
	want := "imagegen_ckpt: " + int8Ckpt + " has the same size on both and different content"
	if !strings.Contains(row.Why, want) {
		t.Errorf("why must say the content differs: want %q in %s", want, row.Why)
	}
	if len(row.Differs) != 1 || !strings.HasPrefix(row.Differs[0], want) {
		t.Errorf("differs = %v", row.Differs)
	}
}

// A node that has not finished hashing is a NAMED MISS: it holds this machine's build by name and size, and the answer
// says it is waiting on the fingerprint of those files. Nothing is sent to it. The same node, once it publishes its
// digests, takes the call (the existing overflow tests).
func TestANodeStillHashingIsANamedMiss(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{healthEdit: markPending(false)})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := runDoor(cfg, lr, overflowReq(t, "qwen-image-2.1", nil), AtOnce)
	noPosts(t, "the node that is still hashing", n)
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "not-capable" {
		t.Fatalf("cluster = %+v", clusterOf(t, res))
	}
	for _, want := range []string{"qwen-image-2.1", "has not fingerprinted", int8Ckpt + " (pending)"} {
		if !strings.Contains(row.Why, want) {
			t.Errorf("why must contain %q: %s", want, row.Why)
		}
	}
}

// A row that still carries the digest while its files say pending (a publisher that is old, or wrong) is no more a match
// than one with no digest: the files are read, not only the digest.
func TestARowWithADigestButPendingFilesIsNotAMatch(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{healthEdit: markPending(true)})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := runDoor(cfg, lr, overflowReq(t, "qwen-image-2.1", nil), AtOnce)
	noPosts(t, "the node whose rows say pending", n)
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "not-capable" || !strings.Contains(row.Why, "(pending)") {
		t.Errorf("the row must be a named miss that says pending: %+v", clusterOf(t, res))
	}
}

// THIS machine's own recipe must be fingerprinted before anything is compared: a file it cannot read is a reason the
// family has no identity here, and the call runs locally as it does for a family with a missing file.
func TestLocalIdentityNamesAFileItCannotFingerprint(t *testing.T) {
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")})
	prev := localFingerprints
	localFingerprints = func(path string, size int64) string {
		if strings.Contains(filepath.ToSlash(path), "/text_encoders/") {
			return mediacap.FingerprintUnreadable
		}
		return prev(path, size)
	}
	t.Cleanup(func() { localFingerprints = prev })
	_, ok, why := localIdentity(cfg, "qwen-image-2.1")
	if ok {
		t.Fatal("a family with a file this machine cannot fingerprint has no identity")
	}
	if !strings.Contains(why, "qwen3vl_8b_bf16.safetensors (unreadable)") {
		t.Errorf("the reason must name the file and its state: %s", why)
	}
}

// matchNode refuses to compare when THIS machine's recipe is not fingerprinted, even against a row that would digest
// alike: two recipes whose files are all "pending" are the same string, and must not be one match.
func TestTwoPendingRecipesAreNeverAMatch(t *testing.T) {
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")})
	local, ok, why := localIdentity(cfg, "qwen-image-2.1")
	if !ok {
		t.Fatal(why)
	}
	pend := func(r mediacap.Recipe) mediacap.Recipe {
		r.Files = append([]mediacap.RecipeFile(nil), r.Files...)
		for i := range r.Files {
			r.Files[i].Fingerprint = mediacap.FingerprintPending
		}
		return r
	}
	local.recipe = pend(local.recipe)
	row := delegate.ImageRecipeView{Name: "qwen-image-2.1", Digest: local.recipe.Digest(), Engine: local.recipe.Engine, Graph: local.recipe.Graph,
		Steps: local.recipe.Steps, CFG: local.recipe.CFG, Sampler: local.recipe.Sampler, Scheduler: local.recipe.Scheduler, Schedule: local.recipe.Schedule,
		License: local.recipe.License, CommercialUse: local.recipe.CommercialUse}
	for _, f := range local.recipe.Files {
		row.Files = append(row.Files, delegate.ImageRecipeFileView{Role: f.Role, Name: f.Name, Bytes: f.Bytes, Fingerprint: f.Fingerprint})
	}
	v := delegate.NodeView{Tasks: []string{taskImage}, HarnessVersion: localVersion, ImageRecipes: []delegate.ImageRecipeView{row}, RefineHonoured: true}
	if local.recipe.Digest() != row.Digest {
		t.Fatal("the premise: both sides are the same string")
	}
	if m, ok := matchNode(local, v, need{}); ok {
		t.Fatalf("two pending recipes matched: %+v", m)
	}
}
