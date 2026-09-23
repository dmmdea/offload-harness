package fleetnode

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/dmmdea/offload-harness/internal/core"
)

// ComposeTask is the fleet task_type of the composition lane (ADR 0059): one
// HyperFrames render of a VETTED template on this node, returned exactly the way
// image-gen returns its PNG — the result's video_path lands under this node's
// media_dir (the default out) and the delegator fetches it by bare name from
// GET /fleet/media/{name}.
//
// Advertised iff config.ComposeRouteConfigured() — the same predicate the pipeline
// gates on, so health never promises a lane dispatch would defer.
//
// Template-only, deliberately. A composition is trusted code: HyperFrames' Chrome
// runs with --no-sandbox and site isolation off, and media dispatch is not
// token-gated (ADR 0023). A free-form `html` or a node-local `project_dir` from an
// unauthenticated tailnet caller would therefore be arbitrary code in an unsandboxed
// browser on this node, so both are refused at ack time; the node's own vetted
// templates (render/compose-templates) with typed, escaped variables are the fleet
// surface. png-sequence is refused too: it writes a DIRECTORY, which /fleet/media
// (bare file names only) cannot serve back.
const ComposeTask = "compose-video"

var composeFleetTemplate = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// buildComposeVideo mirrors mcpserver.handleComposeVideo for the template form.
func buildComposeVideo(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Template   string          `json:"template"`
		Variables  map[string]any  `json:"variables"`
		HTML       json.RawMessage `json:"html"`
		ProjectDir json.RawMessage `json:"project_dir"`
		Out        string          `json:"out"`
		Format     string          `json:"format"`
		FPS        float64         `json:"fps"`
		Quality    string          `json:"quality"`
		Resolution string          `json:"resolution"`
		Workers    float64         `json:"workers"`
		Strict     *bool           `json:"strict"`
		Snapshots  []float64       `json:"snapshots"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("compose-video payload: %w", err)
	}
	if len(in.HTML) > 0 || len(in.ProjectDir) > 0 {
		return core.Request{}, noop, fmt.Errorf("compose-video payload: html and project_dir are not accepted over the fleet — compositions are trusted code (HyperFrames' Chrome runs without a sandbox) and media dispatch is not token-gated; send a vetted template name plus variables")
	}
	if in.Template == "" {
		return core.Request{}, noop, fmt.Errorf("compose-video payload: template required")
	}
	if !composeFleetTemplate.MatchString(in.Template) {
		return core.Request{}, noop, fmt.Errorf("compose-video payload: template %q is not a template name", in.Template)
	}
	if in.Format == "png-sequence" {
		return core.Request{}, noop, fmt.Errorf("compose-video payload: png-sequence writes a directory, which /fleet/media cannot serve; use mp4, webm, mov or gif")
	}
	params := map[string]any{"template": in.Template}
	if in.Variables != nil {
		params["variables"] = in.Variables
	}
	for k, v := range map[string]string{"out": in.Out, "format": in.Format, "quality": in.Quality, "resolution": in.Resolution} {
		if v != "" {
			params[k] = v
		}
	}
	if in.FPS != 0 {
		params["fps"] = in.FPS
	}
	if in.Workers != 0 {
		params["workers"] = in.Workers
	}
	if in.Strict != nil {
		params["strict"] = *in.Strict
	}
	if len(in.Snapshots) > 0 {
		params["snapshots"] = in.Snapshots
	}
	return core.Request{Task: core.TaskComposeVideo, Params: params}, noop, nil
}
