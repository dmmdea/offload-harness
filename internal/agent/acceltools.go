package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AccelLane is one accelerator's in-process lane for the agent loop: the device
// id (from config.Accelerators) and the func that reaches its sidecar. The
// builder passes every wired lane in config order (pipeline.NewLoopAccel), and
// that order is load-bearing — see accelLaneTools.
type AccelLane struct {
	ID   string
	Call NPUFunc
	// Remote marks a lane that forwards to a fleet node carrying the device
	// (accelremote, Coral Phase B); the tool descriptions say so.
	Remote bool
}

// laneTool is the one adapter shape every accelerator tool takes, on both the
// loop and the MCP surface: parse args, refuse a missing or mistyped required
// argument as a defer (never a tool error), pass the JSON through to the
// sidecar tool of the same name. Extracted from npuTools' local closure so the
// Coral table cannot drift from the Hailo one.
func laneTool(call NPUFunc, name, sidecar, requiredArg, desc, schema string) Tool {
	return Tool{
		ToolSpec: ToolSpec{Name: name, Description: desc, Schema: json.RawMessage(schema)},
		Exec: func(ctx context.Context, args string) (string, error) {
			var in map[string]any
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return "", err
			}
			raw, present := in[requiredArg]
			s, isStr := raw.(string)
			if present && !isStr {
				b, _ := json.Marshal(map[string]any{"deferred": true,
					"reason": fmt.Sprintf("%s must be a string, got %T", requiredArg, raw)})
				return string(b), nil
			}
			if strings.TrimSpace(s) == "" {
				b, _ := json.Marshal(map[string]any{"deferred": true, "reason": "empty " + requiredArg})
				return string(b), nil
			}
			return call(ctx, sidecar, in)
		},
	}
}

// coralTools is the Coral Edge TPU's tool table (Coral design D5): four
// capabilities, each grounded on an artifact the sidecar verifies by sha256.
// Two of the names — offload_object_detect, offload_image_embed — are
// capability names the Hailo lane also owns; the shared-name rule in
// accelLaneTools decides who registers them on a box carrying both.
func coralTools(call NPUFunc) []Tool {
	img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG/BMP)"}`
	return []Tool{
		laneTool(call, "offload_classify_image", "classify", "image_path",
			"Classify an image on the LOCAL Coral Edge TPU (free, on-box, ~3 ms). domain=imagenet (default; EfficientNet-EdgeTPU-S, 1000 classes) or birds|insects|plants (MobileNet v2 iNat). Returns {results:[{label,score}],best,model,domain} top-k or a defer.",
			`{"type":"object","properties":{`+img+`,"domain":{"type":"string","enum":["imagenet","birds","insects","plants"],"description":"label space (default imagenet)"},"top_k":{"type":"integer","description":"results to return (default 5)"}},"required":["image_path"]}`),
		laneTool(call, "offload_object_detect", "object_detect", "image_path",
			"Detect the 80 COCO object classes on the LOCAL Coral Edge TPU (EfficientDet-Lite0 320; size=lite1|lite2 for the larger inputs). Returns {objects:[{label,class_id,x,y,w,h,score}],count} in image pixels, sorted by score, or a defer.",
			`{"type":"object","properties":{`+img+`,"score_threshold":{"type":"number","description":"minimum score (default 0.3)"},"size":{"type":"string","enum":["lite0","lite1","lite2"],"description":"detector input size (default lite0 = 320 px)"}},"required":["image_path"]}`),
		laneTool(call, "offload_semantic_segment", "semantic_segment", "image_path",
			"SEMANTIC segmentation on the LOCAL Coral Edge TPU (DeepLabV3 MobileNet v2, 21 Pascal VOC classes) — a per-pixel class-id PNG, NOT instances (that is offload_segment on a Hailo box). Returns {mask_path,classes:[{class_id,label,pixels}],width,height} or a defer.",
			`{"type":"object","properties":{`+img+`,"out_path":{"type":"string","description":"output mask PNG (default: <name>.segmask.png)"}},"required":["image_path"]}`),
		laneTool(call, "offload_image_embed", "embed", "image_path",
			"1280-d IMAGE embedding on the LOCAL Coral Edge TPU (EfficientNet-EdgeTPU-S extractor) for image-to-image similarity and clustering. space=efficientnet-edgetpu-s — NOT a CLIP space, so there is no text tower to pair it with. Returns {embedding,dim,space} or a defer.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
	}
}

// laneToolsFor maps an accelerator id to its tool table. An unknown id has no
// table and registers nothing — the loop must never invent tools for a device
// it has no adapter for.
func laneToolsFor(id string, call NPUFunc) []Tool {
	switch id {
	case "hailo-8l":
		return npuTools(call)
	case "coral-edgetpu":
		return coralTools(call)
	}
	return nil
}

// accelLaneTools registers every wired lane's tools under the SHARED-NAME RULE
// (Coral D5): a capability name has exactly one owner per box, the FIRST listed
// accelerator that owns it. `have` is what is already registered; a lane's
// tool whose name is taken is skipped. Lanes come in config.Accelerators order,
// which is also the order hwdetect.DetectAllAccelerators emits and the order
// the MCP surface walks — so the two surfaces resolve a shared name identically.
func accelLaneTools(lanes []AccelLane, have []Tool) []Tool {
	taken := map[string]bool{}
	for _, t := range have {
		taken[t.Name] = true
	}
	var out []Tool
	for _, lane := range lanes {
		if lane.Call == nil {
			continue
		}
		for _, t := range laneToolsFor(lane.ID, lane.Call) {
			if taken[t.Name] {
				continue
			}
			taken[t.Name] = true
			if lane.Remote {
				// The seat must know the image is read on THIS box and shipped: a
				// path on the far node would fail here, the reverse of the local lane.
				t.Description += " [FLEET: this box has no " + lane.ID + " — forwarded to the fleet node that does; image_path is a file on THIS box and its bytes travel with the call (cap 8 MiB); result carries placement{node,wall_ms}]"
			}
			out = append(out, t)
		}
	}
	return out
}
