package comfyinst

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The launch markers are written by render/comfy-ownership.mjs through a staged .tmp file and a rename over the live name. A plain
// os.ReadFile of one fails while that rename runs (on Windows it opens without sharing delete), and a read that meets the instant the
// name is absent answers "not found" for a marker that is there a moment later: a lease's release then leaves its kept instance running,
// and another lease's instance is not spared when a wrapper stops its own tree. Every read of a marker goes through
// gpulease.ReadSharedFile, which waits for the rename (internal/gpulease/gate.go).
func TestNoReadOfALaunchMarkerIsAPlainReadFile(t *testing.T) {
	src, err := os.ReadFile("comfyinst.go")
	if err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile(`\bos\.ReadFile\(`)
	for i, line := range strings.Split(string(src), "\n") {
		code, _, _ := strings.Cut(line, "//")
		if plain.MatchString(code) {
			t.Errorf("comfyinst.go:%d reads a launch marker with os.ReadFile, want gpulease.ReadSharedFile: %s", i+1, strings.TrimSpace(line))
		}
	}
}
