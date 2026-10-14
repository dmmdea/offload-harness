package mediacap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// ---- fixtures --------------------------------------------------------------------------------------

func putSized(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// comfyWith makes a ComfyUI install whose models dir holds the given class/name files, each of the size given.
func comfyWith(t *testing.T, files map[string]int) string {
	t.Helper()
	comfy := t.TempDir()
	for rel, n := range files {
		putSized(t, filepath.Join(comfy, "models", filepath.FromSlash(rel)), n)
	}
	return comfy
}

// wanFiles are the four files the Wan 2.2 graph loads by builder default, with a distinct size each.
func wanFiles() map[string]int {
	return map[string]int{
		"diffusion_models/" + wanDefaults.highUnet: 11,
		"diffusion_models/" + wanDefaults.lowUnet:  22,
		"text_encoders/" + wanDefaults.textEncoder: 33,
		"vae/" + wanDefaults.vae:                   44,
	}
}

func ltxFiles() map[string]int {
	return map[string]int{
		"diffusion_models/" + ltxDefaults.transformer:         101,
		"text_encoders/" + ltxDefaults.textEncoder:            102,
		"vae/" + ltxDefaults.videoVae:                         103,
		"vae/" + ltxDefaults.audioVae:                         104,
		"latent_upscale_models/" + ltxDefaults.latentUpscaler: 105,
	}
}

func merge(ms ...map[string]int) map[string]int {
	out := map[string]int{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func rowsOf(rows []LaneRow, lane string) []LaneRow {
	var out []LaneRow
	for _, r := range rows {
		if r.Lane == lane {
			out = append(out, r)
		}
	}
	return out
}

func rowNamed(t *testing.T, rows []LaneRow, lane, name string) LaneRow {
	t.Helper()
	for _, r := range rows {
		if r.Lane == lane && r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s row named %q in %+v", lane, name, rows)
	return LaneRow{}
}

func fileSizes(r LaneRow) []int64 {
	var out []int64
	for _, f := range r.Files {
		out = append(out, f.Bytes)
	}
	return out
}

func fileRoles(r LaneRow) string {
	var out []string
	for _, f := range r.Files {
		out = append(out, f.Role)
	}
	return strings.Join(out, ",")
}

func sizesEqual(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- video, ComfyUI --------------------------------------------------------------------------------

// A node whose video binding is a ComfyUI script publishes its default family with the files its graph loads and how big each
// is on this node. The default is Wan 2.2 when videogen_family names nothing; the other families are not listed (the node has
// neither a videogen_families entry nor their files for them).
func TestLaneRowsTheDefaultVideoFamilyListsItsFilesWithSizes(t *testing.T) {
	cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfyWith(t, wanFiles())}
	rows := rowsOf(LaneRows(cfg), LaneVideo)
	if len(rows) != 1 {
		t.Fatalf("one family bound and present, want one video row, got %+v", rows)
	}
	r := rows[0]
	if r.Name != "wan22" || !r.Default || r.Engine != "comfyui" || r.Digest != "" {
		t.Errorf("row = %+v, want the default wan22 ComfyUI row with no digest", r)
	}
	if got := fileRoles(r); got != "unet_high,unet_low,text_encoder,wan vae" {
		t.Errorf("roles = %q", got)
	}
	if !sizesEqual(fileSizes(r), 11, 22, 33, 44) {
		t.Errorf("sizes = %v, want the sizes of the files on disk", fileSizes(r))
	}
	if r.Files[0].Name != wanDefaults.highUnet || r.Files[3].Name != wanDefaults.vae {
		t.Errorf("names = %+v", r.Files)
	}
}

// A binding whose weight is missing is still listed, with bytes -1, so a delegator can name what the node lacks. The default
// family is listed even when none of its files is on the node.
func TestLaneRowsAMissingFileKeepsBytesMinusOneAndTheRowStays(t *testing.T) {
	files := wanFiles()
	delete(files, "text_encoders/"+wanDefaults.textEncoder)
	cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfyWith(t, files)}
	r := rowNamed(t, LaneRows(cfg), LaneVideo, "wan22")
	if !sizesEqual(fileSizes(r), 11, 22, -1, 44) {
		t.Errorf("sizes = %v, want -1 for the missing text encoder only", fileSizes(r))
	}
	bare := config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: t.TempDir()}
	r = rowNamed(t, LaneRows(bare), LaneVideo, "wan22")
	if !sizesEqual(fileSizes(r), -1, -1, -1, -1) {
		t.Errorf("a node that holds none of the default family's files still lists it: %v", fileSizes(r))
	}
}

// Another ComfyUI family is a row only when the node has bound it (a videogen_families entry, even one whose files are not
// there yet) or holds every file its graph loads. A family neither bound nor complete is absent, which is what a delegator
// reads as "this node does not render it".
func TestLaneRowsAnotherFamilyOnlyWhenBoundOrComplete(t *testing.T) {
	base := func(comfy string) config.Config {
		return config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfy}
	}
	// Neither bound nor complete.
	cfg := base(comfyWith(t, wanFiles()))
	if got := rowsOf(LaneRows(cfg), LaneVideo); len(got) != 1 || got[0].Name != "wan22" {
		t.Fatalf("only the default family: %+v", got)
	}
	// Bound by an entry, files not there: listed, every file -1.
	cfg = base(comfyWith(t, wanFiles()))
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"ltx25": {}}
	ltx := rowNamed(t, LaneRows(cfg), LaneVideo, "ltx25")
	if ltx.Default || !sizesEqual(fileSizes(ltx), -1, -1, -1, -1, -1) {
		t.Errorf("ltx25 bound but absent: %+v", ltx)
	}
	// Complete with no entry: listed, with its sizes.
	cfg = base(comfyWith(t, merge(wanFiles(), ltxFiles())))
	ltx = rowNamed(t, LaneRows(cfg), LaneVideo, "ltx25")
	if !sizesEqual(fileSizes(ltx), 101, 102, 103, 104, 105) {
		t.Errorf("ltx25 complete: %v", fileSizes(ltx))
	}
	// Partly there with no entry is not a family this node renders.
	partial := merge(wanFiles(), map[string]int{"diffusion_models/" + ltxDefaults.transformer: 7})
	cfg = base(comfyWith(t, partial))
	for _, r := range rowsOf(LaneRows(cfg), LaneVideo) {
		if r.Name == "ltx25" {
			t.Errorf("a family with some of its files and no binding is not listed: %+v", r)
		}
	}
}

// videogen_family picks the default row. A spelling the runner does not know (the request-level "wan") renders Wan 2.2, so it is
// the wan22 row, exactly as the pipeline and the route table fold it.
func TestLaneRowsTheDefaultRowFollowsVideogenFamily(t *testing.T) {
	cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", VideoGenFamily: "ltx25", ComfyDir: comfyWith(t, merge(wanFiles(), ltxFiles()))}
	rows := rowsOf(LaneRows(cfg), LaneVideo)
	def := ""
	for _, r := range rows {
		if r.Default {
			if def != "" {
				t.Fatalf("two default rows: %+v", rows)
			}
			def = r.Name
		}
	}
	if def != "ltx25" {
		t.Errorf("default = %q, want ltx25 (rows %+v)", def, rows)
	}
	cfg.VideoGenFamily = "wan"
	if r := rowNamed(t, LaneRows(cfg), LaneVideo, "wan22"); !r.Default {
		t.Errorf("videogen_family wan renders Wan 2.2: %+v", r)
	}
}

// The license pair a family declares travels with its row; a family that declares none carries none (unknown, never
// commercial-safe).
func TestLaneRowsTheLicenseComesFromTheFamilysBinding(t *testing.T) {
	yes := true
	cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfyWith(t, wanFiles()),
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"wan22": {License: "Apache-2.0", CommercialUse: &yes}}}
	r := rowNamed(t, LaneRows(cfg), LaneVideo, "wan22")
	if r.License != "Apache-2.0" || r.CommercialUse == nil || !*r.CommercialUse {
		t.Errorf("license pair = %q %v", r.License, r.CommercialUse)
	}
	cfg.VideoGenFamilies = nil
	r = rowNamed(t, LaneRows(cfg), LaneVideo, "wan22")
	if r.License != "" || r.CommercialUse != nil {
		t.Errorf("an undeclared license stays unknown: %q %v", r.License, r.CommercialUse)
	}
}

// ---- video, sd.cpp ---------------------------------------------------------------------------------

// An sd.cpp video family is a row under its own name, with the weight files its binding names, published by file name and
// never by path. Its binary and backend are the node's own business and are not listed.
func TestLaneRowsAnSdcppFamilyIsARowByBasename(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "fastwan-5b-q8.gguf")
	putSized(t, model, 7)
	cfg := config.Config{
		VideoGenFamily: "fastwan",
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"fastwan": {
			Engine: config.EngineSdcpp, SdcppBin: filepath.Join(dir, "sd-cli"), SdcppBackend: "vulkan0",
			SdcppModel: model, SdcppVAE: filepath.Join(dir, "wan2.2-vae.safetensors"),
		}},
	}
	rows := rowsOf(LaneRows(cfg), LaneVideo)
	if len(rows) != 1 {
		t.Fatalf("an sd.cpp-only video lane has one row: %+v", rows)
	}
	r := rows[0]
	if r.Name != "fastwan" || !r.Default || r.Engine != "sdcpp" || r.Digest != "" {
		t.Errorf("row = %+v", r)
	}
	if len(r.Files) != 2 || r.Files[0].Name != "fastwan-5b-q8.gguf" || r.Files[0].Bytes != 7 || r.Files[1].Name != "wan2.2-vae.safetensors" || r.Files[1].Bytes != -1 {
		t.Errorf("files = %+v, want the model (7 bytes) and the missing VAE (-1), by file name", r.Files)
	}
}

// An sd.cpp family may carry the name of a ComfyUI one. It is then the sd.cpp row, and the ComfyUI builder-default files of that
// name (which its renders never load) are not listed beside it.
func TestLaneRowsAnSdcppFamilyNamedLikeAComfyOneReplacesIt(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfyWith(t, wanFiles()),
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"wan22": {Engine: config.EngineSdcpp, SdcppModel: filepath.Join(dir, "w.gguf")}},
	}
	var wan []LaneRow
	for _, r := range rowsOf(LaneRows(cfg), LaneVideo) {
		if r.Name == "wan22" {
			wan = append(wan, r)
		}
	}
	if len(wan) != 1 || wan[0].Engine != "sdcpp" {
		t.Errorf("wan22 must be the sd.cpp row only: %+v", wan)
	}
}

// When the node's default video family is an sd.cpp one, a ComfyUI family that is also listed (its files are on the node and the
// script is bound) is not the default: exactly one row is.
func TestLaneRowsExactlyOneDefaultWhenTheDefaultIsSdcpp(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		VideoGenScript: "render/comfy-video.mjs", ComfyDir: comfyWith(t, wanFiles()), VideoGenFamily: "fastwan",
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"fastwan": {Engine: config.EngineSdcpp, SdcppModel: filepath.Join(dir, "m.gguf")}},
	}
	var defaults []string
	for _, r := range rowsOf(LaneRows(cfg), LaneVideo) {
		if r.Default {
			defaults = append(defaults, r.Name)
		}
	}
	if len(defaults) != 1 || defaults[0] != "fastwan" {
		t.Errorf("default rows = %v, want only the sd.cpp family", defaults)
	}
}

// ---- animation and audio ---------------------------------------------------------------------------

func TestLaneRowsAnimateIsTheFourWanAnimateFiles(t *testing.T) {
	comfy := comfyWith(t, map[string]int{
		"diffusion_models/" + animateDefaults.unet:     5,
		"text_encoders/" + animateDefaults.textEncoder: 6,
		"clip_vision/" + animateDefaults.clipVision:    7,
		"vae/" + animateDefaults.vae:                   8,
	})
	cfg := config.Config{AnimateGenScript: "render/comfy-animate.mjs", ComfyDir: comfy}
	rows := rowsOf(LaneRows(cfg), LaneAnimate)
	if len(rows) != 1 {
		t.Fatalf("one animate row, got %+v", rows)
	}
	r := rows[0]
	if r.Name != "wan-animate2" || !r.Default || r.Engine != "comfyui" || r.Digest != "" || !sizesEqual(fileSizes(r), 5, 6, 7, 8) {
		t.Errorf("row = %+v", r)
	}
	if got := fileRoles(r); got != "unet,text_encoder,clip_vision,vae" {
		t.Errorf("roles = %q", got)
	}
	// The sd.cpp animate lane has no ComfyUI weight to list: no row (a reader takes that as unknown).
	cfg.AnimateGenEngine = config.EngineSdcpp
	if got := rowsOf(LaneRows(cfg), LaneAnimate); len(got) != 0 {
		t.Errorf("an sd.cpp animate lane publishes no ComfyUI row: %+v", got)
	}
	if got := rowsOf(LaneRows(config.Config{ComfyDir: comfy}), LaneAnimate); len(got) != 0 {
		t.Errorf("no animate binding, no row: %+v", got)
	}
}

// Music is the four ACE-Step files; voice has no row in this release (no accessor names the Chatterbox files), and the audio.cpp
// engine has none either.
func TestLaneRowsMusicIsTheAceStepFilesAndVoiceHasNoRow(t *testing.T) {
	comfy := comfyWith(t, map[string]int{
		"diffusion_models/" + aceDefaults.unet: 1, "text_encoders/" + aceDefaults.clip1: 2,
		"text_encoders/" + aceDefaults.clip2: 3, "vae/" + aceDefaults.vae: 4,
	})
	cfg := config.Config{MusicGenScript: "render/comfy-music.mjs", VoiceGenScript: "render/tts.mjs", ComfyDir: comfy}
	rows := rowsOf(LaneRows(cfg), LaneAudio)
	if len(rows) != 1 {
		t.Fatalf("music only: %+v", rows)
	}
	if r := rows[0]; r.Name != "acestep" || !r.Default || r.Engine != "comfyui" || !sizesEqual(fileSizes(r), 1, 2, 3, 4) {
		t.Errorf("row = %+v", r)
	}
	cfg.MusicGenEngine = config.EngineAudiocpp
	if got := rowsOf(LaneRows(cfg), LaneAudio); len(got) != 0 {
		t.Errorf("an audio.cpp music lane publishes no ComfyUI row: %+v", got)
	}
	voiceOnly := config.Config{VoiceGenScript: "render/tts.mjs", TTSEndpoint: "http://192.0.2.5:8000", ComfyDir: comfy}
	if got := rowsOf(LaneRows(voiceOnly), LaneAudio); len(got) != 0 {
		t.Errorf("voice has no row in this release: %+v", got)
	}
}

// ---- what a row never carries ----------------------------------------------------------------------

// Rows leave the machine on /fleet/health, so no name is a path: no absolute path, no drive letter, no backslash and no `..`.
// A ComfyUI name keeps its subfolders (the loader opens it that way).
func TestLaneRowsPublishNoAbsolutePathOrParentOrDrive(t *testing.T) {
	cfg := config.Config{
		VideoGenScript: "render/comfy-video.mjs", ComfyDir: t.TempDir(),
		VideoGenUnetHigh: `C:\models\wan\high.gguf`, VideoGenUnetLow: "/srv/models/low.gguf", VideoGenTextEncoder: "../enc/umt5.safetensors",
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"fastwan": {
			Engine: config.EngineSdcpp, SdcppModel: `D:\weights\fastwan.gguf`, SdcppVAE: "/opt/weights/vae.safetensors",
		}},
	}
	rows := LaneRows(cfg)
	seen := 0
	for _, r := range rows {
		for _, f := range r.Files {
			seen++
			if strings.ContainsAny(f.Name, `\:`) || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "..") {
				t.Errorf("%s/%s publishes %q", r.Lane, r.Name, f.Name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no file rows were built")
	}
	cfg = config.Config{VideoGenScript: "render/comfy-video.mjs", ComfyDir: t.TempDir(), VideoGenUnetHigh: "wan/high_noise.gguf"}
	if r := rowNamed(t, LaneRows(cfg), LaneVideo, "wan22"); r.Files[0].Name != "wan/high_noise.gguf" {
		t.Errorf("a relative name keeps its subfolder: %q", r.Files[0].Name)
	}
}

// A row built from sizes is not an identity: it carries no digest and no content fingerprint, in any lane.
func TestLaneRowsAreNeverAnIdentity(t *testing.T) {
	cfg := config.Config{
		VideoGenScript: "render/comfy-video.mjs", AnimateGenScript: "render/comfy-animate.mjs", MusicGenScript: "render/comfy-music.mjs",
		ComfyDir: comfyWith(t, merge(wanFiles(), map[string]int{"diffusion_models/" + aceDefaults.unet: 1})),
	}
	rows := LaneRows(cfg)
	if len(rowsOf(rows, LaneVideo)) == 0 || len(rowsOf(rows, LaneAnimate)) == 0 || len(rowsOf(rows, LaneAudio)) == 0 {
		t.Fatalf("all three lanes bound: %+v", rows)
	}
	for _, r := range rows {
		if r.Digest != "" {
			t.Errorf("%s/%s has digest %q", r.Lane, r.Name, r.Digest)
		}
		for _, f := range r.Files {
			if f.Fingerprint != "" {
				t.Errorf("%s/%s file %s has fingerprint %q", r.Lane, r.Name, f.Name, f.Fingerprint)
			}
		}
	}
}

func TestLaneRowsNoLaneBoundNoRows(t *testing.T) {
	if got := LaneRows(config.Config{ComfyDir: comfyWith(t, wanFiles())}); len(got) != 0 {
		t.Errorf("files on disk with no binding are no rows: %+v", got)
	}
	if got := LaneRows(config.Config{}); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}
