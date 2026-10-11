package fleetnode

// A 412 says WHY (F59 polish round). A delegator tells a node that has not finished reading its weight files (a wait: it cannot
// say yet, ask again soon) from one whose files or binding are not what it published (a difference: leave it alone longer), and
// it can only do that if the refusal names the cause in a member of its own and not in a sentence it would have to match. These tests pin the node's half: the member, its three values, and which refusal gets which.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// refusalOf checks that rec is a 412 of the shape every refusal has ({"status":"error","error":...}) and returns its cause.
func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("want a 412, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["status"] != "error" || body["error"] == "" || body["error"] == nil {
		t.Errorf("the refusal keeps the shape of every refusal, status and error: %s", rec.Body.String())
	}
	cause, _ := body[mediacap.RecipeRefusalCauseKey].(string)
	return cause
}

// dispatchWithin posts an image-gen dispatch whose request ends after d, as a client that gives up does.
func dispatchWithin(t *testing.T, s *Server, d time.Duration, id string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", strings.NewReader(dispatchBody(t, id, payload))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// A family whose weight file was replaced in place no longer digests to what the delegator matched: that is a CHANGE.
func TestTheRefusalOfAChangedFamilyNamesTheCauseChanged(t *testing.T) {
	cfg := recipeNode(t)
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)
	before := recipeOfFamily(t, cfg, "qwen-image-2.1-fast").Digest()
	replaceInPlace(t, cfg, "models/diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors", 0xCD)

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-cause-changed", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1-fast", "recipe_digest": before}), nil)
	if got := refusalOf(t, rec); got != mediacap.RecipeRefusalChanged {
		t.Errorf("cause = %q, want %q for a file replaced in place", got, mediacap.RecipeRefusalChanged)
	}
	if queued, running := jobs.Counts(); queued+running != 0 {
		t.Error("a refused dispatch creates no job")
	}
}

// A family the node does not serve (its binding changed since it published) is a difference too, not a wait.
func TestTheRefusalOfAFamilyTheNodeDoesNotServeNamesTheCauseChanged(t *testing.T) {
	cfg := recipeNode(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-cause-gone", map[string]any{
		"prompt": "p", "family": "no-such-family", "recipe_digest": good}), nil)
	if got := refusalOf(t, rec); got != mediacap.RecipeRefusalChanged {
		t.Errorf("cause = %q, want %q for a family this node does not serve", got, mediacap.RecipeRefusalChanged)
	}
}

// A family the node now binds to another engine (sd.cpp has no recipe to match) is a difference too.
func TestTheRefusalOfANonComfyBindingNamesTheCauseChanged(t *testing.T) {
	cfg := recipeNode(t)
	cfg.ImageGenFamilies["z-image"] = config.FamilyOverlay{
		"license": json.RawMessage(`"Apache-2.0"`), "commercial_use": json.RawMessage(`true`),
		"imagegen_engine": json.RawMessage(`"sdcpp"`), "sdcpp_bin": json.RawMessage(`"/opt/sd-cli"`),
		"sdcpp_model": json.RawMessage(`"/models/z.gguf"`),
	}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-cause-sdcpp", map[string]any{
		"prompt": "p", "family": "z-image", "recipe_digest": good}), nil)
	if got := refusalOf(t, rec); got != mediacap.RecipeRefusalChanged {
		t.Errorf("cause = %q, want %q for a binding with no recipe: %s", got, mediacap.RecipeRefusalChanged, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a ComfyUI one") {
		t.Errorf("the node's sentence says why: %s", rec.Body.String())
	}
}

// A node that is still reading its files (a restart, a file just replaced, a slow disk) cannot vouch for the family yet, and has
// not found it different: its refusal says PENDING, and it names the file as before.
func TestTheRefusalOfAFileStillBeingReadNamesTheCausePending(t *testing.T) {
	cfg := recipeNode(t)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest() // from the real cache, before the reads are held
	heldFingerprints(t)
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)

	rec := dispatchWithin(t, s, 150*time.Millisecond, "job-cause-pending", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1", "recipe_digest": good})
	if got := refusalOf(t, rec); got != mediacap.RecipeRefusalPending {
		t.Errorf("cause = %q, want %q for files whose reads have not finished: %s", got, mediacap.RecipeRefusalPending, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "(pending)") {
		t.Errorf("the refusal still names the file and its state: %s", rec.Body.String())
	}
	if queued, running := jobs.Counts(); queued+running != 0 {
		t.Error("a refused dispatch creates no job")
	}
}

// A node that cannot read one of its own files cannot vouch for the family, and that is not a wait: its refusal says UNREADABLE.
func TestTheRefusalOfAFileThatCannotBeReadNamesTheCauseUnreadable(t *testing.T) {
	cfg := recipeNode(t)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()
	swapFingerprints(t, mediacap.NewFingerprinterWith(func(path string, size int64) (string, error) {
		if strings.Contains(filepath.ToSlash(path), "/text_encoders/") {
			return "", os.ErrPermission
		}
		return mediacap.FingerprintFile(path, size)
	}))
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-cause-unreadable", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1", "recipe_digest": good}), nil)
	if got := refusalOf(t, rec); got != mediacap.RecipeRefusalUnreadable {
		t.Errorf("cause = %q, want %q for a file this node cannot read: %s", got, mediacap.RecipeRefusalUnreadable, rec.Body.String())
	}
}

// One file that cannot be read outweighs any number that are only pending: the node is waiting for the others, but it will not be
// able to vouch for the family when they land, so it is not a wait. A file the node does not hold is not a gap at all.
func TestAnUnreadableFileOutweighsPendingOnes(t *testing.T) {
	file := func(name string, bytes int64, fp string) mediacap.RecipeFile {
		return mediacap.RecipeFile{Role: "ckpt", Name: name, Bytes: bytes, Fingerprint: fp}
	}
	for _, tc := range []struct {
		what  string
		files []mediacap.RecipeFile
		want  string
	}{
		{"only pending", []mediacap.RecipeFile{file("a", 1, mediacap.FingerprintPending), file("b", 1, mediacap.FingerprintPending)}, mediacap.RecipeRefusalPending},
		{"pending and unreadable", []mediacap.RecipeFile{file("a", 1, mediacap.FingerprintPending), file("b", 1, mediacap.FingerprintUnreadable)}, mediacap.RecipeRefusalUnreadable},
		{"a state nothing publishes", []mediacap.RecipeFile{file("a", 1, mediacap.FingerprintPending), file("b", 1, "")}, mediacap.RecipeRefusalUnreadable},
		{"pending and a missing file", []mediacap.RecipeFile{file("a", 1, mediacap.FingerprintPending), file("b", -1, "")}, mediacap.RecipeRefusalPending},
	} {
		if got := gapCause(mediacap.Recipe{Files: tc.files}); got != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.what, got, tc.want)
		}
	}
}
