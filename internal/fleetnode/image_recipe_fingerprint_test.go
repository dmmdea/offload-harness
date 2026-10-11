package fleetnode

// The content fingerprint on the node (F59, ADR 0082 Amendment 2). /fleet/health publishes it per weight file and must
// never wait for it; the 412 re-check recomputes it from the disk; two nodes holding copies of the same files publish the
// same digest, and a same-named, same-size file with other bytes does not.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// swapFingerprints makes f the fingerprinter the node's health and 412 check read through, for one test.
func swapFingerprints(t *testing.T, f *mediacap.Fingerprinter) {
	t.Helper()
	prev := recipeFingerprints
	recipeFingerprints = f
	t.Cleanup(func() { recipeFingerprints = prev })
}

// warmRecipes hashes every weight file of cfg's image families, so a health read after it publishes digests and not
// pending.
func warmRecipes(t *testing.T, cfg config.Config) {
	t.Helper()
	for _, fi := range cfg.ImageFamilies() {
		name := fi.Name
		if fi.Default {
			name = ""
		}
		recipeOfFamily(t, cfg, name)
	}
}

// replaceInPlace overwrites a weight file with other bytes of the same length, at a later modification time (the way a
// same-named re-release lands on a node).
func replaceInPlace(t *testing.T, cfg config.Config, rel string, fill byte) {
	t.Helper()
	p := filepath.Join(cfg.ComfyDir, filepath.FromSlash(rel))
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

// HEALTH NEVER WAITS FOR A HASH. With the read of every weight file held, /fleet/health answers at once, the rows are
// there with every file pending and NO digest (a digest of unhashed files is not an identity), and when the reads settle
// the next health read carries the digests without waiting for the 60 s memo to run out.
func TestHealthNeverBlocksOnHashingAndPublishesPending(t *testing.T) {
	cfg := recipeNode(t)
	gate := make(chan struct{})
	swapFingerprints(t, mediacap.NewFingerprinterWith(func(path string, size int64) (string, error) {
		<-gate
		return mediacap.FingerprintFile(path, size)
	}))
	now := time.Now()
	prev := mediaClock
	mediaClock = func() time.Time { return now }
	t.Cleanup(func() { mediaClock = prev })
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	answered := make(chan struct{})
	var h healthRecipes
	go func() {
		defer close(answered)
		h, _ = readHealthRecipes(t, s)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("/fleet/health waited for the weight files to be hashed")
	}
	if len(h.Recipes) != 3 {
		t.Fatalf("every family is still listed while its files are hashed, got %d rows", len(h.Recipes))
	}
	for _, row := range h.Recipes {
		if row.Digest != "" {
			t.Errorf("%s: published digest %q for files that are not hashed", row.Name, row.Digest)
		}
		for _, f := range row.Files {
			if f.Fingerprint != mediacap.FingerprintPending || f.Bytes <= 0 {
				t.Errorf("%s: file %s = %+v, want its size and fingerprint %q", row.Name, f.Name, f, mediacap.FingerprintPending)
			}
		}
	}

	close(gate)
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, _ = readHealthRecipes(t, s)
		ready := len(h.Recipes) == 3
		for _, row := range h.Recipes {
			ready = ready && row.Digest != ""
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the digests never appeared after the reads settled (the memo must not hold a pending answer for its whole minute)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, row := range h.Recipes {
		name := row.Name
		if row.Default {
			name = ""
		}
		if want := recipeOfFamily(t, cfg, name).Digest(); row.Digest != want {
			t.Errorf("%s: digest %s, the family's own recipe digests to %s", row.Name, row.Digest, want)
		}
		for _, f := range row.Files {
			if !mediacap.IsFingerprint(f.Fingerprint) {
				t.Errorf("%s: %s fingerprint = %q, want a sha256", row.Name, f.Name, f.Fingerprint)
			}
		}
	}
}

// NODE AND DELEGATOR AGREE ON IDENTICAL FILES. Two machines whose trees hold copies of the same files digest each family
// alike (the delegator computes its own recipe the way the node does), and one same-named, same-size file with other
// bytes makes exactly its family differ.
func TestTwoNodesWithCopiesOfTheSameFilesAgreeAndOneChangedFileSplitsThem(t *testing.T) {
	a, b := recipeNode(t), recipeNode(t)
	warmRecipes(t, a)
	warmRecipes(t, b)
	for _, name := range []string{"", "qwen-image-2.1", "qwen-image-2.1-fast"} {
		if da, db := recipeOfFamily(t, a, name).Digest(), recipeOfFamily(t, b, name).Digest(); da != db {
			t.Fatalf("family %q: copies of the same files must digest alike on both machines", name)
		}
	}
	for _, row := range ImageRecipes(b) {
		name := row.Name
		if row.Default {
			name = ""
		}
		if row.Digest != recipeOfFamily(t, a, name).Digest() {
			t.Errorf("family %q: the node published %s, the other machine computes %s", row.Name, row.Digest, recipeOfFamily(t, a, name).Digest())
		}
	}

	replaceInPlace(t, b, "models/diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors", 0xAB)
	warmRecipes(t, b)
	for name, differs := range map[string]bool{"": false, "qwen-image-2.1": false, "qwen-image-2.1-fast": true} {
		same := recipeOfFamily(t, a, name).Digest() == recipeOfFamily(t, b, name).Digest()
		if same == differs {
			t.Errorf("family %q: digests alike = %v, want %v after one file of qwen-image-2.1-fast got other bytes of the same size", name, same, !differs)
		}
	}
}

// THE 412 SEES CONTENT. A dispatch that carries the digest the delegator matched is refused when the file under the family
// was replaced in place by one of the same name and size, and admitted again with the digest of what is there now.
func TestRecipeDigestRecheckSeesAFileReplacedInPlace(t *testing.T) {
	cfg := recipeNode(t)
	runner := &fakeRunner{}
	s, jobs := newTestServer(t, cfg, runner, nil)
	before := recipeOfFamily(t, cfg, "qwen-image-2.1-fast").Digest()

	replaceInPlace(t, cfg, "models/diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors", 0xCD)
	rec := do(t, s, "POST", "/fleet/dispatch", dispatchBody(t, "job-replaced", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1-fast", "recipe_digest": before}), nil)
	if rec.Code != 412 {
		t.Fatalf("a file replaced in place by one of the same size must be a 412, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "digests to") {
		t.Errorf("the refusal must say what the node's family digests to now: %s", rec.Body.String())
	}
	if queued, running := jobs.Counts(); queued+running != 0 || len(runner.requests()) != 0 {
		t.Error("a refused dispatch creates no job and runs nothing")
	}
	after := recipeOfFamily(t, cfg, "qwen-image-2.1-fast").Digest()
	if after == before {
		t.Fatal("the family's digest must change when a weight file's content does")
	}
	rec = do(t, s, "POST", "/fleet/dispatch", dispatchBody(t, "job-after", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1-fast", "recipe_digest": after}), nil)
	if rec.Code != 202 {
		t.Errorf("the digest of what is on the disk now must be admitted, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A node that cannot read one of its own weight files cannot vouch for the family: the 412 names the file and its state,
// and does not guess from the size.
func TestRecipeDigestRecheckNamesAFileItCannotFingerprint(t *testing.T) {
	cfg := recipeNode(t)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()
	swapFingerprints(t, mediacap.NewFingerprinterWith(func(path string, size int64) (string, error) {
		if strings.Contains(filepath.ToSlash(path), "/text_encoders/") {
			return "", os.ErrPermission
		}
		return mediacap.FingerprintFile(path, size)
	}))
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)
	rec := do(t, s, "POST", "/fleet/dispatch", dispatchBody(t, "job-unreadable", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1", "recipe_digest": good}), nil)
	if rec.Code != 412 {
		t.Fatalf("a family with a file this node cannot fingerprint must be a 412, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "qwen3vl_8b_bf16.safetensors (unreadable)") {
		t.Errorf("the refusal must name the file and its state: %s", body)
	}
	if queued, running := jobs.Counts(); queued+running != 0 {
		t.Error("a refused dispatch creates no job")
	}
}
