package delegate

import (
	"context"
	"testing"
)

// The delegator's health decoder reads the family rows of a node's video, animation and audio bindings (health media_recipes,
// ADR 0082 Amendment 3): the lane, the name a caller reaches the family by, the files with their sizes (-1: the node does not
// hold the file) and the license pair. A node that predates the key decodes to nothing, which is unknown and never "no weights".
func TestNodeViewDecodesMediaRecipes(t *testing.T) {
	srv := healthServer(t, `{
		"node_id": "node-b", "schema_version": 1, "harness_version": "0.186.0",
		"supported_task_types": ["video-gen", "animate", "audio-gen"], "queue_depth": 0,
		"media_recipes": [
			{"lane": "video-gen", "name": "wan22", "default": true, "engine": "comfyui", "digest": "",
			 "files": [{"role": "unet_high", "name": "wan/high.gguf", "bytes": 10}, {"role": "vae", "name": "v.safetensors", "bytes": -1}],
			 "license": "Apache-2.0", "commercial_use": true},
			{"lane": "animate", "name": "wan-animate2", "default": true, "engine": "comfyui", "digest": "", "files": []},
			{"lane": "audio-gen", "name": "acestep", "default": true, "engine": "comfyui", "digest": "", "files": []}
		]
	}`, nil)
	v, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.MediaRecipes) != 3 {
		t.Fatalf("rows = %+v", v.MediaRecipes)
	}
	r := v.MediaRecipes[0]
	if r.Lane != "video-gen" || r.Name != "wan22" || !r.Default || r.Engine != "comfyui" || r.Digest != "" ||
		r.License != "Apache-2.0" || r.CommercialUse == nil || !*r.CommercialUse {
		t.Errorf("video row = %+v", r)
	}
	if len(r.Files) != 2 || r.Files[0].Role != "unet_high" || r.Files[0].Name != "wan/high.gguf" || r.Files[0].Bytes != 10 || r.Files[1].Bytes != -1 {
		t.Errorf("files = %+v", r.Files)
	}
	if v.MediaRecipes[1].Lane != "animate" || v.MediaRecipes[2].Lane != "audio-gen" {
		t.Errorf("rows = %+v", v.MediaRecipes)
	}
}

func TestANodeThatPredatesMediaRecipesDecodesToNothing(t *testing.T) {
	old := healthServer(t, mediaHealthJSON, nil)
	v, err := FetchNodeView(context.Background(), old.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.MediaRecipes != nil {
		t.Errorf("an older node publishes no rows, which is unknown and never an empty list of weights: %+v", v.MediaRecipes)
	}
}
