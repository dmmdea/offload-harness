package mcpserver

import (
	"os"
	"path/filepath"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// reviewDiffIO is the filesystem seam of offload_review_diff's diff_path branch: the three
// calls that can fail for a reason a test cannot cause on every platform (the working
// directory cannot be read, a read_root that cannot be made absolute, a confined read that
// returns no document). Production leaves every field nil and gets the real call; a test sets
// the one it wants to break, so each early refusal of the door is pinned instead of assumed.
type reviewDiffIO struct {
	Getwd func() (string, error)
	Abs   func(string) (string, error)
	Read  func(paths []string, readRoot string) ([]core.ContextDoc, error)
}

// reviewIO resolves the seam: this server's overrides over the real calls.
func (s *Server) reviewIO() reviewDiffIO {
	io := reviewDiffIO{Getwd: os.Getwd, Abs: filepath.Abs, Read: delegate.InlineContextPaths}
	if o := s.reviewDiffFS; o != nil {
		if o.Getwd != nil {
			io.Getwd = o.Getwd
		}
		if o.Abs != nil {
			io.Abs = o.Abs
		}
		if o.Read != nil {
			io.Read = o.Read
		}
	}
	return io
}
