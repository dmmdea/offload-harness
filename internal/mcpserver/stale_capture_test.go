package mcpserver

// stale_capture_test.go: a start-up capture that holds no stamp does not start the guard from "now" (0.184.0 integration
// review, silent failure hunt). The guard seeds from the identity the process captured first thing. When that capture holds
// a path and no size (its open or its stat failed), the guard used to fall back to the stamp of its own file as of mcpserver.New,
// which is after the config was loaded and the pipeline opened: a deploy landing in between was taken for the image the process
// runs, nothing was reported and no line said the fallback had happened. The guard now says it is inactive, in the log once and
// on every door that carries stale_binary_guard.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

func TestAGuardWhoseCaptureHoldsNoStampIsInactiveNotStampedNow(t *testing.T) {
	logged := captureStaleLog(t)
	path := t.TempDir() + "/local-offload.exe"
	st := installImage(t, path, "0.183.0")
	st.Size = 0 // the capture's open (or stat) failed: it recorded where it looked and no stamp
	st.SHA256 = ""
	old := startupIdentity
	startupIdentity = func() (buildinfo.Startup, bool) { return st, true }
	t.Cleanup(func() { startupIdentity = old })

	swapImage(t, path, imageBody("0.184.0")) // a deploy lands between the capture and New

	s := delegateTestServer(t, staleLocalRun())
	if s.exe.known {
		t.Fatal("the guard took the stamp of the file as of New for the image the process started from")
	}
	if state := s.exe.guardState(); !strings.HasPrefix(state, "inactive:") || !strings.Contains(state, "capture") {
		t.Fatalf("guardState = %q, want inactive, naming the capture that held no stamp", state)
	}
	if got := logged.String(); !strings.Contains(got, "stale-binary guard is inactive") || !strings.Contains(got, "capture") {
		t.Fatalf("log %q: want one line saying the guard is inactive because the capture held no stamp", got)
	}
	if note := s.staleBinaryNote(buildinfo.Version); note != "" {
		t.Fatalf("an inactive guard accused: %q", note)
	}
}

// A capture that could not resolve its own path holds nothing to compare with either.
func TestAGuardWhoseCaptureHoldsNoPathIsInactiveToo(t *testing.T) {
	captureStaleLog(t)
	old := startupIdentity
	startupIdentity = func() (buildinfo.Startup, bool) { return buildinfo.Startup{Version: buildinfo.Version}, true }
	t.Cleanup(func() { startupIdentity = old })
	s := delegateTestServer(t, staleLocalRun())
	if s.exe.known || !strings.HasPrefix(s.exe.guardState(), "inactive:") {
		t.Fatalf("known=%v guardState=%q, want an inactive guard", s.exe.known, s.exe.guardState())
	}
}
