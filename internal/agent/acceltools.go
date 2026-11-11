package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	// Claims names the tools this lane serves ahead of the listed order
	// (config accelerator_tool_owners, ADR 0068). A claim on a tool the lane's
	// table lacks is ignored, so the first-listed rule still decides it.
	Claims []string
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

// rknpuTools is the Rockchip RK3588 NPU's tool table: three capabilities on models the
// sidecar verifies by sha256, names and schemas equal to the MCP rows. Every name is one the
// Coral and/or the Hailo lane also owns, so the shared-name rule in accelLaneTools decides who
// registers it on a box carrying more than one device.
func rknpuTools(call NPUFunc) []Tool {
	img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG)"}`
	return []Tool{
		laneTool(call, "offload_classify_image", "classify", "image_path",
			"Classify an image on the LOCAL Rockchip RK3588 NPU (free, on-box; 1000 ImageNet classes). Returns {results:[{label,score}],best,model,domain} top-k (default 5) or a defer.",
			`{"type":"object","properties":{`+img+`,"top_k":{"type":"integer","description":"results to return (default 5)"}},"required":["image_path"]}`),
		laneTool(call, "offload_object_detect", "object_detect", "image_path",
			"Detect the 80 COCO object classes on the LOCAL Rockchip RK3588 NPU (free, on-box). Returns {objects:[{label,class_id,x,y,w,h,score}],count} in image pixels, sorted by score, or a defer.",
			`{"type":"object","properties":{`+img+`,"score_threshold":{"type":"number","description":"minimum score (the sidecar's default when omitted)"}},"required":["image_path"]}`),
		laneTool(call, "offload_image_embed", "embed", "image_path",
			"IMAGE embedding on the LOCAL Rockchip RK3588 NPU (a CLIP-class image tower) for image-to-image similarity and clustering. Returns {embedding,dim,space,model} or a defer. space names the embedding space — NOT the Hailo's tinyclip and NOT the Coral's efficientnet-edgetpu-s, so vectors from different devices are never comparable, even at the same dimension. No text tower is served.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
	}
}

// axeraTools is the Axera AX650-class NPU's tool table: nine capabilities on an M.2 card reached
// through the loopback sidecar, names and schemas equal to the MCP rows. Most names are ones the
// other devices also own, so the shared-name rule in accelLaneTools decides who registers them on
// a box carrying more than one device; text_embed and zero_shot pair with THIS device's
// image_embed (siglip2-base-p16-224).
func axeraTools(call NPUFunc) []Tool {
	img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG)"}`
	return []Tool{
		laneTool(call, "offload_classify_image", "classify", "image_path",
			"Classify an image on the LOCAL Axera AX650 NPU (free, on-box; 1000 ImageNet classes by SigLIP2 zero-shot over the class names). Returns {results:[{label,score}],best,model,domain} top-k (default 5) or a defer.",
			`{"type":"object","properties":{`+img+`,"top_k":{"type":"integer","description":"results to return (default 5)"}},"required":["image_path"]}`),
		laneTool(call, "offload_object_detect", "object_detect", "image_path",
			"Detect the 80 COCO object classes (person, car, dog, laptop, ...) on the LOCAL Axera AX650 NPU (RF-DETR small, free, on-box). Returns {objects:[{label,class_id,x,y,w,h,score}],count} in image pixels, sorted by score or a defer.",
			`{"type":"object","properties":{`+img+`,"score_threshold":{"type":"number","description":"minimum score (default 0.5)"}},"required":["image_path"]}`),
		laneTool(call, "offload_semantic_segment", "semantic_segment", "image_path",
			"SEMANTIC segmentation on the LOCAL Axera AX650 NPU (DeepLabV3+ MobileNet, 21 Pascal VOC classes): a per-pixel class-id PNG, NOT instances (that is offload_segment). Returns {mask_path,classes:[{class_id,label,pixels}],width,height} or a defer.",
			`{"type":"object","properties":{`+img+`,"out_path":{"type":"string","description":"output mask PNG (default: <name>.segmask.png)"}},"required":["image_path"]}`),
		laneTool(call, "offload_segment", "segment", "image_path",
			"Instance SEGMENTATION on the LOCAL Axera AX650 NPU: RF-DETR boxes for the 80 COCO classes, each refined to a mask by MobileSAM; everything=true gives class-agnostic masks from a point grid. Writes an instance-id mask PNG (uint8: 0=background, i=instances[i-1]) next to the input or to out_path. Returns {instances:[{label,class_id,x,y,w,h,score}],mask_path,count} or a defer.",
			`{"type":"object","properties":{`+img+`,"everything":{"type":"boolean","description":"true = class-agnostic masks from a point grid (default false = 80 COCO classes)"},"score_threshold":{"type":"number","description":"minimum instance score (default 0.5)"},"out_path":{"type":"string","description":"output mask PNG (default: <name>.mask.png)"}},"required":["image_path"]}`),
		laneTool(call, "offload_image_embed", "embed", "image_path",
			"768-d IMAGE embedding on the LOCAL Axera AX650 NPU (SigLIP2 base) for similarity search and clustering. Returns {embedding,dim,space,model}; space=siglip2-base-p16-224 pairs with this device's offload_text_embed and offload_zero_shot, and vectors from other devices are never comparable.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
		laneTool(call, "offload_text_embed", "text_embed", "text",
			"TEXT embedding on the LOCAL Axera AX650 NPU in the SAME 768-d space as this device's offload_image_embed (SigLIP2 base): text-to-image search over frames with zero cloud. Returns {embedding,dim,space,model} or a defer.",
			`{"type":"object","properties":{"text":{"type":"string","description":"the text to embed"}},"required":["text"]}`),
		laneTool(call, "offload_zero_shot", "zero_shot", "image_path",
			"ZERO-SHOT image classification on the LOCAL Axera AX650 NPU: score an image against free-text labels, both SigLIP2 towers on the NPU. Returns {results:[{label,similarity,prob}],best} ranked best-first or a defer.",
			`{"type":"object","properties":{`+img+`,"labels":{"type":"array","items":{"type":"string"},"description":"candidate labels, free text"},"template":{"type":"string","description":"prompt template (default \"a photo of {}.\")"}},"required":["image_path","labels"]}`),
		laneTool(call, "offload_depth", "depth", "image_path",
			"Relative DEPTH map on the LOCAL Axera AX650 NPU (Depth Anything 3 small). Writes an 8-bit PNG (bright = near) at the input's resolution. Returns {depth_path,min,max,mean,model} or a defer.",
			`{"type":"object","properties":{`+img+`,"out_path":{"type":"string","description":"output PNG (default: <name>.depth.png)"}},"required":["image_path"]}`),
		laneTool(call, "offload_pose", "pose", "image_path",
			"Human POSE estimation on the LOCAL Axera AX650 NPU (RF-DETR person boxes, then RTMPose-M): people with 17 named COCO keypoints each, in the image's own pixel space. Returns {people:[{x,y,w,h,score,keypoints:{nose:{x,y,score},...}}],count} or a defer.",
			`{"type":"object","properties":{`+img+`,"score_threshold":{"type":"number","description":"minimum person score (default 0.5)"}},"required":["image_path"]}`),
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
	case "rknpu":
		return rknpuTools(call)
	case "axera-ax650":
		return axeraTools(call)
	}
	return nil
}

// accelLaneTools registers every wired lane's tools under the SHARED-NAME RULE
// (Coral D5): a capability name has exactly one owner per box, the FIRST listed
// accelerator that owns it. `have` is what is already registered; a lane's
// tool whose name is taken is skipped. Lanes come in config.Accelerators order,
// which is also the order hwdetect.DetectAllAccelerators emits and the order
// the MCP surface walks — so the two surfaces resolve a shared name identically.
// A lane's Claims (accelerator_tool_owners, ADR 0068) take their names first,
// the same override mcpserver.accelOwnerPlan applies. The owner is the first
// choice, not the only one: every other lane that serves the name answers when
// the owner cannot (AccelFallback, ADR 0102).
func accelLaneTools(lanes []AccelLane, have []Tool) []Tool {
	taken := map[string]bool{}
	for _, t := range have {
		taken[t.Name] = true
	}
	// A claim takes the name before the walk, as accelOwnerPlan does on the MCP
	// surface; the first lane to claim a name keeps it.
	claimedBy := map[string]string{}
	for _, lane := range lanes {
		if lane.Call == nil {
			continue
		}
		tools := laneToolsFor(lane.ID, lane.Call)
		for _, name := range lane.Claims {
			has := slices.ContainsFunc(tools, func(t Tool) bool { return t.Name == name })
			if _, dup := claimedBy[name]; has && !dup && !taken[name] {
				claimedBy[name] = lane.ID
			}
		}
	}
	var out []Tool
	ownerOf := map[string]string{} // tool name -> the lane that registered it here
	for _, lane := range lanes {
		if lane.Call == nil {
			continue
		}
		for _, t := range laneToolsFor(lane.ID, lane.Call) {
			if owner, claimed := claimedBy[t.Name]; taken[t.Name] || (claimed && owner != lane.ID) {
				continue
			}
			taken[t.Name] = true
			ownerOf[t.Name] = lane.ID
			if lane.Remote {
				// The seat must know the image is read on THIS box and shipped: a
				// path on the far node would fail here, the reverse of the local lane.
				t.Description += " [FLEET: this box has no " + lane.ID + " — forwarded to the fleet node that does; image_path is a file on THIS box and its bytes travel with the call (cap 8 MiB); result carries placement{node,wall_ms}]"
			}
			out = append(out, t)
		}
	}
	// Every other lane that serves a registered name answers when the owner cannot (ADR 0102),
	// in lane order: the owner is the first choice, never the only one.
	steps := map[string][]AccelStep{}
	for _, lane := range lanes {
		if lane.Call == nil {
			continue
		}
		for _, t := range laneToolsFor(lane.ID, lane.Call) {
			owner, registered := ownerOf[t.Name]
			if !registered || owner == lane.ID || slices.ContainsFunc(steps[t.Name], func(s AccelStep) bool { return s.Device == lane.ID }) {
				continue
			}
			exec := t.Exec
			steps[t.Name] = append(steps[t.Name], AccelStep{Device: lane.ID, Props: AccelSchemaProps(t.Schema),
				Run: func(ctx context.Context, args map[string]any) map[string]any {
					b, _ := json.Marshal(args)
					return accelResult(exec(ctx, string(b)))
				}})
		}
	}
	for i := range out {
		chain := steps[out[i].Name]
		if len(chain) == 0 {
			continue
		}
		desc, schema := AccelFallbackSurface(out[i].Name, out[i].Description, string(out[i].Schema), true)
		out[i].Description, out[i].Schema = desc, json.RawMessage(schema)
		name, owner, exec := out[i].Name, ownerOf[out[i].Name], out[i].Exec
		out[i].Exec = func(ctx context.Context, args string) (string, error) {
			var in map[string]any
			_ = json.Unmarshal([]byte(args), &in)
			mine := args
			if _, said := in[AnySpaceArg]; said {
				// The caller's word to this loop, never a sidecar argument.
				sub := map[string]any{}
				for k, v := range in {
					if k != AnySpaceArg {
						sub[k] = v
					}
				}
				b, _ := json.Marshal(sub)
				mine = string(b)
			}
			res, err := exec(ctx, mine)
			if err != nil {
				return res, err
			}
			got := accelResult(res, nil)
			if _, cannot := AccelCannotServe(got); !cannot {
				return res, nil // the owner answered: its bytes, untouched
			}
			b, merr := json.Marshal(AccelFallback(ctx, name, owner, got, in, chain))
			if merr != nil {
				return res, nil
			}
			return string(b), nil
		}
	}
	return out
}

// accelResult reads one lane tool's answer as a dict. An error or an answer that is not a JSON
// object is a defer, which is how every lane already reports a call it could not make.
func accelResult(res string, err error) map[string]any {
	if err != nil {
		return map[string]any{"deferred": true, "reason": err.Error()}
	}
	var m map[string]any
	if json.Unmarshal([]byte(res), &m) != nil || m == nil {
		return map[string]any{"deferred": true, "reason": "the device returned a non-object result"}
	}
	return m
}
