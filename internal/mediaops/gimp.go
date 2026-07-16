package mediaops

import (
	"fmt"
	"strings"
)

// GIMP design-file conversion (spec §1 flatten_design): open .xcf/.psd, flatten,
// export a raster, and write the layer listing to a sidecar file. The script is
// built ONLY from validated paths — there is no user-supplied script text, ever
// (spec: raw script-fu pass-through is a permanent non-goal).
//
// Batch contract LIVE-VERIFIED against gimp-console-3.2.4 (2026-07-16):
//   * needs --batch-interpreter=plug-in-script-fu-eval ("No batch interpreter
//     specified" otherwise);
//   * (gimp-file-load RUN-NONINTERACTIVE "src") / (gimp-file-save
//     RUN-NONINTERACTIVE image "dst") — the 2.x drawable arg is gone;
//   * (gimp-image-get-layers image) returns a vector in car;
//   * `display` does NOT reach batch stdout — the layer list is written to a
//     sidecar file via open-output-file instead.

// Layer is one entry of a design file's layer listing.
type Layer struct {
	Name    string `json:"name"`
	Visible bool   `json:"visible"`
}

func gimpPath(p string) (string, error) {
	if strings.ContainsAny(p, "\"\\'") {
		// backslashes are normalized by the caller below; quotes are hostile input
		if strings.ContainsAny(p, "\"'") {
			return "", fmt.Errorf("path %q contains quote characters — rejected", p)
		}
	}
	return strings.ReplaceAll(p, `\`, "/"), nil
}

// BuildGimpScript renders the flatten_design script-fu for (src -> dst raster,
// layer sidecar). Pure. src must be a .xcf or .psd.
func BuildGimpScript(src, dst, layersPath string) (string, error) {
	lower := strings.ToLower(src)
	if !strings.HasSuffix(lower, ".xcf") && !strings.HasSuffix(lower, ".psd") {
		return "", fmt.Errorf("flatten_design source must be .xcf or .psd, got %q", src)
	}
	var err error
	if src, err = gimpPath(src); err != nil {
		return "", err
	}
	if dst, err = gimpPath(dst); err != nil {
		return "", err
	}
	if layersPath, err = gimpPath(layersPath); err != nil {
		return "", err
	}
	return fmt.Sprintf(`(let* ((image (car (gimp-file-load RUN-NONINTERACTIVE "%s"))) (layers (gimp-image-get-layers image)) (port (open-output-file "%s"))) (for-each (lambda (l) (display (string-append "LAYER:" (car (gimp-item-get-name l)) "|" (if (= (car (gimp-item-get-visible l)) TRUE) "visible" "hidden")) port) (newline port)) (vector->list (car layers))) (close-output-port port) (gimp-image-flatten image) (gimp-file-save RUN-NONINTERACTIVE image "%s"))`, src, layersPath, dst), nil
}

// GimpArgs wraps a built script in the gimp-console batch argv (always ends with
// gimp-quit so the console exits).
func GimpArgs(script string) []string {
	return []string{"-i", "--batch-interpreter=plug-in-script-fu-eval", "-b", script, "-b", "(gimp-quit 0)"}
}

// ParseLayerList parses the sidecar layer file ("LAYER:<name>|visible|hidden" per
// line; the name may itself contain pipes — split on the LAST one). Pure.
func ParseLayerList(content string) []Layer {
	var out []Layer
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "LAYER:") {
			continue
		}
		body := strings.TrimPrefix(line, "LAYER:")
		i := strings.LastIndex(body, "|")
		if i < 0 {
			continue
		}
		out = append(out, Layer{Name: body[:i], Visible: body[i+1:] == "visible"})
	}
	return out
}
