// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Hand-authored novel command surface (NOT generator output): input staging.
//
// WHY THIS COMMAND EXISTS. LoadImage takes a BARE FILENAME resolved inside
// ComfyUI's input dir, and /history records only that filename — never the host
// file it came from, never its content. The moment the input dir is cleaned (or
// a second `input.png` is dropped in), every archived run referencing that name
// becomes unreproducible, and the matched still+prompt+seed comparisons that
// justify keeping months of history quietly stop meaning anything.
//
// `stage` closes that hole: it content-hashes the host file, puts it into the
// input dir under a content-addressed name, and records
// content_sha256 -> comfy_filename -> host_path in input_asset. Two host files
// named input.png can no longer collide, the same bytes staged twice reuse one
// entry instead of duplicating, and a staged file that has since been deleted
// is re-materialised from the recorded host path under its ORIGINAL name — so
// the archived graph still resolves.

package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"comfyui-pp-cli/internal/comfy/media"
	"comfyui-pp-cli/internal/config"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// stageAssetTypes are the destination roots ComfyUI's /upload/image accepts.
var stageAssetTypes = map[string]bool{"input": true, "temp": true, "output": true}

// stagePresenceProbeTimeout bounds the "is the staged file still there?" check.
// It is deliberately short and independent of --timeout: the probe is an
// optimisation, and a slow answer must never eat the upload's budget.
const stagePresenceProbeTimeout = 5 * time.Second

// stageResult is the machine-readable outcome of one staging call.
type stageResult struct {
	Action string `json:"action"` // staged | reused | rematerialised
	Method string `json:"method"` // copy | upload
	// GraphValue is the ONLY field a caller needs to paste into a graph: the
	// value LoadImage.inputs.image (or any bare-filename input) takes.
	GraphValue    string `json:"graph_value"`
	Filename      string `json:"filename"`
	Subfolder     string `json:"subfolder,omitempty"`
	Type          string `json:"type"`
	ContentSHA256 string `json:"content_sha256"`
	HashAlgorithm string `json:"hash_algorithm"`
	Bytes         int64  `json:"bytes"`
	HostPath      string `json:"host_path"`
	InputDir      string `json:"input_dir,omitempty"`
	TargetPath    string `json:"target_path,omitempty"`
	StagedAt      string `json:"staged_at,omitempty"`
	Note          string `json:"note,omitempty"`
}

// stageAssetRow is an existing input_asset row.
type stageAssetRow struct {
	ContentSHA256 string
	ComfyFilename string
	HostPath      string
	Bytes         int64
	StagedAt      string
}

// pp:data-source live
//
// stage defaults to POSTing /upload/image (the path that works without knowing
// where ComfyUI keeps its input dir). --input-dir switches it to a direct copy,
// which needs no server at all.
func newStageCmd(flags *rootFlags) *cobra.Command {
	var (
		inputDir  string
		method    string
		name      string
		subfolder string
		assetType string
		overwrite bool
		force     bool
	)

	cmd := &cobra.Command{
		Use:   "stage <hostpath>",
		Short: "Stage a host file into ComfyUI's input dir and record its content identity",
		Long: `Copy (or upload) a host file into ComfyUI's input dir, content-hash it, and
record content_sha256 -> comfy_filename -> host path in the local store. Prints
the bare filename to use in the graph.

Why this is not just a file copy: LoadImage takes a BARE FILENAME resolved
inside ComfyUI's input dir, and /history records only that filename. Once the
input dir is cleaned, or a second file with the same base name is dropped in,
every archived run referencing that name silently becomes unreproducible — which
is exactly what a matched still+prompt+seed comparison across models depends on
holding for months.

The staged name is content-addressed (stem-<sha12>.ext), so two different
host files called input.png cannot overwrite each other, and identical bytes
always land on one name. Staging the same content twice REUSES the existing
entry instead of duplicating it; if the staged file has since been deleted, it
is re-materialised under its original recorded name so archived graphs still
resolve.

Methods:
  upload  POST the file to /upload/image (default; needs no path knowledge)
  copy    write straight into --input-dir (or $COMFYUI_INPUT_DIR); no server needed

The recorded filename is always what the SERVER reports back, not what was
requested — if ComfyUI renames an upload to "shot (1).png", that rename is the
identity that /history will show, so that is what gets stored.`,
		Example: `  comfyui-pp-cli stage ./refs/portrait.png
  comfyui-pp-cli stage ./refs/portrait.png --input-dir C:\ComfyUI\input
  comfyui-pp-cli stage ./refs/portrait.png --json
  comfyui-pp-cli stage ./refs/plate.png --name plate.png --subfolder refs`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Dry-run is checked before positional validation so the
			// harness's bare `--dry-run` probe short-circuits instead of
			// falling into the usage branch.
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "stage")
			}
			if len(args) == 0 {
				return comfyRequiresInput(cmd, flags)
			}
			if len(args) > 1 {
				return usageErr(fmt.Errorf("stage takes exactly one host path (got %d); quote paths containing spaces", len(args)))
			}
			return runStage(cmd, flags, args[0], stageOptions{
				InputDir:  inputDir,
				Method:    method,
				Name:      name,
				Subfolder: subfolder,
				AssetType: assetType,
				Overwrite: overwrite,
				Force:     force,
			})
		},
	}

	cmd.Flags().StringVar(&inputDir, "input-dir", "", "ComfyUI's input directory on this host; setting it selects the direct-copy method (env: COMFYUI_INPUT_DIR)")
	cmd.Flags().StringVar(&method, "method", "auto", "How to place the file: auto (copy when --input-dir is known, else upload), copy, or upload")
	cmd.Flags().StringVar(&name, "name", "", "Override the staged filename; must be relative to the input dir, never a host path. Default is content-addressed: stem-<sha12>.ext")
	cmd.Flags().StringVar(&subfolder, "subfolder", "", "Subfolder inside the input dir; the graph value becomes <subfolder>/<filename>")
	cmd.Flags().StringVar(&assetType, "type", "input", "Upload destination root: input, temp, or output")
	cmd.Flags().BoolVar(&overwrite, "overwrite", true, "Replace a same-named file on upload. Default true because staged names are content-addressed, so a name collision means identical bytes; --overwrite=false lets ComfyUI rename instead (and the rename becomes the recorded identity)")
	cmd.Flags().BoolVar(&force, "force", false, "Re-stage even when this exact content is already recorded")

	return cmd
}

type stageOptions struct {
	InputDir  string
	Method    string
	Name      string
	Subfolder string
	AssetType string
	Overwrite bool
	Force     bool
}

func runStage(cmd *cobra.Command, flags *rootFlags, hostArg string, opts stageOptions) error {
	hostPath := hostArg
	if abs, err := filepath.Abs(hostArg); err == nil {
		hostPath = abs
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		if os.IsNotExist(err) {
			return notFoundErr(fmt.Errorf("no such file: %s", hostPath))
		}
		return notFoundErr(fmt.Errorf("reading %s: %w", hostPath, err))
	}
	if info.IsDir() {
		return usageErr(fmt.Errorf("%s is a directory; stage takes a single file", hostPath))
	}

	inputDir := strings.TrimSpace(opts.InputDir)
	if inputDir == "" {
		inputDir = strings.TrimSpace(os.Getenv("COMFYUI_INPUT_DIR"))
	}
	stageMethod, err := stageResolveMethod(opts.Method, inputDir)
	if err != nil {
		return err
	}
	// The copy path never dials the server, so it declares the narrower
	// strategy; only the upload path is a live call.
	strategy := "local"
	if stageMethod == "upload" {
		strategy = "live"
	}
	if err := validateDataSourceStrategy(flags, strategy); err != nil {
		return usageErr(err)
	}

	assetType := strings.ToLower(strings.TrimSpace(opts.AssetType))
	if assetType == "" {
		assetType = "input"
	}
	if !stageAssetTypes[assetType] {
		return usageErr(fmt.Errorf("invalid --type %q: must be input, temp, or output", opts.AssetType))
	}

	subfolder, err := stageNormaliseSubfolder(opts.Subfolder)
	if err != nil {
		return usageErr(err)
	}

	sha, size, err := media.HashFile(hostPath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", hostPath, err)
	}

	filename := strings.TrimSpace(opts.Name)
	if filename == "" {
		filename = media.StagedName(hostPath, sha)
	} else {
		filename = filepath.ToSlash(filename)
	}
	if err := media.ValidateComfyFilename(filename); err != nil {
		return usageErr(fmt.Errorf("--name: %w", err))
	}
	graphValue := filename
	if subfolder != "" {
		graphValue = subfolder + "/" + filename
	}

	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()

	s, err := openStagingStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.DB()

	existing, found, err := stageLookupContent(ctx, db, sha)
	if err != nil {
		return err
	}

	// rematerialiseTarget, when set, is the recorded filename that MUST come
	// back unchanged: anything else would move an identity archived runs
	// already reference.
	var rematerialiseTarget string

	result := stageResult{
		Method:        stageMethod,
		Type:          assetType,
		ContentSHA256: sha,
		HashAlgorithm: media.HashAlgorithm,
		Bytes:         size,
		HostPath:      hostPath,
		InputDir:      inputDir,
	}

	if found && !opts.Force {
		present, known, note := stageAssetPresent(ctx, flags, stageMethod, inputDir, existing.ComfyFilename, assetType)
		if present || !known {
			// Refresh only the host-path pointer. comfy_filename is the
			// identity archived runs already reference and must not move.
			if _, err := db.ExecContext(ctx,
				`UPDATE input_asset SET host_path = ? WHERE content_sha256 = ?`, hostPath, sha); err != nil {
				return fmt.Errorf("refreshing staged host path: %w", err)
			}
			result.Action = "reused"
			result.GraphValue = existing.ComfyFilename
			result.Filename = path.Base(existing.ComfyFilename)
			result.Subfolder = stageParentDir(existing.ComfyFilename)
			result.Bytes = stagePreferInt64(existing.Bytes, size)
			result.StagedAt = existing.StagedAt
			result.Note = strings.TrimSpace("identical content already staged as " + existing.ComfyFilename + "; not duplicated. " + note)
			if existing.HostPath != "" && existing.HostPath != hostPath {
				result.Note += " Previously staged from " + existing.HostPath + "."
			}
			return stageEmit(cmd, flags, result)
		}
		// The recorded file is gone. Re-materialise it under the SAME name so
		// archived graphs referencing it keep resolving. Overwrite is forced:
		// the content hash already matched, so there is nothing to lose, and
		// letting ComfyUI rename would move the very identity being restored.
		result.Action = "rematerialised"
		graphValue = existing.ComfyFilename
		filename = path.Base(existing.ComfyFilename)
		subfolder = stageParentDir(existing.ComfyFilename)
		rematerialiseTarget = existing.ComfyFilename
		opts.Overwrite = true
		result.Note = "the recorded staged file was missing; re-materialised under its original name so archived runs still resolve."
	} else {
		result.Action = "staged"
		if err := stageAssertFilenameFree(ctx, db, graphValue, sha); err != nil {
			return err
		}
	}
	checkedGraphValue := graphValue

	switch stageMethod {
	case "copy":
		target := filepath.Join(inputDir, filepath.FromSlash(graphValue))
		if err := stageCopyFile(hostPath, target); err != nil {
			return err
		}
		result.TargetPath = target
	case "upload":
		cfg, err := config.Load(flags.configPath)
		if err != nil {
			return configErr(err)
		}
		if strings.TrimSpace(cfg.BaseURL) == "" {
			return configErr(errors.New("no base_url configured; set it in the config file or COMFYUI_BASE_URL, or pass --input-dir to stage by copy instead"))
		}
		uploaded, err := stageUploadImage(ctx, flags, cfg.BaseURL, hostPath, filename, subfolder, assetType, opts.Overwrite)
		if err != nil {
			return err
		}
		// The server is authoritative about the stored name: with
		// --overwrite=false ComfyUI renames a colliding upload to "shot (1).png"
		// and THAT is what /history will report.
		if uploaded.Name != "" && uploaded.Name != filename {
			result.Note = strings.TrimSpace(result.Note + " ComfyUI stored the upload as " + uploaded.Name + " (a file of the requested name already existed); the recorded identity follows the server.")
			filename = uploaded.Name
		}
		if uploaded.Subfolder != "" {
			subfolder = strings.Trim(filepath.ToSlash(uploaded.Subfolder), "/")
		}
		if uploaded.Type != "" {
			assetType = uploaded.Type
			result.Type = uploaded.Type
		}
		graphValue = filename
		if subfolder != "" {
			graphValue = subfolder + "/" + filename
		}
	}

	// A server-side rename lands on a name that was never collision-checked, so
	// re-check before it becomes the recorded identity.
	if graphValue != checkedGraphValue {
		if err := stageAssertFilenameFree(ctx, db, graphValue, sha); err != nil {
			return err
		}
	}
	if rematerialiseTarget != "" && graphValue != rematerialiseTarget {
		return apiErr(fmt.Errorf(
			"re-materialising %q produced %q instead; refusing to move the recorded identity, because archived runs reference the original name. Remove the conflicting file or re-run with --force --name %s",
			rematerialiseTarget, graphValue, rematerialiseTarget))
	}
	if found && opts.Force && existing.ComfyFilename != "" && existing.ComfyFilename != graphValue {
		result.Note = strings.TrimSpace(result.Note + fmt.Sprintf(
			" WARNING: --force moved this content's recorded filename from %q to %q; archived runs referencing the old name no longer resolve to a staged asset.",
			existing.ComfyFilename, graphValue))
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO input_asset (content_sha256, comfy_filename, host_path, bytes)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(content_sha256) DO UPDATE SET
		     comfy_filename = excluded.comfy_filename,
		     host_path      = excluded.host_path,
		     bytes          = excluded.bytes`,
		sha, graphValue, hostPath, size); err != nil {
		return fmt.Errorf("recording input asset: %w", err)
	}

	result.GraphValue = graphValue
	result.Filename = filename
	result.Subfolder = subfolder
	if row, ok, err := stageLookupContent(ctx, db, sha); err == nil && ok {
		result.StagedAt = row.StagedAt
	}
	return stageEmit(cmd, flags, result)
}

// stageAssertFilenameFree rejects a filename already bound to DIFFERENT
// content. That collision is the one this whole command exists to prevent: the
// same graph value would resolve to other bytes than the archived run consumed,
// and nothing downstream would ever notice.
func stageAssertFilenameFree(ctx context.Context, db *sql.DB, graphValue, sha string) error {
	var other string
	err := db.QueryRowContext(ctx,
		`SELECT content_sha256 FROM input_asset WHERE comfy_filename = ? AND content_sha256 <> ? LIMIT 1`,
		graphValue, sha).Scan(&other)
	switch {
	case err == nil:
		return usageErr(fmt.Errorf(
			"%q is already staged for different content (sha256 %s...); reusing that name would silently change what archived runs referenced. Pass --name to choose another filename",
			graphValue, media.ShortSHA(other)))
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("checking staged filename: %w", err)
	}
}

// stageResolveMethod picks between the direct copy and the /upload/image POST.
func stageResolveMethod(requested, inputDir string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", "auto":
		if inputDir != "" {
			return "copy", nil
		}
		return "upload", nil
	case "copy":
		if inputDir == "" {
			return "", usageErr(errors.New("--method copy needs ComfyUI's input directory: pass --input-dir or set COMFYUI_INPUT_DIR"))
		}
		return "copy", nil
	case "upload":
		return "upload", nil
	default:
		return "", usageErr(fmt.Errorf("invalid --method %q: must be auto, copy, or upload", requested))
	}
}

// stageNormaliseSubfolder trims a subfolder to a slash-separated relative path
// and rejects anything that would escape the input dir.
func stageNormaliseSubfolder(raw string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(filepath.ToSlash(raw)), "/")
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	if err := media.ValidateComfyFilename(trimmed); err != nil {
		return "", fmt.Errorf("--subfolder: %w", err)
	}
	return trimmed, nil
}

func stageParentDir(graphValue string) string {
	dir := path.Dir(filepath.ToSlash(graphValue))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

func stagePreferInt64(recorded, fallback int64) int64 {
	if recorded > 0 {
		return recorded
	}
	return fallback
}

// openStagingStore opens the local store and applies the ComfyUI domain
// migrations. Idempotent; safe on every invocation.
func openStagingStore(ctx context.Context) (*store.Store, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("comfyui-pp-cli"))
	if err != nil {
		return nil, fmt.Errorf("opening local store: %w", err)
	}
	if err := store.MigrateComfyUI(ctx, s.DB()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func stageLookupContent(ctx context.Context, db *sql.DB, sha string) (stageAssetRow, bool, error) {
	var (
		row       stageAssetRow
		hostPath  sql.NullString
		byteCount sql.NullInt64
		stagedAt  any
	)
	err := db.QueryRowContext(ctx,
		`SELECT content_sha256, comfy_filename, host_path, bytes, staged_at
		   FROM input_asset WHERE content_sha256 = ?`, sha).
		Scan(&row.ContentSHA256, &row.ComfyFilename, &hostPath, &byteCount, &stagedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return stageAssetRow{}, false, nil
	}
	if err != nil {
		return stageAssetRow{}, false, fmt.Errorf("looking up staged content: %w", err)
	}
	row.HostPath = hostPath.String
	row.Bytes = byteCount.Int64
	row.StagedAt = comfyTimeString(stagedAt)
	return row, true, nil
}

// stageAssetPresent answers "is the recorded staged file still there?".
//
// The `known` return matters: an unknown answer must NOT trigger a re-stage,
// because re-staging on a false negative is how a stable identity gets churned.
// Only a definite "missing" re-materialises.
func stageAssetPresent(ctx context.Context, flags *rootFlags, method, inputDir, graphValue, assetType string) (present bool, known bool, note string) {
	switch method {
	case "copy":
		if inputDir == "" {
			return false, false, "input dir unknown, presence not verified."
		}
		target := filepath.Join(inputDir, filepath.FromSlash(graphValue))
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			return true, true, ""
		} else if os.IsNotExist(err) {
			return false, true, ""
		}
		return false, false, "presence not verified."
	case "upload":
		cfg, err := config.Load(flags.configPath)
		if err != nil || strings.TrimSpace(cfg.BaseURL) == "" {
			return false, false, "server unreachable, presence not verified."
		}
		probeCtx, cancel := context.WithTimeout(ctx, stagePresenceProbeTimeout)
		defer cancel()
		status, err := stageProbeView(probeCtx, cfg.BaseURL, graphValue, assetType)
		switch {
		case err != nil:
			return false, false, "server did not answer the presence probe; presence not verified."
		case status == http.StatusOK:
			return true, true, ""
		case status == http.StatusNotFound || status == http.StatusBadRequest:
			return false, true, ""
		default:
			return false, false, fmt.Sprintf("presence probe returned HTTP %d; presence not verified.", status)
		}
	}
	return false, false, ""
}

// stageProbeView asks /view whether a staged file is still on the server. HEAD
// keeps it to headers so probing a 300 MB video costs nothing.
func stageProbeView(ctx context.Context, baseURL, graphValue, assetType string) (int, error) {
	params := url.Values{}
	params.Set("filename", path.Base(graphValue))
	params.Set("type", assetType)
	if sub := stageParentDir(graphValue); sub != "" {
		params.Set("subfolder", sub)
	}
	target := strings.TrimRight(baseURL, "/") + "/view?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// stageCopyFile writes src to dst via a temp file in the destination directory
// and renames it into place, so a half-copied file is never visible to a render
// that is already queued.
func stageCopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pp-stage-*")
	if err != nil {
		return fmt.Errorf("creating staging temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return fmt.Errorf("copying to %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing staging temp file: %w", err)
	}
	// Windows rejects a rename onto an existing file, so clear the target
	// first. Identical content under a content-addressed name makes this a
	// no-op replacement rather than a destructive one.
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("replacing %s: %w", dst, err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("moving staged file into %s: %w", dst, err)
	}
	return nil
}

// stageUploadResponse is ComfyUI's /upload/image reply.
type stageUploadResponse struct {
	Name      string `json:"name"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// stageUploadImage POSTs a multipart/form-data upload to /upload/image.
//
// The multipart mechanics moved to comfyMultipartUpload (comfy_upload_mask.go)
// when `upload mask` arrived and needed the identical pipe/teardown/status
// handling against a different endpoint. This function keeps stage's own
// vocabulary — subfolder, assetType, overwrite — and translates it into the
// form fields /upload/image expects.
func stageUploadImage(ctx context.Context, flags *rootFlags, baseURL, hostPath, filename, subfolder, assetType string, overwrite bool) (stageUploadResponse, error) {
	fields := map[string]string{"type": assetType}
	if subfolder != "" {
		fields["subfolder"] = subfolder
	}
	if overwrite {
		fields["overwrite"] = "true"
	}
	return comfyMultipartUpload(ctx, flags, baseURL, "/upload/image", hostPath, filename, fields)
}

func stageEmit(cmd *cobra.Command, flags *rootFlags, result stageResult) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, result)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%s %s\n", stageActionLabel(result.Action), result.GraphValue)
	fmt.Fprintf(w, "  graph value:  %q  <- bare filename; LoadImage never takes a host path\n", result.GraphValue)
	fmt.Fprintf(w, "  content:      %s:%s (%s)\n", result.HashAlgorithm, result.ContentSHA256, stageHumanBytes(result.Bytes))
	fmt.Fprintf(w, "  host path:    %s\n", result.HostPath)
	fmt.Fprintf(w, "  method:       %s (type=%s)\n", result.Method, result.Type)
	if result.TargetPath != "" {
		fmt.Fprintf(w, "  written to:   %s\n", result.TargetPath)
	}
	if result.StagedAt != "" {
		fmt.Fprintf(w, "  staged at:    %s\n", result.StagedAt)
	}
	if result.Note != "" {
		fmt.Fprintf(w, "  note:         %s\n", result.Note)
	}
	return nil
}

func stageActionLabel(action string) string {
	switch action {
	case "reused":
		return yellow("reused")
	case "rematerialised":
		return yellow("rematerialised")
	default:
		return green("staged")
	}
}

func stageHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// comfyTimeString renders a SQLite DATETIME column defensively. The driver may
// hand back a time.Time (decltype-driven) or the raw string, and a scan into
// sql.NullTime would fail on the latter — so the column is scanned into `any`
// and normalised here.
func comfyTimeString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case string:
		return strings.TrimSpace(t)
	case []byte:
		return strings.TrimSpace(string(t))
	default:
		return fmt.Sprintf("%v", t)
	}
}
