package mcpserver

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/accelclient"
	"github.com/dmmdea/offload-harness/internal/accelremote"
	"github.com/dmmdea/offload-harness/internal/config"
)

// accelMCPTool is one row of an accelerator's MCP tool table: the advertised
// name, the sidecar tool it maps to 1:1, the one argument it cannot run
// without, and its description/schema. Tables are per device (Coral D5); the
// registration loop in NewServer walks config.Accelerators in order and applies
// the shared-name rule — a capability name has exactly one owner per box, the
// FIRST listed accelerator that owns it.
type accelMCPTool struct {
	name, sidecar, arg, desc, schema string
}

// accelMCPTools returns the tool table for an accelerator id, or nil for a
// device this build has no adapter for (it then registers nothing — the
// server must never invent tools for a device it cannot drive).
func accelMCPTools(id string) []accelMCPTool {
	img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG)"}`
	switch id {
	case "hailo-8l":
		// requiredArg: image_path for the vision tools, text for the text tower.
		return []accelMCPTool{
			{"offload_face_detect", "face_detect", "image_path", "Detect faces in an image on the LOCAL Hailo-8L NPU (free, on-box, ~300 FPS). Returns {faces:[{x,y,w,h,score,kps}],count}; kps = 5 landmarks (eyes, nose, mouth corners) in image pixels.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
			{"offload_face_embed", "face_embed", "image_path", "Face IDENTITY vectors on the LOCAL Hailo-8L NPU: every face -> a 512-d ArcFace embedding. Cosine similarity between two is the identity score (same person ~0.5+, different ~0.3-). Use to cluster who appears where across a project, no cloud. Returns {faces:[{x,y,w,h,score,kps,embedding}],count}.", `{"type":"object","properties":{` + img + `,"max_faces":{"type":"integer","description":"strongest-score faces to embed (default 16)"}},"required":["image_path"]}`},
			{"offload_object_detect", "object_detect", "image_path", "Detect the 80 COCO object classes (person, car, dog, laptop, ...) on the LOCAL Hailo-8L NPU (YOLOv8s, on-chip NMS). Returns {objects:[{label,class_id,x,y,w,h,score}],count} sorted by score.", `{"type":"object","properties":{` + img + `,"score_threshold":{"type":"number","description":"minimum score (default 0.3)"}},"required":["image_path"]}`},
			{"offload_person_embed", "person_embed", "image_path", "Person RE-IDENTIFICATION vectors on the LOCAL Hailo-8L NPU (YOLOv8s person boxes -> OSNet 512-d). Works with NO visible face (clothing/body) — tracks the same person across shots. Returns {people:[{x,y,w,h,score,embedding}],count}.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
			{"offload_depth", "depth", "image_path", "Preview-grade relative depth map on the LOCAL Hailo-8L NPU (Depth-Anything-V2, 224 px). Writes an 8-bit PNG (bright = near). Returns {depth_path,min,max,mean}. Shot analysis / parallax previews, not a production depth pass.", `{"type":"object","properties":{` + img + `,"out_path":{"type":"string","description":"output PNG (default: next to the input as <name>.depth.png)"}},"required":["image_path"]}`},
			{"offload_enhance_low_light", "enhance_low_light", "image_path", "Brighten an under-exposed frame on the LOCAL Hailo-8L NPU (Zero-DCE) at the original resolution. Returns {enhanced_path,width,height}. Preview-grade.", `{"type":"object","properties":{` + img + `,"out_path":{"type":"string","description":"output PNG (default: <name>.enhanced.png)"}},"required":["image_path"]}`},
			{"offload_image_embed", "embed", "image_path", "512-d IMAGE embedding on the LOCAL Hailo-8L NPU (TinyCLIP ViT-61M) for similarity search / clustering of frames and thumbnails. Returns {embedding,dim}.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
			{"offload_pose", "pose", "image_path", "Human POSE estimation on the LOCAL Hailo-8L NPU (YOLOv8s-pose): people with 17 named COCO keypoints each (nose, eyes, shoulders, ..., ankles), in the image's own pixel space. Returns {people:[{x,y,w,h,score,keypoints:{nose:{x,y,score},...}}],count}.", `{"type":"object","properties":{` + img + `,"score_threshold":{"type":"number","description":"minimum person score (default 0.3)"}},"required":["image_path"]}`},
			{"offload_segment", "segment", "image_path", "Instance SEGMENTATION on the LOCAL Hailo-8L NPU. Default: YOLOv8s-seg, 80 COCO classes; everything=true: FastSAM (class-agnostic segment-everything). Writes an instance-id mask PNG (uint8: 0=background, i=instances[i-1]) next to the input or to out_path. Returns {instances:[{label,class_id,x,y,w,h,score}],mask_path,count}.", `{"type":"object","properties":{` + img + `,"everything":{"type":"boolean","description":"true = FastSAM class-agnostic masks (default false = 80 COCO classes)"},"score_threshold":{"type":"number","description":"minimum instance score (default 0.25)"},"out_path":{"type":"string","description":"output mask PNG (default: <name>.mask.png)"}},"required":["image_path"]}`},
			{"offload_text_embed", "text_embed", "text", "TEXT embedding computed ON the LOCAL Hailo-8L NPU, in the SAME space as offload_image_embed's image vectors (space=tinyclip, 512-d, the default) — text-to-image similarity search over frames with zero cloud. space=siglip2 (768-d) pairs only with the siglip2 image side used by offload_zero_shot. Returns {embedding,dim,space}.", `{"type":"object","properties":{"text":{"type":"string","description":"the text to embed"},"space":{"type":"string","enum":["tinyclip","siglip2"],"description":"embedding space (default tinyclip = matches offload_image_embed)"}},"required":["text"]}`},
			{"offload_zero_shot", "zero_shot", "image_path", "ZERO-SHOT image classification on the LOCAL Hailo-8L NPU — score an image against free-text labels, BOTH towers (image + text) on the NPU. space=tinyclip (default) or siglip2 (stronger: 73.0 vs 67.8 top-1). Returns {results:[{label,similarity,prob}],best} ranked best-first.", `{"type":"object","properties":{` + img + `,"labels":{"type":"array","items":{"type":"string"},"description":"candidate labels, free text"},"space":{"type":"string","enum":["tinyclip","siglip2"],"description":"model pair (default tinyclip)"},"template":{"type":"string","description":"prompt template (default \"a photo of a {}\")"}},"required":["image_path","labels"]}`},
		}
	case "coral-edgetpu":
		// Four capabilities (Coral D5), each on an artifact the sidecar verifies by
		// sha256. offload_object_detect and offload_image_embed are capability names
		// the Hailo lane also owns — the shared-name rule decides on a box with both.
		return []accelMCPTool{
			{"offload_classify_image", "classify", "image_path", "Classify an image on the LOCAL Coral Edge TPU (free, on-box, ~3 ms). domain=imagenet (default; EfficientNet-EdgeTPU-S, 1000 ImageNet classes) or birds|insects|plants (MobileNet v2 iNat). Returns {results:[{label,score}],best,model,domain} top-k (default 5).", `{"type":"object","properties":{` + img + `,"domain":{"type":"string","enum":["imagenet","birds","insects","plants"],"description":"label space (default imagenet)"},"top_k":{"type":"integer","description":"results to return (default 5)"}},"required":["image_path"]}`},
			{"offload_object_detect", "object_detect", "image_path", "Detect the 80 COCO object classes (person, car, dog, laptop, ...) on the LOCAL Coral Edge TPU (EfficientDet-Lite0 320; size=lite1|lite2 for the larger inputs). Returns {objects:[{label,class_id,x,y,w,h,score}],count} in image pixels, sorted by score.", `{"type":"object","properties":{` + img + `,"score_threshold":{"type":"number","description":"minimum score (default 0.3)"},"size":{"type":"string","enum":["lite0","lite1","lite2"],"description":"detector input size (default lite0 = 320 px)"}},"required":["image_path"]}`},
			{"offload_semantic_segment", "semantic_segment", "image_path", "SEMANTIC segmentation on the LOCAL Coral Edge TPU (DeepLabV3 MobileNet v2, 21 Pascal VOC classes): a per-pixel class-id PNG — NOT instances (that is offload_segment on a Hailo box; the two outputs are not interchangeable). Returns {mask_path,classes:[{class_id,label,pixels}],width,height}.", `{"type":"object","properties":{` + img + `,"out_path":{"type":"string","description":"output mask PNG (default: <name>.segmask.png)"}},"required":["image_path"]}`},
			{"offload_image_embed", "embed", "image_path", "1280-d IMAGE embedding on the LOCAL Coral Edge TPU (EfficientNet-EdgeTPU-S extractor) for image-to-image similarity search and clustering. space=efficientnet-edgetpu-s — NOT a CLIP space: there is no text tower to pair it with (no offload_text_embed / offload_zero_shot on this device). Returns {embedding,dim,space}.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
		}
	}
	return nil
}

// accelOwns is the capability list status reports per device — mirrors
// profiles.json `accelerators.<id>.owns`.
func accelOwns(id string) []string {
	switch id {
	case "hailo-8l":
		return []string{"face_detect", "face_embed", "object_detect", "person_embed", "depth", "enhance_low_light", "image_embed", "pose", "segment", "text_embed", "zero_shot"}
	case "coral-edgetpu":
		return []string{"classify_image", "object_detect", "semantic_segment", "image_embed"}
	}
	return nil
}

// accelLaneConfig is one accelerator's sidecar parameters as config names
// them — the same table pipeline/loopaccel.go keeps for the loop, so the MCP
// surface and the agent loop drive one sidecar per device identically.
type accelLaneConfig struct {
	endpoint string
	cmd      string
	timeout  time.Duration
	idleSec  int
	note     string
}

func accelLaneConfigFor(cfg config.Config, id string) (accelLaneConfig, bool) {
	switch id {
	case "hailo-8l":
		t := time.Duration(cfg.HailoTimeoutSec) * time.Second
		if t <= 0 {
			t = 60 * time.Second
		}
		return accelLaneConfig{cfg.HailoEndpoint, cfg.HailoSidecarCmd, t, cfg.HailoIdleSec,
			"on-demand loopback sidecar; the first NPU call starts it (cold ~2 s + HEF load), it exits itself after hailo_idle_sec idle"}, true
	case "coral-edgetpu":
		t := time.Duration(cfg.CoralTimeoutSec) * time.Second
		if t <= 0 {
			t = 30 * time.Second
		}
		return accelLaneConfig{cfg.CoralEndpoint, cfg.CoralSidecarCmd, t, cfg.CoralIdleSec,
			"on-demand loopback sidecar (accelerators/coral/server.py); the first call starts it (cold ~0.5-2 s model load on the TPU), it exits itself after coral_idle_sec idle; reads sysfs temp/status, never writes it"}, true
	}
	return accelLaneConfig{}, false
}

// accelSidecar lazily builds the lane for an accelerator id: one client, one
// spawn function (nil when its *_sidecar_cmd is unset), one Sidecar per device
// shared by every tool on it so concurrent first calls share a single spawn.
func (s *Server) accelSidecar(id string) *accelclient.Sidecar {
	s.accelMu.Lock()
	defer s.accelMu.Unlock()
	if s.accel == nil {
		s.accel = map[string]*accelclient.Sidecar{}
	}
	if sc, ok := s.accel[id]; ok {
		return sc
	}
	lc, ok := accelLaneConfigFor(s.p.Cfg(), id)
	if !ok {
		// No adapter for this id: a Sidecar that can neither reach nor spawn
		// anything, so every call defers with ErrNoSidecarCmd rather than
		// panicking on a nil.
		sc := accelclient.NewSidecar(accelclient.NewDevice(id, "http://127.0.0.1:0", time.Second), nil, time.Second)
		s.accel[id] = sc
		return sc
	}
	var spawn func() error
	if lc.cmd != "" {
		spawn = accelclient.SpawnCmd(lc.cmd, lc.idleSec)
	}
	sc := accelclient.NewSidecar(accelclient.NewDevice(id, lc.endpoint, lc.timeout), spawn, 45*time.Second)
	s.accel[id] = sc
	return sc
}

// accelCall is the one path every accelerator tool takes: ensure the device's
// sidecar, call the tool, pass the dict through. Transport/spawn failures
// become defers prefixed with the device id (the caller does the work another
// way); the sidecar's own structured refusals pass through untouched — they
// are results.
func (s *Server) accelCall(ctx context.Context, id, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	sc := s.accelSidecar(id)
	if err := sc.Ensure(ctx); err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": id + ": " + err.Error()})
	}
	out, err := sc.Client().Call(ctx, tool, args)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": id + ": " + err.Error()})
	}
	return jsonResult(out)
}

// handleAccelTool adapts one accelerator tool: parse args, refuse an empty
// required argument as a defer, pass the rest through to the device's sidecar.
func (s *Server) handleAccelTool(id, tool, requiredArg string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in map[string]any
		if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
			return bad, nil
		}
		if in[requiredArg] == nil || in[requiredArg] == "" {
			return jsonResult(map[string]any{"deferred": true, "reason": "empty " + requiredArg})
		}
		return s.accelCall(ctx, id, tool, in)
	}
}

// registerAccelTools walks config.Accelerators IN ORDER and registers each
// device's table under the shared-name rule (Coral D5): the first listed owner
// of a name registers it; a later device's same-named tool is skipped and
// logged once. Today no box lists both devices, so the rule is a tested
// invariant rather than a live path. Returns the owner map for tests.
func (s *Server) registerAccelTools(srv *mcp.Server, cfg config.Config) map[string]string {
	owner := map[string]string{}
	for _, id := range cfg.Accelerators {
		for _, t := range accelMCPTools(id) {
			if first, dup := owner[t.name]; dup {
				log.Printf("accelerator %s: %s is already served by %s on this box; skipped (shared-name rule, docs/systems/accelerators.md)", id, t.name, first)
				continue
			}
			owner[t.name] = id
			srv.AddTool(&mcp.Tool{Name: t.name, Description: t.desc, InputSchema: json.RawMessage(t.schema)}, s.handleAccelTool(id, t.sidecar, t.arg))
		}
	}
	// Fleet devices (Coral Phase B, 0.115.0): an id this box does NOT carry but
	// lists in fleet_accelerators registers the same table, forwarding through
	// accelremote to the first delegate_remotes node whose health lists it. The
	// walk is AFTER the local devices, so a local device wins a shared name.
	for _, id := range cfg.FleetAccelerators {
		if slices.Contains(cfg.Accelerators, id) {
			continue // the device is here; the local lane above already owns its names
		}
		for _, t := range accelMCPTools(id) {
			if first, dup := owner[t.name]; dup {
				log.Printf("fleet accelerator %s: %s is already served by %s on this box; skipped (shared-name rule)", id, t.name, first)
				continue
			}
			owner[t.name] = id + FleetOwnerSuffix
			desc := t.desc + " [FLEET: this box has no " + id + " — the call is forwarded to the fleet node that does; image_path is read HERE and its bytes travel with the job (cap 8 MiB); the result carries placement{node,wall_ms}]"
			srv.AddTool(&mcp.Tool{Name: t.name, Description: desc, InputSchema: json.RawMessage(t.schema)}, s.handleFleetAccelTool(id, t.sidecar, t.arg))
		}
	}
	return owner
}

// FleetOwnerSuffix marks a forwarded owner in registerAccelTools' owner map.
const FleetOwnerSuffix = "@fleet"

// handleFleetAccelTool adapts one forwarded accelerator tool: parse args,
// refuse an empty required argument as a defer, hand the rest to accelremote.
// A forwarding failure is a device-prefixed defer — the caller does the work
// another way — and the node's own result passes through untouched.
func (s *Server) handleFleetAccelTool(id, tool, requiredArg string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in map[string]any
		if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
			return bad, nil
		}
		if in[requiredArg] == nil || in[requiredArg] == "" {
			return jsonResult(map[string]any{"deferred": true, "reason": "empty " + requiredArg})
		}
		out, err := accelremote.Call(ctx, s.p.Cfg(), id, tool, in)
		if err != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": id + " (fleet): " + err.Error()})
		}
		return jsonResult(out)
	}
}

// accelStatus is the status block's `accelerators` entry: one row per listed
// device with its lane config, owned capabilities and a quick health probe
// that NEVER spawns the sidecar — status must stay side-effect free.
func accelStatus(ctx context.Context, cfg config.Config) map[string]any {
	if len(cfg.Accelerators) == 0 && len(cfg.FleetAccelerators) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, id := range cfg.FleetAccelerators {
		if slices.Contains(cfg.Accelerators, id) {
			continue
		}
		out[id] = map[string]any{"owns": accelOwns(id), "fleet": true,
			"note": "not on this box: its tools forward to the first delegate_remotes node whose /fleet/health lists it (accelremote, cap 8 MiB per image)"}
	}
	for _, id := range cfg.Accelerators {
		lc, ok := accelLaneConfigFor(cfg, id)
		entry := map[string]any{"owns": accelOwns(id)}
		if !ok {
			entry["note"] = "listed in accelerators but this build has no adapter for it — nothing is registered"
			out[id] = entry
			continue
		}
		entry["endpoint"] = lc.endpoint
		entry["sidecar_cmd_configured"] = lc.cmd != ""
		entry["note"] = lc.note
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if h, err := accelclient.NewDevice(id, lc.endpoint, 2*time.Second).Health(pctx); err != nil {
			entry["health_error"] = err.Error() + " (not running — normal between uses)"
		} else {
			entry["health"] = h
		}
		cancel()
		out[id] = entry
	}
	return out
}

// accelSidecars is the server's per-device sidecar map plus its guard; declared
// here beside the code that uses it and embedded into Server by mcpserver.go.
type accelSidecars struct {
	accelMu sync.Mutex
	accel   map[string]*accelclient.Sidecar
}
