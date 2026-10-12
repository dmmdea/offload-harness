package main

// The three lists of tempdir_lint_test.go. Regenerate the keys with TEMPDIR_LINT_PRINT=1 (the command is in that file),
// then write each reason after reading the code: the reason is the review.

// tempUseAllowlist: every direct use of the OS temp dir in a test file, as "file:form" with its count, and why the folder
// it makes is gone on every path (a failed test and a panic included) or is never made. Listed at the 0.182.0 tip, after
// F77, F77b and F79 landed. A suite root is one folder per suite run (the run's own TestMain makes it, its deferred
// removal ends it); a run killed from outside is the one case no test code can clean, and the tripwire reports it.
var tempUseAllowlist = map[string]tempAllow{
	"internal/delegate/stateisolation_test.go:value:os.MkdirTemp":    {1, "TestMain hands os.MkdirTemp to runIsolated, which makes the suite's one state root and removes it in a defer when the run ends; no test of this package starts a copy of the test binary (os.Executable only hashes the running one), so no second root is made"},
	"internal/exemplars/exemplars_test.go:os.MkdirTemp(empty)":       {1, "tmpDir registers os.RemoveAll with t.Cleanup on the line after it makes the folder, and cleanups run when the test ends by failure or panic too (t.TempDir would say the same in one line: a follow-up outside this lint's file set)"},
	"internal/fleetnode/compose_project_test.go:os.MkdirTemp(empty)": {1, "projectPayload packs into a folder it removes with a deferred RemoveAll before it returns, panic included; it also runs from this package's init, which is why the package holds TestARelaunchedTestBinaryLeavesNothingInTheTempDir"},
	"internal/mcpserver/stateisolation_test.go:value:os.MkdirTemp":   {1, "TestMain hands os.MkdirTemp to runIsolated, which makes the suite's one state root and removes it in a defer when the run ends; no test of this package starts a copy of the test binary"},
	"internal/mediaremote/stateisolation_test.go:value:os.MkdirTemp": {1, "TestMain hands os.MkdirTemp to runIsolated, which makes the suite's one state root and removes it in a defer when the run ends; a started copy shares that root (inheritedSuiteRoot) and TestARelaunchedTestBinaryLeavesNothingInTheTempDir proves it makes none"},
	"internal/pairworkloads/main_test.go:value:os.MkdirTemp":         {1, "TestMain hands os.MkdirTemp to runIsolated, which makes the suite's one state root and removes it in a defer when the run ends; a started copy shares that root (inheritedSuiteRoot) and TestARelaunchedTestBinaryLeavesNothingInTheTempDir proves it makes none"},
	"internal/pipeline/leaseisolation_test.go:os.MkdirTemp(empty)":   {1, "the suite's one state root, made by TestMain and removed after m.Run; the re-launched browse sidecar copy is recognised first (isBrowseSidecarCopy) and makes none, which TestARelaunchedTestBinaryLeavesNothingInTheTempDir proves"},
	"internal/pipeline/pipeline_vision_test.go:os.TempDir":           {1, "joins a lock path that must stay absent: the vision path only reads it and nothing creates the folder, so no folder is made"},
	"main_state_isolation_test.go:value:os.MkdirTemp":                {1, "TestMain hands os.MkdirTemp to runIsolated, which makes the suite's one state root and removes it in a defer when the run ends; a started copy shares that root (inheritedSuiteRoot) and TestARelaunchedTestBinaryLeavesNothingInTheTempDir proves it makes none"},
}

// relaunchExempt: every package whose tests start their own test binary (os.Args[0] or os.Executable) and holds no
// TestARelaunchedTestBinaryLeavesNothingInTheTempDir, with the reason a started copy leaves nothing. The lint also fails an
// entry here when the package's TestMain, init functions or package-level variables themselves use the OS temp dir.
var relaunchExempt = map[string]string{
	"internal/audioio":     "a started copy is a stand-in ffmpeg: TestMain (AUDIOIO_FAKE_FFMPEG set) prints a fixed message and exits, and the init in opus_test.go (binary named ffmpeg) writes only the path its caller names; both end before any test and make no folder",
	"internal/comfyinst":   "the copy runs the one test TestTerminateStopsARealProcess, which only sleeps as the process under test; the package has no TestMain and no init",
	"internal/fakeexe":     "a started copy is a stand-in executable: the init answers by file name, writes its last argument and exits before any test; Place and Dir keep every stand-in inside the calling test's own temp dir, which TestAHelperCallStaysInsideTheTestsOwnTempDirAndIsGoneWhenItsTestEnds pins",
	"internal/gpuactivity": "the copy runs TestHelperExitNow, which exits 0 at once; the package has no TestMain and no init",
	"internal/gpugen":      "a started copy (the engine-spec variable set) runs the stub engine, which writes only the outputs its spec names, and exits in TestMain before any test",
	"internal/gpulease":    "a started copy (GPULEASE_HELPER_ROOT or GPULEASE_HELPER_HOLD_MIB set) runs a helper in TestMain that works in the lease root its parent names, a t.TempDir, and exits before any test; the other uses only read the image path of the running binary",
	"internal/hfinstall":   "the copy runs TestHelperProcess, which prints two JSON lines and exits 1; the package has no TestMain and no init",
	"internal/mediaops":    "a started copy (MEDIAOPS_FAKE_TOOL set) acts as the fake tool in TestMain, writes only the output path its argv names and exits before any test",
	"internal/sandbox":     "a started copy is the cage worker or an escape probe: TestMain applies the cage and execs the command it is given, or prints one result line, and exits before any test; neither makes a folder",
	"internal/sttremote":   "a started copy under the name ffmpeg writes its last argument in the init and exits before any test; the package has no TestMain",
}

// relaunchNeverStarted: every package whose tests refer to the path of their own test binary but never start a copy of
// it, with what they do with the path. The lint fails an entry when a test of the package hands that path to
// exec.Command, exec.CommandContext, their winexec twins or os.StartProcess (a path passed to some other function is not followed).
var relaunchNeverStarted = map[string]string{
	"internal/delegate": "os.Executable only names the running test binary so that it can be hashed (hashOwnExecutable); no copy is started, which is why its TestMain may make the suite's state root without a copy-sharing rule",
	"internal/nodeswap": "os.Executable only names this process's image for a lookup of running processes (platformDeps' finder); no copy is started",
}
