package main

// The signals the wrapper form of `gpu reserve` ends its command on (C02, 2026-10-09).
//
// Go on Windows delivers CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT and CTRL_SHUTDOWN_EVENT to a program that asked
// for them as syscall.SIGTERM, and ends the process once its handler has had its grace (about five seconds
// for a console close). Asking for os.Interrupt alone meant the wrapper of a 7 hour render died with its
// console and said nothing: its PAIR card stayed open until a fleet-node sweep closed it "harness process
// exited before the job finished", and its lease was found stale. Asking for SIGTERM too lets the wrapper
// kill its command, close the card with the true cause and release the lease inside that grace. On Linux and
// macOS the same signal is a plain `kill`. A `taskkill /F` or a SIGKILL gives the process no signal, and
// nothing in-process can answer it: the orphan sweep's text covers that case (pairworkloads.orphanText).

import (
	"fmt"
	"os"
	"syscall"
)

// wrapperSignals are the signals the wrapper asks for while its command runs.
var wrapperSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// wrapperSignalFeed is a test seam: when set, it is handed the wrapper's signal channel once the wrapper is
// listening, so a test can deliver the signal a console close would (a real one cannot be driven from a test,
// and Windows cannot send SIGTERM to a process). Production leaves it nil.
var wrapperSignalFeed func(chan<- os.Signal)

// endsGraceBounded reports whether the signal is one after which the OS ends the process in a few seconds:
// SIGTERM, which is how a console close, a logoff and a shutdown reach a Go program on Windows. The wrapper
// then releases its lease at once and leaves the seat's warm-back owed instead of doing it first.
func endsGraceBounded(s os.Signal) bool { return s == syscall.SIGTERM }

// warmSkippedOnTermSentence is what the wrapper says in place of the warm-back.
const warmSkippedOnTermSentence = "gpu reserve: the command was ended on a termination signal (its console was closed, or the session is ending or the machine is shutting down), and the OS gives that about five seconds: releasing the lease now and NOT warming the seat back; the warm stays owed (`gpu status` shows it) and the seat loads on its next request, or with `gpu release --warm-seat`"

// signalWhy is the clause the wrapper appends to the exit status on the card of a command it ended on a
// signal. " (interrupted)" is what a Ctrl-C always read.
func signalWhy(s os.Signal) string {
	switch s {
	case os.Interrupt:
		return " (interrupted)"
	case syscall.SIGTERM:
		return " (terminated: its console was closed, or the session is ending or the machine is shutting down)"
	}
	return fmt.Sprintf(" (ended on signal %v)", s)
}
