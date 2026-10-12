// Package tripwirefixture is the module the temp-folder tripwire proves itself on
// (scripts/test-temp-tripwire-selftest.sh and its PowerShell twin). It is a module of
// its own, so no build or test of the repository ever sees it, and each package below
// is one verdict of the tripwire:
//
//	clean    takes its scratch from t.TempDir only                      -> CLEAN
//	leaker   makes three folders in the OS temp dir and removes one     -> LEAK, three tripwire-leak-* entries
//	spawner  starts a copy of its test binary from the temp folder      -> PROCESSES
//	failing  fails a test and leaves nothing                            -> TESTS-FAILED
//	hanger   sleeps for 40 s and leaves nothing                         -> a run stopped while it is in progress
//
// The leaker and the spawner are deliberate defects: they exist to be caught. The hanger is not one: it is the test binary,
// running from the watched folder, that an interrupted run must stop.
package tripwirefixture
