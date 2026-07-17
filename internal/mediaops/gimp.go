package mediaops

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// gimpMu serializes ALL headless GIMP invocations in this process: concurrent
// gimp-console instances contend on the profile-directory lock (research pitfall
// 2026-07-16). flatten_design and instantiate_design share the exposure, so the
// mutex lives here where both paths run the console.
var gimpMu sync.Mutex

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

// schemeString escapes an arbitrary text value for a TinyScheme string literal
// (backslashes then quotes). Unlike paths, template copy may legitimately
// contain quotes — escape, don't reject.
func schemeString(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

// BuildInstantiateScript renders the instantiate_design script-fu: open the
// .xcf/.psd template, set named text layers' copy (gimp-text-layer-set-text),
// swap named pixel layers for replacement images (loaded via
// gimp-file-load-layer, inserted at the old layer's offsets, old removed),
// flatten, save the raster to dst. Pure; deterministic (layer names sorted).
// A layer-name mismatch errors inside GIMP (gimp-image-get-layer-by-name) and
// surfaces on stderr — the single most common template failure.
// PDB names verified against gimp-console-3.2 (2026-07-17): shipped 3.2 scripts
// use gimp-drawable-get-offsets (3.x rename), insert-layer parent 0.
func BuildInstantiateScript(src, dst string, setText, replaceImage map[string]string) (string, error) {
	lower := strings.ToLower(src)
	if !strings.HasSuffix(lower, ".xcf") && !strings.HasSuffix(lower, ".psd") {
		return "", fmt.Errorf("instantiate_design template must be .xcf or .psd, got %q", src)
	}
	if len(setText)+len(replaceImage) == 0 {
		return "", fmt.Errorf("instantiate_design needs at least one of set_text/replace_image")
	}
	var err error
	if src, err = gimpPath(src); err != nil {
		return "", err
	}
	if dst, err = gimpPath(dst); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `(let* ((image (car (gimp-file-load RUN-NONINTERACTIVE "%s"))))`, src)
	for _, name := range sortedKeys(setText) {
		fmt.Fprintf(&b, ` (gimp-text-layer-set-text (car (gimp-image-get-layer-by-name image "%s")) "%s")`,
			schemeString(name), schemeString(setText[name]))
	}
	for _, name := range sortedKeys(replaceImage) {
		png, perr := gimpPath(replaceImage[name])
		if perr != nil {
			return "", perr
		}
		// Insert at the OLD layer's stack position — NOT -1/top, which covered every
		// layer above it (live E2E 2026-07-17: the replacement hid the headline text
		// layer) — and scale the replacement to the old layer's bounds so an
		// oversized image cannot swallow the canvas.
		fmt.Fprintf(&b, ` (let* ((old (car (gimp-image-get-layer-by-name image "%s")))`+
			` (new (car (gimp-file-load-layer RUN-NONINTERACTIVE image "%s")))`+
			` (off (gimp-drawable-get-offsets old))`+
			` (pos (car (gimp-image-get-item-position image old))))`+
			` (gimp-image-insert-layer image new 0 pos)`+
			` (gimp-layer-scale new (car (gimp-drawable-get-width old)) (car (gimp-drawable-get-height old)) FALSE)`+
			` (gimp-layer-set-offsets new (car off) (cadr off))`+
			` (gimp-image-remove-layer image old))`,
			schemeString(name), png)
	}
	fmt.Fprintf(&b, ` (gimp-image-flatten image) (gimp-file-save RUN-NONINTERACTIVE image "%s"))`, dst)
	return b.String(), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
