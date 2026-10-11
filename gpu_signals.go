package main

// The signals the wrapper form of `gpu reserve` ends its command on (C02, 2026-10-09).
//
// Go on Windows delivers CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT and CTRL_SHUTDOWN_EVENT to a program that asked
// for them as syscall.SIGTERM, and ends the process once its handler has had its grace (about five seconds
// for a console close). Asking for os.Interrupt alone meant the wrapper of a 7 hour render died with its
// console and said nothing: its PAIR card stayed open until a fleet-node sweep closed it "harness process
// exited before the job finished", and its lease was found stale. Asking for SIGTERM too lets the wrapper
// kill its command, close the card saying what ended it (the cause as likely, termCauseLikely) and release the lease without waiting for a seat reload (the warm-back is skipped on this path). On Linux and
// macOS the same signal is a plain `kill`. A `taskkill /F` or a SIGKILL gives the process no signal, and
// nothing in-process can answer it: the orphan sweep's text covers that case (pairworkloads.orphanText).

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// wrapperSignals are the signals the wrapper asks for while its command runs.
var wrapperSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// wrapperSignalFeed is a test seam: when set, it is handed the wrapper's signal channel the moment the wrapper
// starts listening (which is now the first thing it does, before any lease work), so a test can deliver the signal a
// console close would at the stage it wants (a real one cannot be driven from a test, and Windows cannot send
// SIGTERM to a process). Production leaves it nil.
var wrapperSignalFeed func(chan<- os.Signal)

// wrapperCommandStarted is a test seam called once the wrapped command is running, for a test that wants its signal to
// arrive while the command runs (the stage the feed's clock used to start at, before the handler moved up).
var wrapperCommandStarted func()

// wrapperBeforeStart is a test seam called after the drain and before the checkpoint that keeps a signalled wrapper from
// starting its command, handed the watch so a test can deliver a signal and wait until it is recorded: there is no other way
// to land one in that window (it is as wide as the lines between two statements). Production leaves it nil.
var wrapperBeforeStart func(*signalWatch)

// The stages a signal can find the wrapper in before its command runs, worded for the card: the card keeps 140 characters
// (pairworkloads.CardErrorMax) and cuts the rest, so each stage text, with the signal and the cause around it, is asserted to
// fit whole (TestEveryStageTextFitsTheCardWhole). The re-queue after a lease lost during the drain reuses stageQueued.
const (
	stageBeforeCard  = "before it asked for the card"
	stageQueued      = "while queued"
	stageDraining    = "while draining the seat"
	stageBeforeStart = "before its command started"
)

// termCauseLikely is what a SIGTERM most likely is, said as likely: Go on Windows delivers a console close, a logoff and
// a shutdown as SIGTERM, but a plain kill sends the same signal (and on Linux it is the only way SIGTERM comes), so the
// wrapper cannot tell which it was. Text that named one of them as the cause claimed what nothing here can know. Kept short
// on purpose: the card keeps 140 characters (pairworkloads.CardErrorMax) and the longest stage text that reaches it, with
// this clause in it, must fit whole (TestEveryStageTextFitsTheCardWhole).
const termCauseLikely = "likely a console close, a logoff or a shutdown; a plain kill sends it too"

// endedBySignal is the error a wrapper ends with when a signal stopped it at stage, before its command ran: the signal
// named, the cause as likely, and that the command never started. It is also the text of the card.
func endedBySignal(s os.Signal, stage string) error {
	switch s {
	case os.Interrupt:
		return fmt.Errorf("interrupted %s; never started", stage)
	case syscall.SIGTERM:
		return fmt.Errorf("ended on SIGTERM %s (%s); never started", stage, termCauseLikely)
	}
	return fmt.Errorf("ended on signal %v %s; never started", s, stage)
}

// signalEndedWait turns the cancel of a queue wait into the error that says why: when a signal closed the watch and the
// wait answered ErrCancelled, the wait ended on that signal; any other error is the queue's own and is returned as it is.
func signalEndedWait(w *signalWatch, err error, stage string) error {
	if err == nil || !errors.Is(err, gpulease.ErrCancelled) {
		return err
	}
	if sig := w.Signal(); sig != nil {
		return endedBySignal(sig, stage)
	}
	return err
}

// cancelClosed reports whether a cancel channel is closed; a nil one never is.
func cancelClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// signalWatch is the wrapper's one consumer of wrapperSignals, registered BEFORE anything else the wrapper does
// (0.180 final check). It used to be registered after the card was opened, the queue wait and the drain, so a console
// close during `--wait 8h` or a long drain found no handler: Go ended the process on the spot, its PAIR card stayed
// open and its waiter record stayed in the line. Now every stage of the wrapper watches Done and returns on it.
//
// One goroutine reads the signal channel once and closes Done; the stages that block (the queue wait, the card
// choice, the drain) are handed Done, and the command stage selects on it. A second signal is dropped on the
// channel's full buffer: the first one is the news.
//
// Nothing outside this process backs that up for every signal: the OS ends the process after its grace for a console
// close, a logoff and a shutdown only, so a Ctrl-C or a plain kill that comes after the first signal does nothing until the
// stage in progress ends, and the stages that never consult Done run to their own bounds: the plan's card-table read (up to
// 20 s), the allocator's reads, the stop of the ComfyUI instances kept under the lease (up to 60 s) and the seat's warm-back
// (minutes, after a Ctrl-C). The handler also stays installed through that finish (sw.Stop is deferred before it), where the
// base wrapper had stopped listening before its warm-back on the exit-0 path. Before this watch moved up, the stages before
// the lease ran with no handler at all, so Ctrl-C ended the process on the spot with its card open and its waiter in the
// line. Ending the process at once on a second signal, after releasing what is held, would close that gap; it is not done
// here (an exit skips the deferred finish and leaves the instances running), and docs/systems/gpu-lease.md says so.
// A nil *signalWatch is the detached form (no wrapper, nothing to watch) and every method is inert.
type signalWatch struct {
	sigc chan os.Signal
	done chan struct{}
	quit chan struct{}
	once sync.Once

	mu  sync.Mutex
	got os.Signal
}

// watchWrapperSignals starts listening. wrapperSignalFeed, when a test set it, is handed the channel at once.
func watchWrapperSignals() *signalWatch {
	w := &signalWatch{sigc: make(chan os.Signal, 1), done: make(chan struct{}), quit: make(chan struct{})}
	signal.Notify(w.sigc, wrapperSignals...)
	if wrapperSignalFeed != nil {
		go wrapperSignalFeed(w.sigc)
	}
	go func() {
		select {
		case s := <-w.sigc:
			w.mu.Lock()
			w.got = s
			w.mu.Unlock()
			close(w.done)
		case <-w.quit:
		}
	}()
	return w
}

// Done is closed once a signal has arrived; nil (never ready) for the detached form.
func (w *signalWatch) Done() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.done
}

// Signal is the signal that arrived, nil before one did.
func (w *signalWatch) Signal() os.Signal {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.got
}

// Sleep is time.Sleep that a signal cuts short, for the polling loops of the card choice.
func (w *signalWatch) Sleep(d time.Duration) {
	if w == nil {
		time.Sleep(d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-w.done:
	}
}

// Stop ends the listening; safe to call twice.
func (w *signalWatch) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		signal.Stop(w.sigc)
		close(w.quit)
	})
}

// endsGraceBounded reports whether the signal is one after which the OS ends the process in a few seconds:
// SIGTERM, which is how a console close, a logoff and a shutdown reach a Go program on Windows. The wrapper
// then releases its lease at once and leaves the seat's warm-back owed instead of doing it first.
func endsGraceBounded(s os.Signal) bool { return s == syscall.SIGTERM }

// warmSkippedOnTerm is what the wrapper says in place of the warm-back. It names the signal and gives the cause as likely
// (termCauseLikely): a SIGTERM is also what a plain kill sends. What the signal ended depends on where it found the wrapper:
// stage "" is a command that was running; any other stage (the texts endedBySignal takes) is a wrapper that never started its
// command, which must not be said to have been ended (the seat's unload may still have gone through, so the warm can be owed).
func warmSkippedOnTerm(stage string) string {
	ended := "the command was ended on SIGTERM"
	if stage != "" {
		ended = "the wrapper was ended on SIGTERM " + stage
		if stage != stageBeforeStart {
			ended += ", before its command started"
		}
	}
	return "gpu reserve: " + ended + " (" + termCauseLikely + "), and a console close gives the process about five seconds: releasing the lease now and NOT warming the seat back; the warm stays owed (`gpu status` shows it) and the seat loads on its next request, or with `gpu release --warm-seat`"
}

// signalWhy is the clause the wrapper appends to the exit status on the card of a command it ended on a
// signal. " (interrupted)" is what a Ctrl-C always read.
func signalWhy(s os.Signal) string {
	switch s {
	case os.Interrupt:
		return " (interrupted)"
	case syscall.SIGTERM:
		return " (ended on SIGTERM: " + termCauseLikely + ")"
	}
	return fmt.Sprintf(" (ended on signal %v)", s)
}
