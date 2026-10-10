package delegate

import (
	"context"
	"testing"
)

// The delegator's health decoder reads what an overflow placement needs of a node (ADR 0082): the recipe of each
// ComfyUI image family, whether the node carries `refine`, and the holder of its lease with the reason and the time
// it declared, which the answer to a caller quotes ("the workstation's lane is held by ... for 5 more minutes"). A
// node that predates the keys decodes to nothing, which is unknown and never a match.
func TestNodeViewDecodesRecipesAndLeaseReason(t *testing.T) {
	srv := healthServer(t, `{
		"node_id": "node-b", "schema_version": 1, "harness_version": "0.179.0",
		"supported_task_types": ["image-gen"], "queue_depth": 0,
		"refine_honoured": true,
		"image_recipes": [
			{"name": "qwen-image-2.1", "default": false, "digest": "d1", "engine": "comfyui", "graph": "qwen-image-2.1",
			 "files": [{"role": "ckpt", "name": "a.safetensors", "bytes": 10}, {"role": "vae", "name": "v.safetensors", "bytes": -1}],
			 "steps": 40, "cfg": 1, "sampler": "euler", "scheduler": "simple", "schedule": "official", "shift": 1.5,
			 "lora_strength": 0.5, "license": "Qwen Research License", "commercial_use": false, "explicit": ["steps", "cfg"]},
			{"name": "krea2", "default": true, "digest": "d2", "engine": "comfyui", "graph": "krea2", "files": []}
		],
		"lease": {"held": true, "class": "media", "pid": 7, "reason": "image-gen", "until": "2026-10-10T12:00:00Z", "remaining_sec": 321, "busy": true}
	}`, nil)
	v, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !v.RefineHonoured || v.HarnessVersion != "0.179.0" {
		t.Errorf("refine_honoured / harness_version = %v / %q", v.RefineHonoured, v.HarnessVersion)
	}
	if len(v.ImageRecipes) != 2 {
		t.Fatalf("recipes = %+v", v.ImageRecipes)
	}
	r := v.ImageRecipes[0]
	if r.Name != "qwen-image-2.1" || r.Default || r.Digest != "d1" || r.Engine != "comfyui" || r.Graph != "qwen-image-2.1" ||
		r.Steps != 40 || r.CFG != 1 || r.Sampler != "euler" || r.Scheduler != "simple" || r.Schedule != "official" ||
		r.Shift != 1.5 || r.LoRAStrength != 0.5 || r.License != "Qwen Research License" || r.CommercialUse == nil || *r.CommercialUse ||
		len(r.Explicit) != 2 {
		t.Errorf("recipe row = %+v", r)
	}
	if len(r.Files) != 2 || r.Files[0].Role != "ckpt" || r.Files[0].Name != "a.safetensors" || r.Files[0].Bytes != 10 || r.Files[1].Bytes != -1 {
		t.Errorf("files = %+v", r.Files)
	}
	if !v.ImageRecipes[1].Default || v.ImageRecipes[1].Name != "krea2" {
		t.Errorf("default row = %+v", v.ImageRecipes[1])
	}
	if !v.LeaseHeld || v.LeaseClass != "media" || v.LeaseReason != "image-gen" || v.LeaseRemainingSec != 321 {
		t.Errorf("lease = held %v class %q reason %q remaining %d", v.LeaseHeld, v.LeaseClass, v.LeaseReason, v.LeaseRemainingSec)
	}

	old := healthServer(t, mediaHealthJSON, nil)
	ov, err := FetchNodeView(context.Background(), old.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if ov.RefineHonoured || len(ov.ImageRecipes) != 0 || ov.LeaseReason != "" || ov.LeaseClass != "" {
		t.Errorf("a node that publishes none decodes to nothing: %+v", ov)
	}
}
