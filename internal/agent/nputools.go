package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// NPUFunc is the in-process accelerator capability injected by the loop's
// builder (wired to the Hailo sidecar via pipeline.NewLoopNPU). Mirrors
// OffloadFunc: keeping it an injected func keeps this package free of any
// hailoclient import and unit-testable with a fake. nil => no NPU tools, and
// the loop's advertised tool list stays byte-identical to a box without the
// device — the same registration pin as the MCP surface (ADR 0024).
//
// The func returns a STRING result (JSON of the sidecar's dict) and follows
// defer-not-crash: transport/spawn failures come back as
// {"deferred":true,...} strings with a nil error, never tool errors, so the
// loop degrades gracefully instead of aborting the step.
type NPUFunc func(ctx context.Context, tool string, args map[string]any) (string, error)

// npuTools builds the accelerator tool set — registered ONLY when an NPUFunc
// is wired. Each tool maps 1:1 to a sidecar tool; the argument names are the
// sidecar's own keyword names, so the JSON passes through unchanged (the same
// adapter shape as mcpserver.handleHailoTool).
func npuTools(npu NPUFunc) []Tool {
	mk := func(name, sidecar, desc, schema string) Tool {
		return Tool{
			ToolSpec: ToolSpec{Name: name, Description: desc, Schema: json.RawMessage(schema)},
			Exec: func(ctx context.Context, args string) (string, error) {
				var in map[string]any
				if err := json.Unmarshal([]byte(args), &in); err != nil {
					return "", err
				}
				raw, present := in["image_path"]
				s, isStr := raw.(string)
				if present && !isStr {
					// A wrong TYPE must not be reported as "empty" — that sends
					// the caller debugging a defect that did not occur.
					b, _ := json.Marshal(map[string]any{"deferred": true,
						"reason": fmt.Sprintf("image_path must be a string, got %T", raw)})
					return string(b), nil
				}
				if strings.TrimSpace(s) == "" {
					return `{"deferred":true,"reason":"empty image_path"}`, nil
				}
				return npu(ctx, sidecar, in)
			},
		}
	}
	img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG)"}`
	return []Tool{
		mk("offload_face_detect", "face_detect",
			"Detect faces in an image on the LOCAL Hailo-8L NPU (free, on-box), without the image entering the agent's context. Returns {faces:[{x,y,w,h,score,kps}],count} or a defer; kps = 5 landmarks in image pixels.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
		mk("offload_face_embed", "face_embed",
			"Face IDENTITY vectors on the LOCAL Hailo-8L NPU: every face -> a 512-d ArcFace embedding (cosine similarity between two is the identity score; same person ~0.5+, different ~0.3-). Returns {faces:[{x,y,w,h,score,kps,embedding}],count} or a defer.",
			`{"type":"object","properties":{`+img+`,"max_faces":{"type":"integer","description":"strongest-score faces to embed (default 16)"}},"required":["image_path"]}`),
		mk("offload_object_detect", "object_detect",
			"Detect the 80 COCO object classes (person, car, dog, laptop, ...) on the LOCAL Hailo-8L NPU (YOLOv8s, on-chip NMS). Returns {objects:[{label,class_id,x,y,w,h,score}],count} sorted by score, or a defer.",
			`{"type":"object","properties":{`+img+`,"score_threshold":{"type":"number","description":"minimum score (default 0.3)"}},"required":["image_path"]}`),
		mk("offload_person_embed", "person_embed",
			"Person RE-IDENTIFICATION vectors on the LOCAL Hailo-8L NPU (person boxes -> 512-d OSNet). Works with NO visible face — tracks the same person across shots. Returns {people:[{x,y,w,h,score,embedding}],count} or a defer.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
		mk("offload_depth", "depth",
			"Preview-grade relative depth map on the LOCAL Hailo-8L NPU. Writes an 8-bit PNG (bright = near). Returns {depth_path,min,max,mean} or a defer.",
			`{"type":"object","properties":{`+img+`,"out_path":{"type":"string","description":"output PNG (default: next to the input as <name>.depth.png)"}},"required":["image_path"]}`),
		mk("offload_enhance_low_light", "enhance_low_light",
			"Brighten an under-exposed frame on the LOCAL Hailo-8L NPU (Zero-DCE) at the original resolution. Returns {enhanced_path,width,height} or a defer.",
			`{"type":"object","properties":{`+img+`,"out_path":{"type":"string","description":"output PNG (default: <name>.enhanced.png)"}},"required":["image_path"]}`),
		mk("offload_image_embed", "embed",
			"512-d IMAGE embedding on the LOCAL Hailo-8L NPU (TinyCLIP) for similarity search / clustering of frames and thumbnails. Returns {embedding,dim} or a defer.",
			`{"type":"object","properties":{`+img+`},"required":["image_path"]}`),
	}
}
