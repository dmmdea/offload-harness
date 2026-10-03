package fleetnode

// The project-bundle door (ADR 0070): POST /fleet/compose-project renders a whole HyperFrames
// project a holder of the fleet token sends, so a machine with no composition lane of its own (a
// thin client) can have any composition rendered on a node that has one. The template door
// (compose-video over /fleet/dispatch) stays as it was: tokenless and vetted templates only.
//
// A project is trusted code — HyperFrames' Chrome runs without a sandbox and executes it — so the
// door is closed unless the node opted in (fleet_compose_projects), holds a fleet token and has the
// composition lane bound (config.ComposeProjectsAdmissible), and the bearer is checked before a byte
// of the body is read. Everything else the door relies on lives in internal/composebundle: the bundle
// is extracted into a fresh directory under the compose cache (regular files only, every name
// confined, caps on the bytes written) and every reference the project's markup makes must stay
// inside it. The directory lives exactly as long as the job.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// ComposeProjectTask is the fleet task type the project door admits.
const ComposeProjectTask = "compose-project"

// ComposeProjectPath is the door's route.
const ComposeProjectPath = "/fleet/compose-project"

// ComposeProjectPayload is the POST /fleet/compose-project body.
type ComposeProjectPayload struct {
	JobID        string    `json:"job_id"`
	Bundle       string    `json:"bundle"`        // base64 (standard encoding) of a gzip-compressed tar
	BundleSHA256 string    `json:"bundle_sha256"` // hex sha256 of the decoded bundle bytes
	Composition  string    `json:"composition,omitempty"`
	Format       string    `json:"format,omitempty"`
	FPS          float64   `json:"fps,omitempty"`
	Quality      string    `json:"quality,omitempty"`
	Resolution   string    `json:"resolution,omitempty"`
	Workers      float64   `json:"workers,omitempty"`
	Strict       *bool     `json:"strict,omitempty"`
	Snapshots    []float64 `json:"snapshots,omitempty"`
}

// ComposeProjectBodyCap is the largest body the door reads: the bundle cap in base64 plus room for
// the rest of the payload.
func ComposeProjectBodyCap(cfg config.Config) int64 {
	return int64(base64.StdEncoding.EncodedLen(int(cfg.EffectiveComposeBundleMaxBytes()))) + 64<<10
}

const composeProjectClosed = "compose-project is not open on this node (it needs fleet_compose_projects, fleet_auth_token and a bound compose route)"

// handleComposeProject checks the door and the bearer BEFORE reading the body, then decodes the
// typed payload and joins the shared admission path.
func (s *Server) handleComposeProject(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Cfg.ComposeProjectsAdmissible() {
		writeError(w, http.StatusForbidden, composeProjectClosed)
		return
	}
	if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := ComposeProjectBodyCap(s.opts.Cfg)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (limit %d bytes: fleet_compose_bundle_max_mb)", limit))
			return
		}
		writeError(w, http.StatusBadRequest, "reading compose-project body: "+err.Error())
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p ComposeProjectPayload
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "malformed compose-project body: "+err.Error())
		return
	}
	s.admit(w, r, dispatchEnvelope{JobID: p.JobID, TaskType: ComposeProjectTask, Payload: body})
}

// buildComposeProject turns an admitted payload into a composition request: the bundle is checked
// against its declared sha256, extracted into a fresh directory and confined, and the request renders
// that directory. The returned cleanup removes the directory; the server runs it when the job ends,
// and on every refusal before that.
func buildComposeProject(_ context.Context, cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	if !cfg.ComposeProjectsAdmissible() {
		return core.Request{}, noop, errors.New(composeProjectClosed)
	}
	var in ComposeProjectPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("compose-project payload: %w", err)
	}
	if in.Format == "png-sequence" {
		return core.Request{}, noop, errors.New("compose-project payload: png-sequence writes a directory, which /fleet/media cannot serve; use mp4, webm, mov or gif")
	}
	raw, err := base64.StdEncoding.DecodeString(in.Bundle)
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("compose-project payload: bundle is not base64: %w", err)
	}
	if int64(len(raw)) > cfg.EffectiveComposeBundleMaxBytes() {
		return core.Request{}, noop, fmt.Errorf("compose-project payload: the bundle is %d bytes, over fleet_compose_bundle_max_mb", len(raw))
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(in.BundleSHA256, hex.EncodeToString(sum[:])) {
		return core.Request{}, noop, errors.New("compose-project payload: bundle_sha256 does not match the bundle")
	}
	base := filepath.Join(cfg.EffectiveComposeCacheDir(), "fleet-projects")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return core.Request{}, noop, fmt.Errorf("compose-project: %w", err)
	}
	dir, err := os.MkdirTemp(base, "proj-")
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("compose-project: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := composebundle.Extract(raw, dir, composebundle.Limits{}); err != nil {
		cleanup()
		return core.Request{}, noop, fmt.Errorf("compose-project bundle refused: %w", err)
	}
	composition := strings.TrimSpace(in.Composition)
	if err := composebundle.Confine(dir, composition); err != nil {
		cleanup()
		return core.Request{}, noop, fmt.Errorf("compose-project bundle refused: %w", err)
	}
	params := map[string]any{"project_dir": dir}
	if composition != "" {
		params["composition"] = composition
	}
	for k, v := range map[string]string{"format": in.Format, "quality": in.Quality, "resolution": in.Resolution} {
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
	return core.Request{Task: core.TaskComposeVideo, Params: params}, cleanup, nil
}
