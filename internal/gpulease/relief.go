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

const (
	// HostRAMSettleBound is the longest a caller with no line to wait in settles for (a chosen figure: on the reference box the
	// host's commit took up to about a minute to come back after a seat unload).
	HostRAMSettleBound = 60 * time.Second
	// hostRAMSettleTick is how long HostRAMSettle waits between two readings of the host.
	hostRAMSettleTick = 2 * time.Second
	// hostRAMSettleProgress and hostRAMSettleStall: a settle that has not seen the shortfall fall this far below the lowest it has
	// seen for this long is not going to see the host recover, and stops. Chosen figures.
	hostRAMSettleProgress = 0.25
	hostRAMSettleStall    = 10 * time.Second
)

// HostRAMSettle is for a caller that has no line to wait in and whose own check is what admits it: a contract door's cold load of
// the agent seat. Right after another model unloads, the host's memory takes up to about a minute to come back, and a contract
// that arrives in that window would be refused for a load that fits seconds later. It reads the host with check; a refusal asks
// the installed relief (relieveHostRAM, which rate-limits itself) and reads again every tick. It returns the last reading and the
// time it waited. Nothing here admits anything: the caller acts on the reading it gets back, with the same rule it passed in.
//
// It stops on the first of three exits: the reading is OK, Impossible or Unreadable (waiting changes none of them); bound has
// passed or ctx is done; or the shortfall (hostRAMShortGiB) has not fallen at least 0.25 GiB below the lowest seen for 10 s
// (no progress). bound <= 0 is one look plus the relief trigger on a refusal, with no wait: a caller whose lease is still held
// while it asks (the warm-back at a release) must not hold the others behind it.
func (m *Manager) HostRAMSettle(ctx context.Context, bound time.Duration, check func() gpuprobe.HostRAMCheck) (gpuprobe.HostRAMCheck, time.Duration) {
	chk := check()
	if chk.OK || chk.Impossible || chk.Unreadable {
		return chk, 0
	}
	m.relieveHostRAM(chk)
	if bound <= 0 {
		return chk, 0
	}
	start := m.now()
	// best is the shortfall at the last reading that counted as progress, and bestAt when that was: slow steady recovery
	// (under 0.25 GiB a tick) still adds up to progress, because each reading is judged against best, not the previous one.
	best := hostRAMShortGiB(chk)
	bestAt := start
	for {
		remaining := bound - m.now().Sub(start)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		m.pauseOrCancel(min(hostRAMSettleTick, remaining), ctx.Done())
		if ctx.Err() != nil {
			break
		}
		chk = check()
		if chk.OK || chk.Impossible || chk.Unreadable {
			break
		}
		m.relieveHostRAM(chk)
		now := m.now()
		if short := hostRAMShortGiB(chk); short <= best-hostRAMSettleProgress {
			best, bestAt = short, now
		} else if now.Sub(bestAt) >= hostRAMSettleStall {
			break
		}
	}
	return chk, m.now().Sub(start)
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
