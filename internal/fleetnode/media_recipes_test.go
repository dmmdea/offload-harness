package fleetnode

// The video, animation and audio family rows on the wire (ADR 0082 Amendment 3). A node publishes, per lane, which family each
// of its bindings names and how big each weight file is here (-1: not there); a delegator that names the node reads it to refuse
// a call for a weight the node lacks. The keys are additive: a node that binds none of the three lanes publishes none of them.

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
	"github.com/dmmdea/offload-harness/internal/delegate"
)

type healthMediaRecipes struct {
	Rows []struct {
		Lane    string `json:"lane"`
		Name    string `json:"name"`
		Default bool   `json:"default"`
		Engine  string `json:"engine"`
		Digest  string `json:"digest"`
		Files   []struct {
			Role        string `json:"role"`
			Name        string `json:"name"`
			Bytes       int64  `json:"bytes"`
			Fingerprint string `json:"fingerprint"`
		} `json:"files"`
	} `json:"media_recipes"`
}

func readHealthMediaRecipes(t *testing.T, s *Server) (healthMediaRecipes, map[string]json.RawMessage) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", rec.Code, rec.Body.String())
	}
	var h healthMediaRecipes
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return h, raw
}

// mediaLaneNode binds the three lanes through ComfyUI and holds the Wan 2.2 default files and the ACE-Step unet only.
func mediaLaneNode(t *testing.T) config.Config {
	t.Helper()
	comfy := t.TempDir()
	for rel, n := range map[string]int{
		"models/diffusion_models/wan2.2_i2v_high_noise_14B_Q4_K_S.gguf":  11,
		"models/diffusion_models/wan2.2_i2v_low_noise_14B_Q4_K_M.gguf":   22,
		"models/text_encoders/umt5_xxl_fp8_e4m3fn_scaled.safetensors":    33,
		"models/vae/wan_2.1_vae.safetensors":                             44,
		"models/diffusion_models/acestep_v1.5_xl_turbo_bf16.safetensors": 5,
	} {
		p := filepath.Join(comfy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{
		ComfyDir: comfy, VideoGenScript: "render/comfy-video.mjs", AnimateGenScript: "render/comfy-animate.mjs",
		MusicGenScript: "render/comfy-music.mjs", VoiceGenScript: "render/tts.mjs",
	}
}

// /fleet/health publishes one row per family each lane binding names, with the files and their sizes on this node, and no
// digest and no fingerprint (a row is not an identity here).
func TestHealthPublishesMediaRecipesForTheBoundLanes(t *testing.T) {
	s, _ := newTestServer(t, mediaLaneNode(t), &fakeRunner{}, nil)
	h, _ := readHealthMediaRecipes(t, s)
	byLane := map[string]int{}
	for _, r := range h.Rows {
		byLane[r.Lane]++
		if r.Digest != "" || r.Engine != "comfyui" {
			t.Errorf("row %s/%s: digest %q engine %q", r.Lane, r.Name, r.Digest, r.Engine)
		}
		for _, f := range r.Files {
			if f.Fingerprint != "" {
				t.Errorf("row %s/%s file %s carries a fingerprint %q", r.Lane, r.Name, f.Name, f.Fingerprint)
			}
		}
	}
	if byLane["video-gen"] != 1 || byLane["animate"] != 1 || byLane["audio-gen"] != 1 || len(h.Rows) != 3 {
		t.Fatalf("rows per lane = %v (rows %+v), want one video, one animate and one music row", byLane, h.Rows)
	}
	for _, r := range h.Rows {
		if r.Lane != "video-gen" {
			continue
		}
		if r.Name != "wan22" || !r.Default || len(r.Files) != 4 || r.Files[0].Bytes != 11 || r.Files[3].Bytes != 44 {
			t.Errorf("video row = %+v", r)
		}
	}
}

// The twin of "a node without image-gen publishes neither key": a node that binds none of the three lanes publishes no
// media_recipes, so an older reader and an image-only node see the shape they always did.
func TestHealthOmitsMediaRecipesOnANodeWithoutTheLanes(t *testing.T) {
	for name, cfg := range map[string]config.Config{
		"image only": {ImageGenScript: "render/comfy-generate.mjs", ComfyDir: t.TempDir()},
		"bare":       {},
		"stt only":   {STTModel: "large-v3-turbo"},
	} {
		s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
		_, raw := readHealthMediaRecipes(t, s)
		if _, has := raw["media_recipes"]; has {
			t.Errorf("%s: a node with none of the three lanes published media_recipes: %s", name, raw["media_recipes"])
		}
	}
}

// Health stays cheap: the rows come from a memo of mediaRoutesTTL, so polling health does not stat the weight files every time,
// and a file that appears or changes shows within a minute.
func TestMediaRecipesAreMemoisedForAMinute(t *testing.T) {
	cfg := mediaLaneNode(t)
	now := time.Now()
	prev := mediaClock
	mediaClock = func() time.Time { return now }
	t.Cleanup(func() { mediaClock = prev })
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	first, _ := readHealthMediaRecipes(t, s)

	if err := os.WriteFile(filepath.Join(cfg.ComfyDir, "models", "vae", "wan_2.1_vae.safetensors"), make([]byte, 999), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _ := readHealthMediaRecipes(t, s)
	if again.Rows[0].Files[3].Bytes != first.Rows[0].Files[3].Bytes {
		t.Fatal("inside the memo window health must not read the weight files again")
	}
	now = now.Add(mediaRoutesTTL + time.Second)
	later, _ := readHealthMediaRecipes(t, s)
	if later.Rows[0].Files[3].Bytes != 999 {
		t.Fatalf("a file that changed must show once the memo is a minute old: %+v", later.Rows[0].Files)
	}
}

// The shipped decoder reads the rows from a real node's health beside the fields placement has always read.
func TestMediaRecipesDecodeThroughTheDelegatorView(t *testing.T) {
	s, _ := newTestServer(t, mediaLaneNode(t), &fakeRunner{}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's decoder rejected a payload with media_recipes: %v", err)
	}
	if view.NodeID != "testnode" {
		t.Errorf("the old keys still decode: node %q", view.NodeID)
	}
	var names []string
	for _, r := range view.MediaRecipes {
		names = append(names, r.Lane+"/"+r.Name)
	}
	if got := strings.Join(names, " "); got != "video-gen/wan22 animate/wan-animate2 audio-gen/acestep" {
		t.Errorf("the view's rows = %q", got)
	}
}
