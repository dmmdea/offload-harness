package main

// F35 made `gpu --config <path> <verb>` reach the verb with its --config in front, but the dispatcher only
// hoists: every verb must then DEFINE the flag itself. `gpu owner-flags` defined only --pid, so
// `gpu --config x owner-flags` was refused by its flag set (flag.ExitOnError, exit 2) while the other six verbs
// took it (0.180.0 media-branch final check). The flag sets exit the process, so the pin runs each verb in a
// child (this test binary, TestHelperGPUVerbDriver) with `--help`: a flag set that knows --config prints its
// usage and exits 0; one that does not exits 2 with "flag provided but not defined".

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// TestHelperGPUVerbDriver runs `gpu` with the arguments in LO_GPU_VERB_ARGS (one per line). A no-op unless set.
func TestHelperGPUVerbDriver(t *testing.T) {
	raw := os.Getenv("LO_GPU_VERB_ARGS")
	if raw == "" {
		return
	}
	if err := runGPU(strings.Split(raw, "\n")); err != nil {
		os.Stderr.WriteString("driver: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Exit(0)
}

func TestEveryGPUVerbAcceptsTheHoistedConfig(t *testing.T) {
	cfg := t.TempDir() + "/config.json"
	for _, verb := range gpuVerbs {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			// Form 1: the global flag in front of the verb, which the dispatcher moves behind it. Form 2: the flag
			// after the verb, which always worked. Both must reach a flag set that defines --config.
			for name, args := range map[string][]string{
				"before the verb": {"--config", cfg, verb, "--help"},
				"after the verb":  {verb, "--config", cfg, "--help"},
			} {
				cmd := winexec.Command(os.Args[0], "-test.run=^TestHelperGPUVerbDriver$", "-test.timeout=60s")
				cmd.Env = append(os.Environ(), "LO_GPU_VERB_ARGS="+strings.Join(args, "\n"))
				done := make(chan struct{})
				var out []byte
				var err error
				go func() {
					out, err = cmd.CombinedOutput()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(70 * time.Second):
					t.Fatalf("gpu %s: %s: the child never ended", verb, name)
				}
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					t.Errorf("gpu %s (--config %s): exit %d, want 0 (%v):\n%s", verb, name, ee.ExitCode(), args, out)
					continue
				}
				if err != nil {
					t.Fatalf("gpu %s: %v", verb, err)
				}
				if strings.Contains(string(out), "not defined") {
					t.Errorf("gpu %s (--config %s): its flag set does not define --config:\n%s", verb, name, out)
				}
			}
		})
	}
}
