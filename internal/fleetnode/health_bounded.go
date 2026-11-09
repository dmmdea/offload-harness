package fleetnode

// /fleet/health must answer quickly while the node is busy. The handler's reads that touch the disk (the media
// route verdicts, the image recipe rows, the video/animation/audio family rows) are memoised for a minute, but the
// request that finds a memo expired used to rebuild it inline: on a node that is loading multi-GB weights the same
// stats queue behind the load, and the asker's probe timed out although the node was fine. A read here runs on its
// own goroutine, one at a time; the request waits for it at most healthReadBound, and a slower rebuild is served
// as the LAST KNOWN value with its age in the payload's stale_reads (never silently), the rebuild finishing
// behind it for the next request. Only a read that has never completed has nothing to fall back on, so it is
// waited for in full, which is what every request did before.

import (
	"log"
	"sync"
	"time"
)

// healthReadBound is how long one health request waits for a memoised read to rebuild before it serves the last
// known value. It sits well under every health client's own budget (see residencyWaitBound) and over the few
// milliseconds a rebuild takes on an idle disk, so a healthy node never serves a stale read. A var so tests can
// shorten it.
var healthReadBound = 250 * time.Millisecond

// StaleRead is one entry of /fleet/health stale_reads: a reading the node served from its last known value because
// a fresh one was not ready in time. Name is the payload part it feeds, AgeMs how long ago that value was taken.
type StaleRead struct {
	Name  string `json:"name"`
	AgeMs int64  `json:"age_ms"`
}

// Names of the readings that can be served stale.
const (
	staleMediaRoutes   = "media_routes"
	staleImageRecipes  = "image_recipes"
	staleMediaRecipes  = "media_recipes"
	staleAgentResident = "agent_residency"
)

// lastKnown holds the last completed result of one slow read and the single-flight latch of the read in the air.
type lastKnown[T any] struct {
	mu     sync.Mutex
	val    T
	at     time.Time
	have   bool
	flight chan struct{} // non-nil while a read runs; closed when it ends
}

// read runs fn (unless one is already running) and returns its result, waiting at most bound once a previous result
// exists. stale is true when the result returned is the previous one, age being how old it is.
func (l *lastKnown[T]) read(bound time.Duration, fn func() T) (val T, age time.Duration, stale bool) {
	l.mu.Lock()
	done := l.flight
	if done == nil {
		done = make(chan struct{})
		l.flight = done
		go func() {
			var v T
			ok := false
			defer func() {
				if r := recover(); r != nil {
					log.Printf("fleet: health read panicked: %v", r)
				}
				l.mu.Lock()
				if ok {
					l.val, l.at, l.have = v, time.Now(), true
				}
				l.flight = nil
				l.mu.Unlock()
				close(done)
			}()
			v = fn()
			ok = true
		}()
	}
	have := l.have
	l.mu.Unlock()
	if !have {
		<-done
	} else {
		t := time.NewTimer(bound)
		select {
		case <-done:
		case <-t.C:
		}
		t.Stop()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	val = l.val
	select {
	case <-done:
	default:
		if l.have {
			stale, age = true, time.Since(l.at)
		}
	}
	return val, age, stale
}

// staleReads collects the StaleRead entries of one health request.
type staleReads []StaleRead

func (s *staleReads) note(name string, age time.Duration) {
	*s = append(*s, StaleRead{Name: name, AgeMs: age.Milliseconds()})
}

// ageBeyondTTL reports how old the cached residency answer is when it is older than its TTL (the refresh is then in
// the air or failing, and the answer being served is the previous one). Never taken, or still inside the TTL, is
// not stale: a cache that was never taken is the unknown the payload already says by omitting seat_loaded.
func (a *agentResidency) ageBeyondTTL() (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.at.IsZero() {
		return 0, false
	}
	age := time.Since(a.at)
	return age, age > agentResidencyTTL
}
