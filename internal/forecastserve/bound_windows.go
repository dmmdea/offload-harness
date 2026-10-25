//go:build windows

package forecastserve

import (
	"os/exec"

	"github.com/dmmdea/offload-harness/internal/proctree"
)

// startBound starts cmd inside a kill-on-close job object: the child ends when this process
// does, however it ends (llama-swap stops a seat with TerminateProcess on the launcher, which
// closes the job's last handle). stop ends the whole tree now, python having no SIGTERM here.
func startBound(cmd *exec.Cmd) (release, stop func(), err error) {
	t, err := proctree.Start(cmd, proctree.Options{DieWithWrapper: true})
	if err != nil {
		return nil, nil, err
	}
	return func() { _ = t.Close() }, func() { _ = t.Terminate() }, nil
}
