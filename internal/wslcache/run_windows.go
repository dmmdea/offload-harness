//go:build windows

package wslcache

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

var system runner = wslExe{}

type wslExe struct{}

// path is wsl.exe under the Windows directory, never a PATH lookup.
func (wslExe) path() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "wsl.exe")
}

// cmd builds one wsl.exe call: a hidden child, UTF-8 output (WSL_UTF8=1), and a working directory WSL can translate
// (System32), because a working directory it cannot translate (a cloud-drive path) prints a warning on every call.
func (w wslExe) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := winexec.CommandContext(ctx, w.path(), args...)
	c.Dir = filepath.Dir(w.path())
	c.Env = append(os.Environ(), "WSL_UTF8=1")
	return c
}

func (w wslExe) running(ctx context.Context) ([]string, error) {
	out, err := w.cmd(ctx, "--list", "--running", "--quiet").Output()
	if err != nil {
		return nil, err // also how "no running distributions" and an account with none answer
	}
	return decodeList(out), nil
}

func (w wslExe) meminfo(ctx context.Context, distro string) ([]byte, error) {
	out, err := w.cmd(ctx, "--distribution", distro, "--cd", "/", "--exec", "cat", "/proc/meminfo").Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", distro, err)
	}
	return out, nil
}

func (w wslExe) drop(ctx context.Context, distro string) error {
	if out, err := w.cmd(ctx, "--distribution", distro, "--user", "root", "--cd", "/", "--exec", "sync").CombinedOutput(); err != nil {
		return fmt.Errorf("sync in %s: %w: %s", distro, err, strings.TrimSpace(string(out)))
	}
	c := w.cmd(ctx, "--distribution", distro, "--user", "root", "--cd", "/", "--exec", "tee", "/proc/sys/vm/drop_caches")
	c.Stdin = strings.NewReader("1\n")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("drop_caches in %s: %w: %s", distro, err, strings.TrimSpace(string(out)))
	}
	return nil
}
