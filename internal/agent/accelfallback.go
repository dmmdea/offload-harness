package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Call-time fallback for a shared accelerator tool (ADR 0102).
//
// A shared tool name has one owner per box (ADR 0037, ADR 0068): that device's description,
// schema and model are what the caller sees. It used to be the only device that could ever
// answer as well, so a tool whose owner's node was off deferred while two other devices that
// serve the same name sat idle. The owner is now the FIRST choice: when it cannot take
// the call, the call runs on the next device that serves the name, in the listed order, and
// the result says so. Both surfaces (the MCP tools and the agent loop) use this one function,
// at the place a tool is registered. A node running a forwarded job never comes through here
// (pipeline.runAccelTask calls its local lane directly), so a call cannot forward twice.

// AccelStep is one more device that serves a shared tool name: how to run the tool there.
type AccelStep struct {
	// Device is the accelerator id.
	Device string
	// Props are the argument names this device's tool takes. The caller sent the owner's
	// arguments; only these travel to this device.
	Props []string
	// Run calls the tool on this device and returns its dict (a defer included).
	Run func(ctx context.Context, args map[string]any) map[string]any
}

// AnySpaceArg is the caller's opt-in for an embedding from another device (see AccelFallback).
const AnySpaceArg = "any_space"

// inputRefusals are the sidecars' refusals of what the caller SENT. Another device would refuse
// the same input, so they are answers, not a reason to try one.
var inputRefusals = []string{"bad_request", "image_missing", "image_unreadable", "out_path_exists",
	"out_path_unwritable", "forbidden", "not_found", "unknown_model"}

// AccelCannotServe says whether out means the device could not take the call, and why: a defer
// (its node is off, its sidecar did not start, the dispatch was refused), or a sidecar refusal
// that is about the device (a model that is missing or failed to load, a disabled or absent
// NPU, an internal error) and not about the caller's input.
func AccelCannotServe(out map[string]any) (reason string, cannot bool) {
	if d, _ := out["deferred"].(bool); d {
		reason, _ = out["reason"].(string)
		return reason, true
	}
	kind, _ := out["error"].(string)
	if kind == "" || slices.Contains(inputRefusals, kind) {
		return "", false
	}
	if detail, _ := out["detail"].(string); detail != "" {
		return kind + ": " + detail, true
	}
	return kind, true
}

// AccelSpaceBound says whether a tool's answer only means something beside other answers from
// the SAME device: an embedding. A vector from another device is in another space, and nothing
// in the number itself says so.
func AccelSpaceBound(tool string) bool { return strings.HasSuffix(tool, "_embed") }

// AccelFallback is called with the owner's answer when AccelCannotServe says the owner could not
// take the call. It runs the tool on each later device in order and returns the first answer a
// device gives, with a `fallback` block naming the owner, why it could not serve, and the device
// that did. When no device serves it, the owner's answer comes back with `fallback_tried`.
//
// An embedding does not move silently: the caller must pass any_space:true, because a vector
// from another device cannot be compared with the owner's. Without it the owner's answer comes
// back with `fallback_withheld` naming the devices that could have answered.
func AccelFallback(ctx context.Context, tool, owner string, out, args map[string]any, steps []AccelStep) map[string]any {
	reason, cannot := AccelCannotServe(out)
	if !cannot || len(steps) == 0 {
		return out
	}
	if any, _ := args[AnySpaceArg].(bool); AccelSpaceBound(tool) && !any {
		names := make([]string, len(steps))
		for i, s := range steps {
			names[i] = s.Device
		}
		out["fallback_withheld"] = "not run on " + strings.Join(names, " or ") + ": an embedding from another device is in a different space and cannot be compared with " + owner + "'s; pass " + AnySpaceArg + ":true to take one anyway (the result's space names it)"
		return out
	}
	if why := outPathRefusal(args); why != "" {
		out["fallback_withheld"] = why
		return out
	}
	var tried []map[string]string
	for _, s := range steps {
		if ctx.Err() != nil {
			break
		}
		sub := map[string]any{}
		for k, v := range args {
			if slices.Contains(s.Props, k) {
				sub[k] = v
			}
		}
		got := s.Run(ctx, sub)
		if got == nil {
			got = map[string]any{"deferred": true, "reason": s.Device + ": empty answer"}
		}
		if why, failed := AccelCannotServe(got); failed {
			tried = append(tried, map[string]string{"device": s.Device, "reason": why})
			continue
		}
		got["fallback"] = map[string]string{"from": owner, "reason": reason, "served_by": s.Device}
		return got
	}
	out["fallback_tried"] = tried
	return out
}

// outPathRefusal says why a call's out_path must not travel to a fallback device, or "" when it
// may. The owner applied its own rule to out_path before it answered (the Axera sidecar's, or
// accelremote's landing for a fleet device); a fallback device's sidecar may apply none, and a
// model seat can pass any path. So the rule every guarded writer of this lane uses is applied
// here, before any device runs: out_path is never the input image, its directory must exist,
// and an existing target is replaced only when it is a regular file that is already a PNG.
func outPathRefusal(args map[string]any) string {
	out, _ := args["out_path"].(string)
	if out = strings.TrimSpace(out); out == "" {
		return ""
	}
	if img, _ := args["image_path"].(string); strings.TrimSpace(img) != "" {
		same := filepath.Clean(out) == filepath.Clean(img)
		if a, ea := os.Stat(out); !same && ea == nil {
			if b, eb := os.Stat(img); eb == nil {
				same = os.SameFile(a, b)
			}
		}
		if same {
			return "not run on another device: out_path " + out + " is the input image and is never replaced"
		}
	}
	if fi, err := os.Stat(filepath.Dir(out)); err != nil || !fi.IsDir() {
		return "not run on another device: the directory of out_path " + out + " does not exist"
	}
	fi, err := os.Lstat(out)
	if err != nil {
		return "" // a new file
	}
	if fi.Mode().IsRegular() {
		if f, oerr := os.Open(out); oerr == nil {
			head := make([]byte, 8)
			n, _ := io.ReadFull(f, head)
			f.Close()
			if n == 8 && string(head) == "\x89PNG\r\n\x1a\n" {
				return "" // an earlier output
			}
		}
	}
	return "not run on another device: out_path " + out + " exists and is not a PNG file; it is never replaced"
}

// AccelSchemaProps returns the argument names a tool's JSON schema declares.
func AccelSchemaProps(schema []byte) []string {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil
	}
	props := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		props = append(props, k)
	}
	slices.Sort(props)
	return props
}

// The sentences a tool's description gains when another device can answer in its owner's place.
// They name no device, so one sentence serves every box (the compact tools profile keeps it by
// exact text); offload_status.accelerators.<id>.fallback_for names the devices.
const (
	AccelFallbackNote      = "FALLBACK: when this device cannot take the call, the next device that serves this tool answers with its own model (arguments it does not take are dropped), and the result's fallback{from,reason,served_by} names it."
	AccelFallbackSpaceNote = "FALLBACK only with " + AnySpaceArg + ":true: when this device cannot take the call, another device that serves this tool answers in a DIFFERENT embedding space, and the result's space and fallback{from,reason,served_by} name it."
	anySpaceProp           = `,"` + AnySpaceArg + `":{"type":"boolean","description":"true = when this device cannot take the call, accept another device's vector, in ANOTHER embedding space that cannot be compared with this one's (default false: the call defers)"}`
)

// AccelFallbackSurface returns the owner's description and schema as a caller should see them
// when another device can answer in the owner's place (fallback). Both are extended as text,
// never re-encoded, and a tool with no other device is returned unchanged, so a box with one
// device advertises exactly what it always did. The sentence goes before a trailing
// "[FLEET: ...]" forwarding note, which stays the description's last sentence.
func AccelFallbackSurface(tool, desc, schema string, fallback bool) (string, string) {
	if !fallback {
		return desc, schema
	}
	note := AccelFallbackNote
	if AccelSpaceBound(tool) {
		note = AccelFallbackSpaceNote
		if i := strings.Index(schema, `},"required"`); i >= 0 {
			schema = schema[:i] + anySpaceProp + schema[i:]
		}
	}
	if i := strings.LastIndex(desc, " [FLEET:"); i >= 0 {
		return desc[:i] + " " + note + desc[i:], schema
	}
	return desc + " " + note, schema
}
