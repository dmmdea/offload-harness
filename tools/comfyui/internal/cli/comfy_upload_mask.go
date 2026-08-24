// upload mask — POST /upload/mask (multipart).
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// WHAT THE ENDPOINT ACTUALLY DOES, read from the ComfyUI server source
// (server.py upload_mask + image_upload) rather than from the route list,
// because the route list does not say any of this:
//
//   - It is NOT a plain file upload. The server opens the EXISTING image named
//     by original_ref, takes the ALPHA CHANNEL of the file you post, puts that
//     alpha onto the original, and saves the composite. Post an opaque PNG and
//     you will silently write a fully-opaque mask.
//   - original_ref is REQUIRED and is a JSON *string* inside the multipart
//     form: {"filename": ..., "subfolder": ..., "type": ...}. Without it the
//     handler raises on json.loads(None) — a 500, not a helpful 400.
//   - The referenced original must already exist on the server. If it does not,
//     the handler's isfile() guard means NOTHING is written and you still get a
//     200 with a filename in the reply. A 200 is not proof the mask landed.
//   - Traversal is rejected server-side (leading '/' or '..' -> 400), and the
//     PNG text chunks of the original are preserved into the composite.
//
// This command therefore validates original_ref locally and says all of the
// above in its help, because every one of these is a silent-wrong-result trap
// rather than an error.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"comfyui-pp-cli/internal/cliutil"
	"comfyui-pp-cli/internal/comfy/media"
	"comfyui-pp-cli/internal/config"
	"github.com/spf13/cobra"
)

// comfyOriginalRef is the JSON value carried in the multipart original_ref
// field: the image the mask's alpha is composited onto.
type comfyOriginalRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder,omitempty"`
	Type      string `json:"type,omitempty"`
}

type comfyUploadMaskResult struct {
	Action      string           `json:"action"`
	Executed    bool             `json:"executed"`
	HostPath    string           `json:"host_path"`
	GraphValue  string           `json:"graph_value,omitempty"`
	OriginalRef comfyOriginalRef `json:"original_ref"`
	Name        string           `json:"name,omitempty"`
	Subfolder   string           `json:"subfolder"`
	Type        string           `json:"type"`
	Note        string           `json:"note"`
}

const comfyMaskAlphaNote = "ComfyUI composites the ALPHA CHANNEL of the posted file onto the original — " +
	"it does not store the file as-is. A mask with no transparency writes a fully-opaque result. " +
	"The server also answers 200 even when the referenced original does not exist, so confirm with " +
	"'comfyui-pp-cli view --filename <name>' rather than trusting the status."

func newComfyUploadMaskCmd(flags *rootFlags) *cobra.Command {
	var (
		originalFilename  string
		originalSubfolder string
		originalType      string
		subfolder         string
		assetType         string
		overwrite         bool
		execute           bool
	)

	cmd := &cobra.Command{
		Use:   "mask <mask-file>",
		Short: "Upload a mask and composite its alpha onto an existing server-side image (--execute required)",
		Long: `POST /upload/mask — upload a mask image whose ALPHA CHANNEL is composited onto
an image already on the server.

THIS IS NOT A FILE COPY. The server opens the image named by --original, takes
the alpha channel of the file you post, puts that alpha onto the original, and
saves the composite. Posting an opaque image writes a fully-opaque mask and
reports success. This is the whole reason headless inpainting is hard to
reproduce, and it is why this command spells the behaviour out.

--original is REQUIRED and names an image that must ALREADY EXIST on the
server (stage it first with 'comfyui-pp-cli stage'). If it does not exist the
server writes nothing and STILL answers 200 with a filename — so a success here
is not proof the mask landed. Verify separately.

--original-subfolder and --original-type locate that original; they default to
"" and "input", which is where 'stage' puts things.

Prints what it would do and sends nothing unless --execute is passed.

Exit codes:
  0   the request was printed (default) or accepted (--execute)
  2   usage error, including a missing or unsafe --original
  4   ComfyUI is unreachable
  5   the server refused the upload`,
		Example: `  comfyui-pp-cli upload mask mask.png --original portrait.png
  comfyui-pp-cli upload mask mask.png --original portrait.png --execute
  comfyui-pp-cli upload mask mask.png --original portrait.png --original-type input --execute --json`,
		Annotations: map[string]string{
			// No mcp:read-only — this writes a file on the server.
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4,5",
		},
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErr(errors.New("no mask file given: comfyui-pp-cli upload mask <mask-file> --original <server-filename>"))
			}
			hostPath := args[0]
			if strings.TrimSpace(originalFilename) == "" {
				return usageErr(errors.New(
					"--original is required: /upload/mask composites onto an EXISTING server-side image and the server 500s without it"))
			}
			// The server rejects traversal with a bare 400. Catching it here
			// turns an opaque status into a sentence that names the problem.
			if err := media.ValidateComfyFilename(originalFilename); err != nil {
				return usageErr(fmt.Errorf("--original %q is not a plain server-side filename: %w", originalFilename, err))
			}
			if assetType == "" {
				assetType = "input"
			}
			if originalType == "" {
				originalType = "input"
			}

			info, err := os.Stat(hostPath)
			if err != nil {
				return notFoundErr(fmt.Errorf("reading mask file %s: %w", hostPath, err))
			}
			if info.IsDir() {
				return usageErr(fmt.Errorf("%s is a directory, not a mask image", hostPath))
			}

			ref := comfyOriginalRef{
				Filename:  originalFilename,
				Subfolder: originalSubfolder,
				Type:      originalType,
			}
			filename := filepath.Base(filepath.FromSlash(hostPath))

			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags,
					fmt.Sprintf("POST /upload/mask with %s onto original %s", filename, originalFilename))
			}

			if !execute {
				return comfyUploadMaskEmit(cmd, flags, comfyUploadMaskResult{
					Action:      "would-upload",
					Executed:    false,
					HostPath:    hostPath,
					OriginalRef: ref,
					Subfolder:   subfolder,
					Type:        assetType,
					Note:        "nothing was sent — re-run with --execute. " + comfyMaskAlphaNote,
				})
			}

			if cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
				return writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: no POST /upload/mask was issued")
			}

			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return configErr(err)
			}
			if strings.TrimSpace(cfg.BaseURL) == "" {
				return configErr(errors.New("no base_url configured; set it in the config file or COMFYUI_BASE_URL"))
			}

			refJSON, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			fields := map[string]string{
				"type":         assetType,
				"original_ref": string(refJSON),
			}
			if subfolder != "" {
				fields["subfolder"] = subfolder
			}
			if overwrite {
				fields["overwrite"] = "true"
			}

			uploaded, err := comfyMultipartUpload(cmd.Context(), flags, cfg.BaseURL, "/upload/mask", hostPath, filename, fields)
			if err != nil {
				return err
			}

			return comfyUploadMaskEmit(cmd, flags, comfyUploadMaskResult{
				Action:      "upload",
				Executed:    true,
				HostPath:    hostPath,
				GraphValue:  uploaded.Name,
				OriginalRef: ref,
				Name:        uploaded.Name,
				Subfolder:   uploaded.Subfolder,
				Type:        uploaded.Type,
				Note:        comfyMaskAlphaNote,
			})
		},
	}

	cmd.Flags().StringVar(&originalFilename, "original", "", "REQUIRED: the server-side filename the mask's alpha is composited onto")
	cmd.Flags().StringVar(&originalSubfolder, "original-subfolder", "", "Subfolder holding the original")
	cmd.Flags().StringVar(&originalType, "original-type", "input", "Directory the original lives in: input, output, or temp")
	cmd.Flags().StringVar(&subfolder, "subfolder", "", "Subfolder to write the composited mask into")
	cmd.Flags().StringVar(&assetType, "type", "input", "Directory to write into: input, output, or temp")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite an existing file instead of letting the server rename it")
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually POST the upload. Without this the call is printed and nothing is sent")

	return cmd
}

func comfyUploadMaskEmit(cmd *cobra.Command, flags *rootFlags, result comfyUploadMaskResult) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, result)
	}
	w := cmd.OutOrStdout()
	if !result.Executed {
		fmt.Fprintf(w, "would POST /upload/mask  %s\n", result.HostPath)
	} else {
		fmt.Fprintf(w, "uploaded mask  %s\n", result.Name)
	}
	fmt.Fprintf(w, "  original:  %s (type=%s subfolder=%q)\n",
		result.OriginalRef.Filename, result.OriginalRef.Type, result.OriginalRef.Subfolder)
	fmt.Fprintf(w, "  writes to: type=%s subfolder=%q\n", result.Type, result.Subfolder)
	fmt.Fprintf(w, "  note: %s\n", result.Note)
	return nil
}

// ---------------------------------------------------------------------------
// shared multipart upload
// ---------------------------------------------------------------------------

// comfyMultipartUpload POSTs a multipart/form-data file upload to one of
// ComfyUI's upload endpoints and decodes the {name, subfolder, type} reply.
//
// This does not go through internal/client: that client marshals every body as
// JSON and has no multipart path. The file is streamed through an io.Pipe so a
// multi-hundred-megabyte input is never buffered in memory.
//
// Generalised out of stageUploadImage when `upload mask` arrived, so both
// endpoints share ONE multipart implementation — the pipe teardown, the
// error-status handling and the empty-name guard are exactly the places where
// two copies would quietly diverge. endpoint is the path ("/upload/image",
// "/upload/mask"); fields are the extra form values, which differ per endpoint
// (mask requires original_ref, image does not).
func comfyMultipartUpload(ctx context.Context, flags *rootFlags, baseURL, endpoint, hostPath, filename string, fields map[string]string) (stageUploadResponse, error) {
	f, err := os.Open(hostPath)
	if err != nil {
		return stageUploadResponse{}, fmt.Errorf("opening %s: %w", hostPath, err)
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var writeErr error
		defer func() { _ = pw.CloseWithError(writeErr) }()
		part, err := mw.CreateFormFile("image", filename)
		if err != nil {
			writeErr = err
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			writeErr = err
			return
		}
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				writeErr = err
				return
			}
		}
		writeErr = mw.Close()
	}()

	target := strings.TrimRight(baseURL, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, pr)
	if err != nil {
		return stageUploadResponse{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "comfyui-pp-cli/"+version)

	timeout := time.Duration(0)
	if flags != nil && flags.timeout > 0 {
		timeout = flags.timeout
	}
	httpClient := &http.Client{Timeout: timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return stageUploadResponse{}, unreachableErr(fmt.Errorf("uploading %s to %s: %w", filepath.Base(hostPath), target, err))
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stageUploadResponse{}, apiErr(fmt.Errorf("upload rejected: HTTP %d from %s: %s",
			resp.StatusCode, target, strings.TrimSpace(string(body))))
	}
	if readErr != nil {
		return stageUploadResponse{}, apiErr(fmt.Errorf("reading upload response: %w", readErr))
	}
	var parsed stageUploadResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return stageUploadResponse{}, apiErr(fmt.Errorf("upload response was not JSON: %s", strings.TrimSpace(string(body))))
	}
	if strings.TrimSpace(parsed.Name) == "" {
		return stageUploadResponse{}, apiErr(fmt.Errorf("upload response carried no filename: %s", strings.TrimSpace(string(body))))
	}
	return parsed, nil
}
