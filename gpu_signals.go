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
