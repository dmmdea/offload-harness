package gpugen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// A full volume is its own class (0.178.0): a batch that stopped on a full drive used to be recorded
// as "other". The rule is the one the render helpers use (isDiskFullError in render/batch-jobs.mjs):
// the TYPED errno of a Go file call, or the errno TOKEN in the text of a child's output; never the
// prose around it, because a failure's text can carry text that is not the failure.
func TestClassifyErrDiskFullByErrnoToken(t *testing.T) {
	cases := []struct{ msg, want string }{
		// what a render helper's output tail carries (the last 400 bytes of a gpugen failure)
		{"gpugen: comfy-generate.mjs failed: exit status 1 (IMAGE BATCH FAILED: the disk is full at job 2/4, writing renders/b.png (comfy-render exited 1: ENOSPC: no space left on device, write (writing renders/b.png)); 2 jobs not run, recorded as such)", "disk_full"},
		{"gpugen: comfy-render.mjs failed: exit status 1 (RENDER FAILED: EDQUOT: disk quota exceeded, write (writing a.png))", "disk_full"},
		{"gpugen: comfy-render.mjs failed: exit status 1 (RENDER FAILED: EROFS: read-only file system, open 'a.png.partial-1-1')", "disk_full"},
		// ComfyUI's own Python reports the number
		{"ComfyUI exec error: OSError: [Errno 28] No space left on device", "disk_full"},
		{"OSError: [Errno 122] Disk quota exceeded", "disk_full"},
		{"OSError: [Errno 30] Read-only file system: 'a.png'", "disk_full"},
		{"OSError: [WinError 112] There is not enough space on the disk", "disk_full"},
		// the token beats a looser class's word in the same message ("room", "timeout")
		{"gpugen: x failed: exit status 1 (ENOSPC: no space left on device, write (writing /work/room/timeout/a.png))", "disk_full"},
	}
	for _, c := range cases {
		if got := ClassifyErr(errString(c.msg)); got != c.want {
			t.Errorf("ClassifyErr(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// Prose is not an errno. A ComfyUI exec error echoes the failing node's inputs, the prompt text among
// them: a picture of a monitor reading "no space left on device" must not be recorded as a full disk.
func TestClassifyErrDiskFullIgnoresProse(t *testing.T) {
	for _, msg := range []string{
		"no space left on device",
		"There is not enough space on the disk.",
		"disk quota exceeded",
		"read-only file system",
		`ComfyUI exec error: {"status_str":"error","current_inputs":{"text":["a monitor reading: no space left on device"]}}`,
		"ENOSPCX is not an errno",
		"OSError: [Errno 2] No such file or directory: 'a.png'",
	} {
		if got := ClassifyErr(errString(msg)); got != "other" {
			t.Errorf("ClassifyErr(%q) = %q, want other (prose alone is not a full disk)", msg, got)
		}
	}
}

// A Go file call that fails on a full volume is classified by its typed errno, whatever its text says
// (Windows words it "There is not enough space on the disk.", Linux "no space left on device").
func TestClassifyErrDiskFullByTypedErrno(t *testing.T) {
	for name, errno := range map[string]syscall.Errno{"ENOSPC": syscall.ENOSPC, "EDQUOT": syscall.EDQUOT, "EROFS": syscall.EROFS} {
		path := &fs.PathError{Op: "write", Path: "a.png", Err: errno}
		for how, err := range map[string]error{
			"PathError":    path,
			"wrapped":      fmt.Errorf("delivering a.png: %w", path),
			"LinkError":    &os.LinkError{Op: "rename", Old: "a.partial", New: "a.png", Err: errno},
			"SyscallError": os.NewSyscallError("write", errno),
		} {
			if got := ClassifyErr(err); got != "disk_full" {
				t.Errorf("%s as %s: ClassifyErr = %q, want disk_full (%v)", name, how, got, err)
			}
			if !IsDiskFull(err) {
				t.Errorf("%s as %s: IsDiskFull = false", name, how)
			}
		}
	}
	// other errnos keep their ordinary class
	if got := ClassifyErr(&fs.PathError{Op: "open", Path: "a.png", Err: syscall.EACCES}); got == "disk_full" {
		t.Errorf("EACCES must not be a full disk, got %q", got)
	}
}

// The runner's own typed class and a timeout or cancel gpugen observed itself are exact; a token in
// the text they carry does not outrank them.
func TestClassifyErrRunErrorClassBeatsTheDiskFullToken(t *testing.T) {
	e := &RunError{Class: "timeout", err: errors.New("gpugen: x timeout after 1s (deadline exceeded): signal: killed (RENDER FAILED: ENOSPC: no space left on device)")}
	if got := ClassifyErr(e); got != "timeout" {
		t.Errorf("a deadline kill is a timeout even when the output tail names ENOSPC, got %q", got)
	}
}

func TestIsDiskFullNil(t *testing.T) {
	if IsDiskFull(nil) {
		t.Error("IsDiskFull(nil) must be false")
	}
}
