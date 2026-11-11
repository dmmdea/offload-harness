package gpulease

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// HostRAMRelief frees memory the host holds for something that can give it back, for a lease that is waiting for host
// RAM. shortGiB is how far the waiting request is from admission (hostRAMShortGiB). It returns a line to print when it
// freed something, "" when it did nothing, and an error only when it tried and failed. The one implementation is
// internal/wslcache (the WSL 2 VM's page cache), installed by config.Load when gpu_hostram_wsl_cache_drop is on.
type HostRAMRelief func(ctx context.Context, shortGiB float64) (string, error)

type reliefHook struct {
	fn  HostRAMRelief
	out io.Writer
}

var installedRelief atomic.Pointer[reliefHook]

// SetHostRAMRelief installs fn for EVERY Manager of this process, the way the headroom is installed
// (SetDefaultHostRAMHeadroom): config.Load calls it, so the CLI, the MCP server and the fleet node all get it from the one
// key. Each line fn returns, and each error, is written to out. A nil fn uninstalls it.
func SetHostRAMRelief(fn HostRAMRelief, out io.Writer) {
	if fn == nil {
		installedRelief.Store(nil)
		return
	}
	if out == nil {
		out = io.Discard
	}
	installedRelief.Store(&reliefHook{fn: fn, out: out})
}

const (
	// reliefInterval is the least time between two attempts by ANY process on the box (the stamp file in the GPU state
	// directory carries it across processes): a film batch queues several leases on one host, and each would otherwise ask
	// on its own tick. A chosen figure.
	reliefInterval = time.Minute
	// reliefTimeout bounds one attempt (a sync can take tens of seconds on a busy disk).
	reliefTimeout = time.Minute
	reliefStamp   = "hostram-relief.stamp"
)

// reliefBusy keeps this process to one attempt at a time.
var reliefBusy atomic.Bool

// relieveHostRAM asks the installed relief, if any, to free memory for a request the host just refused on host-RAM grounds.
// It returns at once and the attempt runs in the background: the caller's loop keeps polling, and in Acquire it keeps the
// waiter's record fresh, which a call blocked past waiterStaleWindow (ten polls) would cost its place in line. The next tick's
// reading of the host is what admits the request; nothing here admits anything.
func (m *Manager) relieveHostRAM(chk gpuprobe.HostRAMCheck) {
	h := installedRelief.Load()
	if h == nil || chk.OK || chk.Impossible || chk.Unreadable {
		return
	}
	short := hostRAMShortGiB(chk)
	if short <= 0 {
		return
	}
	if !reliefBusy.CompareAndSwap(false, true) {
		return
	}
	stamp := filepath.Join(m.gpuDir(), reliefStamp)
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < reliefInterval {
		reliefBusy.Store(false)
		return
	}
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		// Without the stamp every waiter on the box would ask on every tick.
		reliefBusy.Store(false)
		return
	}
	go func() {
		defer reliefBusy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), reliefTimeout)
		defer cancel()
		line, err := h.fn(ctx, short)
		switch {
		case err != nil:
			fmt.Fprintf(h.out, "gpu lease: host RAM relief failed: %v\n", err)
		case line != "":
			fmt.Fprintf(h.out, "gpu lease: %s\n", line)
		}
	}()
}

// hostRAMShortGiB is how much memory the host would have to give back for the refused request to be admitted on the
// reading the refusal rested on: the larger of the touched-set shortfall (headroom less what would be left available) and
// the commit shortfall (the projected charge less its bound). A relief that frees less cannot admit it by itself.
func hostRAMShortGiB(c gpuprobe.HostRAMCheck) float64 {
	short := 0.0
	if c.TouchShort {
		short = max(short, c.HeadroomGiB-c.AvailableAfterGiB)
	}
	if c.CommitShort && c.CommitApplies {
		short = max(short, c.CommitProjectedGiB-c.CommitBoundGiB)
	}
	return short
}
