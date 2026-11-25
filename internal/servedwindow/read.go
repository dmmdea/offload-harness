package servedwindow

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// LoadedReadTimeout bounds one loaded-only window read: the answer comes from a seat that is already
// resident, so it takes milliseconds, and a seat that does not answer in this long is not worth waiting
// for by a caller that has other work.
const LoadedReadTimeout = 3 * time.Second

// ReadLoaded asks the engine for seat's window and remembers what it says. It is the only engine call in
// this package, and it never loads anything: llama-swap's /running roster is read first and a seat that is
// not resident is answered "no" without a request to it (an /upstream request would start the seat, and
// would restart llama-swap's idle timer on a seat that is up, so a caller must not poll this).
// ok=false is "not read": the seat is cold, the engine did not answer, or it reports no window.
func ReadLoaded(ctx context.Context, cfg config.Config, seat string) (n int, ok bool) {
	seat = strings.TrimSpace(seat)
	if seat == "" || strings.TrimSpace(cfg.Endpoint) == "" {
		return 0, false
	}
	c, err := swapclient.New(cfg.Endpoint, LoadedReadTimeout)
	if err != nil {
		return 0, false
	}
	rctx, cancel := context.WithTimeout(ctx, LoadedReadTimeout)
	defer cancel()
	n, err = c.ContextWindow(rctx, seat)
	if err != nil || n <= 0 {
		return 0, false
	}
	Remember(cfg, seat, n)
	return n, true
}

// RefreshHook is the function a run installs with Loop.WithWindowRefresh when its start-up probe could not
// answer: the loop calls it once, after the seat has answered its first request (so it is loaded), and it
// does one ReadLoaded. A window it reads replaces the one the run began on (was, from source), is
// remembered, is logged as one line, and is handed to note, which the door appends to the run's
// ctx_window_note. A seat that does not answer leaves the run as it was.
func RefreshHook(cfg config.Config, seat string, was int, source string, note func(string)) func(context.Context) (int, bool) {
	return func(ctx context.Context) (int, bool) {
		n, ok := ReadLoaded(ctx, cfg, seat)
		if !ok {
			return 0, false
		}
		line := fmt.Sprintf("context window: %d (re-read from the engine after its load; was %d from %s)", n, was, source)
		log.Printf("agent task: %s", line)
		if note != nil {
			note(line)
		}
		return n, true
	}
}
