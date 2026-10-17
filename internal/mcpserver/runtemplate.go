package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/rungraph"
)

// offload_run_template: look up and run the ComfyUI workflow templates a machine's installed package carries.
//
// ONE tool with an action enum (list, get, readiness, run), and not a `template=` argument on offload_run_graph: a template is a
// different object from a graph (a UI-format workflow that is converted on the node that runs it, with that node's own listing of its
// nodes, and gated by licence), and the run-graph tool's schema is already the longest of the media doors. The cost of one more tool was
// measured at 0.186.0: an average tool is 1,019 tokens in the full profile and 672 compact, so one more tool is about +3.2% of the
// 31-tool full list (docs/systems/mcp-server.md).
//
// list, get and readiness are read-only answers of render/run-template.mjs over the node's own tree. run asks the same script to resolve the
// template (the gate: which template this release may run, and what the node lacks), and only for a template that may run it goes through
// the run-graph task: the same lease, ComfyUI lifecycle, satisfier and envelope as offload_run_graph, with the template NAME in place of
// a graph file (the runner converts it once ComfyUI is up). It is not a second dispatch.

var templateNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const runTemplateActions = "list, get, readiness or run"

// templateBadArgs is the typed refusal of a call the door cannot read, the shape every door answers a bad argument in.
func templateBadArgs(reason string) (*mcp.CallToolResult, error) {
	return jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: " + reason})
}

// templateStepFailed is the deferral for a node-side step that did not answer: the script is missing, the tree is not a ComfyUI tree, the
// script exited non-zero or printed something that is not its JSON object.
func templateStepFailed(verb string, err error) (*mcp.CallToolResult, error) {
	return jsonResult(map[string]any{"deferred": true, "reason": fmt.Sprintf("run-template %s failed: %v", verb, err)})
}

// queryTemplate runs one verb of render/run-template.mjs: the injected seam in a test, else the script beside the configured run-graph
// runner on this machine's node runtime, over this machine's ComfyUI tree.
func (s *Server) queryTemplate(ctx context.Context, args []string) ([]byte, error) {
	if s.templateExec != nil {
		return s.templateExec(ctx, args)
	}
	cfg := s.mediaCfg()
	if strings.TrimSpace(cfg.ComfyDir) == "" {
		return nil, errors.New("comfy_dir is not configured on this machine, so there is no templates package to read")
	}
	if strings.TrimSpace(cfg.RunGraphScript) == "" {
		return nil, errors.New("no run-graph route is configured (run_graph_script), so there is no run-template script beside it")
	}
	runner, err := gpugen.ResolveScript(cfg.RunGraphScript)
	if err != nil {
		return nil, err
	}
	script, err := rungraph.TemplateScript(runner)
	if err != nil {
		return nil, err
	}
	if fi, serr := os.Stat(script); serr != nil || fi.IsDir() {
		return nil, fmt.Errorf("script not found at %s (it ships beside the run-graph runner)", script)
	}
	return rungraph.QueryTemplate(ctx, cfg.NodePath, script, cfg.ComfyDir, args, 0)
}

// templateObject reads the script's answer: one JSON object.
func templateObject(out []byte) (map[string]any, string, error) {
	text := strings.TrimSpace(string(out))
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return nil, text, errors.New("the script's answer is not a JSON object")
	}
	return obj, text, nil
}

func textResult(text string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

func (s *Server) handleRunTemplate(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Action       string          `json:"action"`
		Template     string          `json:"template"`
		Kind         string          `json:"kind"`
		Tag          string          `json:"tag"`
		Text         string          `json:"text"`
		LicenseClass string          `json:"license_class"`
		Runnable     *bool           `json:"runnable"`
		ReadsInput   *bool           `json:"reads_input"`
		Limit        *int            `json:"limit"`
		Offset       *int            `json:"offset"`
		Overrides    json.RawMessage `json:"overrides"`
		Seed         json.RawMessage `json:"seed"`
		OutDir       string          `json:"out_dir"`
		ReserveVram  string          `json:"reserve_vram"`
		// Not parameters of this tool: a run is on this machine's lane. They are read only to be refused by name when a caller
		// passes the ones the other media doors take.
		Route   string   `json:"route"`
		Remotes []string `json:"remotes"`
		Node    string   `json:"node"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	switch in.Action {
	case "list", "get", "readiness", "run":
	default:
		return templateBadArgs("action must be " + runTemplateActions)
	}
	if in.Action != "list" {
		if in.Template == "" {
			return templateBadArgs("template is required for " + in.Action + " (a name from list)")
		}
		if !templateNameRE.MatchString(in.Template) || strings.Contains(in.Template, "..") {
			return templateBadArgs("template must be a template name (letters, digits, . _ -), not a path")
		}
	}
	switch in.Action {
	case "list":
		return s.templateList(ctx, in.Kind, in.Tag, in.Text, in.LicenseClass, in.Runnable, in.ReadsInput, in.Limit, in.Offset)
	case "get", "readiness":
		return s.templateRead(ctx, in.Action, in.Template)
	}

	// run
	var overrides map[string]any
	if len(in.Overrides) > 0 && string(in.Overrides) != "null" {
		if err := json.Unmarshal(in.Overrides, &overrides); err != nil || overrides == nil {
			return templateBadArgs("overrides must be an object of <node id>.<input name> keys to string, number or boolean values")
		}
	}
	seed := ""
	if len(in.Seed) > 0 && string(in.Seed) != "null" {
		var n float64
		if err := json.Unmarshal(in.Seed, &n); err != nil || n < 0 || n != math.Trunc(n) || n > 1<<53 {
			return templateBadArgs("seed must be a whole number from 0")
		}
		seed = strconv.FormatInt(int64(n), 10)
	}
	out, err := s.queryTemplate(ctx, []string{"resolve", in.Template})
	if err != nil {
		return templateStepFailed("resolve", err)
	}
	ans, text, err := templateObject(out)
	if err != nil {
		return templateStepFailed("resolve", err)
	}
	// The gate's answer is the answer for a template this release will not run (a Defer or a refusal with its class and, for what the
	// node lacks, the exact list): nothing is started, leased or sent.
	if ok, _ := ans["ok"].(bool); !ok {
		return textResult(text)
	}
	// A run is on this machine's lane. The fleet door takes an API-format graph, and a template is converted on the node that runs it
	// with that node's own listing; a template that reads input files is also never sent, because a node can match an input file only by
	// name. A caller that names a route beyond local, a fleet node or remotes is told so, with the reason the node's answer gave.
	if (in.Route != "" && in.Route != "local") || len(in.Remotes) > 0 || in.Node != "" {
		note, _ := ans["fleet_note"].(string)
		return jsonResult(map[string]any{
			"ok": false, "outcome": "refuse", "refused": true, "class": "fleet_not_offered", "code": "TEMPLATE_FLEET_NOT_OFFERED",
			"template": in.Template, "detail": "a template run stays on this machine's lane in this release: " + note,
		})
	}
	manifest, err := json.Marshal(ans["runtime_manifest"])
	if err != nil || ans["runtime_manifest"] == nil {
		return templateStepFailed("resolve", errors.New("the script's answer carries no runtime manifest"))
	}
	manifestPath, cleanManifest, err := materialize("", string(manifest), "run-template-manifest-*.json")
	if err != nil {
		return templateBadArgs("manifest: " + err.Error())
	}
	defer cleanManifest()
	params := map[string]any{
		"template":      in.Template,
		"manifest_path": manifestPath,
		"out_dir":       in.OutDir,
		"reserve_vram":  in.ReserveVram,
	}
	if len(overrides) > 0 {
		body, _ := json.Marshal(overrides)
		overridesPath, cleanOverrides, err := materialize("", string(body), "run-template-overrides-*.json")
		if err != nil {
			return templateBadArgs("overrides: " + err.Error())
		}
		defer cleanOverrides()
		params["overrides_path"] = overridesPath
	}
	if seed != "" {
		params["seed"] = seed
	}
	// The run-graph task, through the run-graph door's own call: the lease, the ComfyUI lifecycle, the satisfier and the envelope are
	// the ones offload_run_graph has. The route is local, so no ticket and no roster: a busy lane answers as a run-graph call on this
	// machine does (a place in this machine's line, resumable with the waiter_token).
	res := mediaremote.RunWith(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskRunGraph, Door: "offload_run_template", Params: withMediaPlace(req.Params.Arguments, params)},
		"local", nil, s.imageWait(nil))
	return result(withTemplateFields(res, ans, in.Template))
}

// templateList asks the script for the catalog, filtered. The flags are in a fixed order, so the same call is the same command.
func (s *Server) templateList(ctx context.Context, kind, tag, text, licenseClass string, runnable, readsInput *bool, limit, offset *int) (*mcp.CallToolResult, error) {
	args := []string{"list"}
	for _, f := range []struct{ flag, value string }{{"--kind", kind}, {"--tag", tag}, {"--text", text}, {"--license-class", licenseClass}} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	if runnable != nil {
		args = append(args, "--runnable", strconv.FormatBool(*runnable))
	}
	if readsInput != nil {
		args = append(args, "--reads-input", strconv.FormatBool(*readsInput))
	}
	if limit != nil {
		args = append(args, "--limit", strconv.Itoa(*limit))
	}
	if offset != nil {
		args = append(args, "--offset", strconv.Itoa(*offset))
	}
	return s.templateAnswer(ctx, "list", args)
}

// templateRead is get and readiness: one template, by name.
func (s *Server) templateRead(ctx context.Context, verb, template string) (*mcp.CallToolResult, error) {
	return s.templateAnswer(ctx, verb, []string{verb, template})
}

// templateAnswer returns the script's own JSON as the tool's answer, byte for byte.
func (s *Server) templateAnswer(ctx context.Context, verb string, args []string) (*mcp.CallToolResult, error) {
	out, err := s.queryTemplate(ctx, args)
	if err != nil {
		return templateStepFailed(verb, err)
	}
	_, text, err := templateObject(out)
	if err != nil {
		return templateStepFailed(verb, err)
	}
	return textResult(text)
}

// withTemplateFields puts the template, its licence class and where it ran beside the outputs of a finished run, so a result says what
// produced it without the caller keeping the resolve answer. A deferral or any answer that is not an object is returned as it is.
func withTemplateFields(r core.Result, ans map[string]any, template string) core.Result {
	if !r.OK || len(r.Data) == 0 {
		return r
	}
	var data map[string]any
	if json.Unmarshal(r.Data, &data) != nil || data == nil {
		return r
	}
	data["template"] = template
	if lic, ok := ans["license"].(map[string]any); ok {
		data["license_class"] = lic["class"]
	}
	for _, k := range []string{"reads_input", "local_only", "fleet_note"} {
		if v, ok := ans[k]; ok {
			data[k] = v
		}
	}
	if b, err := json.Marshal(data); err == nil {
		r.Data = b
	}
	return r
}
