package gpualloc

import (
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// TestMain lets this test binary act as nvidia-smi when a test installs the stand-in
// (internal/gpuprobe/smitest): the copy of the binary that is exec'd as nvidia-smi plays the part and
// exits before any test runs.
func TestMain(m *testing.M) {
	smitest.MaybeRun()
	os.Exit(m.Run())
}
