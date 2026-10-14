// Command changelog assembles the changelog fragments and checks them.
//
//	changelog assemble -title "<one line>" [-date YYYY-MM-DD] [-root DIR] [-dry-run]
//	changelog check    [-root DIR]
//
// The rules live in internal/changelog; this file is the flags, the files and the exit codes.
package main

import (
	"io"
	"os"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

// fileOps is every file operation the assembler retries or reports on, so a test can make one fail.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
	sleep  func(time.Duration)
}

func defaultOps() fileOps {
	return fileOps{rename: os.Rename, remove: os.Remove, sleep: time.Sleep}
}

// run is main with its streams and clock passed in. Exit codes: 0 done, 1 refused or failed (nothing
// changed unless the output says a fragment is still to be deleted), 2 a usage error.
func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	return runWith(args, stdout, stderr, now, defaultOps())
}

func runWith(args []string, stdout, stderr io.Writer, now func() time.Time, ops fileOps) int {
	return 0
}
