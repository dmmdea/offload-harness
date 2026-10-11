package fleetnode

// The 412 re-check waits for a fingerprint, so it must not outlive the request that asked for it (F59 fix round): a
// dispatch whose client is gone, or whose read stalls, ends with the file named as not fingerprinted, never hangs the handler.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// heldFingerprints swaps in a fingerprinter whose reads all wait for the returned release, and releases them at the end.
func heldFingerprints(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	released := false
	release = func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	swapFingerprints(t, mediacap.NewFingerprinterWith(func(path string, size int64) (string, error) {
		<-gate
		return mediacap.FingerprintFile(path, size)
	}))
	return release
}

func answeredWithin(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return in %s: the handler would hang on a read that has not finished", what, d)
		return nil
	}
}

// THE RE-CHECK ENDS WITH ITS CALLER. With every read held, a check whose context is cancelled returns the refusal that names
// the files as pending, at once, instead of waiting for reads that may never finish.
func TestRecipeRecheckEndsWhenItsCallerIsGone(t *testing.T) {
	cfg := recipeNode(t)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest() // taken from the real cache, before the reads are held
	heldFingerprints(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := answeredWithin(t, 5*time.Second, "checkRecipe of a cancelled caller", func() error { return checkRecipe(ctx, cfg, "qwen-image-2.1", good) })
	var mm *recipeMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want the recipe refusal", err)
	}
	if !strings.Contains(err.Error(), "qwen3vl_8b_bf16.safetensors (pending)") {
		t.Errorf("the refusal must name the file that has no fingerprint yet and say it is pending: %s", err)
	}
}

// The same through the dispatch builder the handler and the claim loop call: the context they pass reaches the re-check.
func TestBuildRequestPassesItsContextToTheRecipeRecheck(t *testing.T) {
	cfg := recipeNode(t)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()
	heldFingerprints(t)
	payload, _ := json.Marshal(map[string]any{"prompt": "p", "family": "qwen-image-2.1", "recipe_digest": good})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := answeredWithin(t, 5*time.Second, "BuildRequest of a cancelled caller", func() error {
		_, cleanup, err := BuildRequest(ctx, cfg, false, "image-gen", payload)
		cleanup()
		return err
	})
	var mm *recipeMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want the recipe refusal (a 412)", err)
	}
}
