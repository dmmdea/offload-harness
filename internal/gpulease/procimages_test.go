package gpulease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunningImagesFindsThisProcessByPid(t *testing.T) {
	self := os.Getpid()
	imgs, err := RunningImages(func(pid int, name string) string {
		if pid == self {
			return "test"
		}
		return ""
	})
	if err != nil {
		t.Skipf("process listing unavailable here: %v", err)
	}
	if len(imgs) != 1 || imgs[0].PID != self || imgs[0].Why != "test" {
		t.Fatalf("want exactly this process, got %+v", imgs)
	}
	exe, _ := os.Executable()
	if !strings.EqualFold(filepath.Base(imgs[0].Path), filepath.Base(exe)) {
		t.Fatalf("image %q is not this test binary %q", imgs[0].Path, exe)
	}
}
