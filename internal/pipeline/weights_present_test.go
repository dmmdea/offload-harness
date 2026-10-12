package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// presentWeights makes every weight file a lane's route names a real file, and unbinds ffmpeg. CT-54: an engine lane refuses, before
// the lease, a route whose weight is missing (the verdict the fleet node's door has always given), so a fixture that runs a lane
// against a stub runner must bind weights that exist: a config that names `/models/x.gguf` on a machine with no /models is exactly the
// route the lane now refuses. A bound path is replaced by a file of the same base name under dir/models (the stub runners never read
// it); ffmpeg is unbound (only when a weight was bound) so the derivation does not depend on whether the test host has ffmpeg and ffprobe on its PATH (the stub
// runners do not call them either). Callers that assert on the path read it back from the config.
func presentWeights(t *testing.T, dir string, cfg *config.Config) {
	t.Helper()
	bound := false
	fix := func(p *string) {
		if strings.TrimSpace(*p) == "" {
			return
		}
		*p = weightFile(t, dir, filepath.Base(*p))
		bound = true
	}
	// an engine binary bound by a name that resolves to nothing (neither a file nor on PATH) becomes a stub file under dir/bin
	bin := func(p *string) {
		v := strings.TrimSpace(*p)
		if v == "" {
			return
		}
		if _, err := os.Stat(v); err == nil {
			return
		}
		if _, err := exec.LookPath(v); err == nil {
			return
		}
		b := filepath.Join(dir, "bin", filepath.Base(v))
		if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		*p = b
		bound = true
	}
	bin(&cfg.SdcppBin)
	bin(&cfg.AnimateGenSdcppBin)
	bin(&cfg.AnimateGenDepthBin)
	bin(&cfg.AudiocppBin)
	for _, p := range []*string{
		&cfg.SdcppModel, &cfg.SdcppVAE, &cfg.SdcppClipL, &cfg.SdcppClipG, &cfg.SdcppT5, &cfg.SdcppLLM,
		&cfg.AnimateGenSdcppModel, &cfg.AnimateGenSdcppVAE, &cfg.AnimateGenSdcppT5xxl, &cfg.AnimateGenDepthModel,
		&cfg.AudiocppVoiceModel, &cfg.AudiocppMusicModel,
	} {
		fix(p)
	}
	for name, fb := range cfg.VideoGenFamilies {
		bin(&fb.SdcppBin)
		for _, p := range []*string{&fb.SdcppModel, &fb.SdcppHighNoiseModel, &fb.SdcppVAE, &fb.SdcppT5xxl} {
			fix(p)
		}
		cfg.VideoGenFamilies[name] = fb
	}
	if bound {
		cfg.FFmpegPath = ""
	}
}
