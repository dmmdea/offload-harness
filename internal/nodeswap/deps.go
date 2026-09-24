package nodeswap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// DefaultDeps wires the real, cross-platform half of Deps. The two
// platform-specific fields — FindProcessesByExe and StopProcess, which need
// CIM/Win32_Process on Windows and have no equivalent this package implements
// on other OSes — are filled in by platformDeps (deps_windows.go /
// deps_other.go), matching the repo's existing _windows.go /
// crossplatform_lint_test.go convention.
func DefaultDeps() Deps {
	d := Deps{
		Hash:                   hashFile,
		ReadHealth:             readHealth,
		InspectGPULease:        inspectGPULease,
		RenameFile:             renameFile,
		CopyFile:               copyFile,
		IsCrossDeviceRenameErr: isCrossDeviceRenameErr,
		RemoveAll:              os.RemoveAll,
		Exists:                 fileExists,
		MkdirAll:               func(path string) error { return os.MkdirAll(path, 0o755) },
		RunCommand:             runPowerShell,
		ExtractTarGz:           extractTarGz,
		Sleep:                  time.Sleep,
		Now:                    time.Now,
	}
	platformDeps(&d)
	return d
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// renameFile is os.Rename, isolated behind Deps so tests can fake a live
// Windows file-lock failure without touching disk. os.Rename fails on
// Windows when the destination already exists OR the source is open with a
// sharing mode that forbids rename (the exact "Access is denied" trap every
// prior deploy hit); the caller (renameWithRetry) is what diagnoses and
// retries, this stays a thin wrapper.
func renameFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

// copyFile is CopyFile's real implementation, used only on installNewBinary's
// cross-device rename fallback (nodeswap.go): a plain byte-for-byte copy that
// preserves the source's file mode (the staged binary must keep +x on Linux)
// and Syncs before Close so every byte is actually on disk before the caller
// re-hashes it. The destination is created fresh (O_EXCL): installNewBinary
// always names it with a run-unique suffix, so an existing file at that path
// is unexpected and safer to refuse than to silently overwrite.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// isCrossDeviceRenameErr is IsCrossDeviceRenameErr's real implementation. It
// reports whether err is the specific, recoverable "source and destination
// are not on the same volume/filesystem" rename failure — never any other
// error — so installNewBinary can safely fall back to a copy for exactly
// this shape and nothing else.
//
// On Linux/macOS this is syscall.EXDEV ("invalid cross-device link"),
// exactly what a real os.Rename across a mount boundary returns.
//
// On Windows this is NOT syscall.EXDEV, despite the name: the E* constants
// the Go windows syscall package defines (EXDEV included) are "invented
// values" in the reserved APPLICATION_ERROR range, for package os's
// internal use, and are never actually returned by any real Win32 API
// (measured on a live cross-drive os.Rename, C:\ staged against a D:\
// target — the exact 2026-09-24 Aorus rollout failure: errors.Is(err,
// syscall.EXDEV) is false; the error unwraps to syscall.Errno(17),
// ERROR_NOT_SAME_DEVICE, "The system cannot move the file to a different
// disk drive"). errno 17 is checked only when actually running on Windows —
// on every other OS it is just some other errno with no special meaning.
func isCrossDeviceRenameErr(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	if runtime.GOOS == "windows" {
		const errorNotSameDevice = 17
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return errno == errorNotSameDevice
		}
	}
	return false
}

// fleetHealthResponse is the subset of GET /fleet/health this package reads
// (docs/FLEET-NODE.md CONTRACT.md v2). Unknown fields are ignored so a newer
// harness release adding fields never breaks this decode.
type fleetHealthResponse struct {
	NodeID         string `json:"node_id"`
	HarnessVersion string `json:"harness_version"`
	JobsRunning    int    `json:"jobs_running"`
	JobsQueued     int    `json:"jobs_queued"`
	QueueDepth     int    `json:"queue_depth"`
}

func readHealth(ctx context.Context, url string) (HealthInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthInfo{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return HealthInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return HealthInfo{}, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r fleetHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return HealthInfo{}, fmt.Errorf("decoding %s: %w", url, err)
	}
	running, queued := r.JobsRunning, r.JobsQueued
	// Older nodes (pre-0.100.0, per docs/FLEET-NODE.md) publish only
	// queue_depth (running+queued combined). Fall back to it so this tool
	// still works against a not-yet-upgraded fleet member.
	if running == 0 && queued == 0 && r.QueueDepth > 0 {
		running = r.QueueDepth
	}
	return HealthInfo{OK: true, NodeID: r.NodeID, Version: r.HarnessVersion, RunningJobs: running, QueuedJobs: queued}, nil
}

// inspectGPULease is InspectGPULease's real implementation — the same
// resolution `local-offload gpu status` uses (internal/gpulease.OpenAt with
// the node's own gpu_lock_path/state_dir, "" meaning the harness's built-in
// defaults). Opening is cheap (no held file handle; Inspect() reads the lease
// meta file fresh each call), so this is safe to call once per poll.
func inspectGPULease(lockPath, stateDir string) (GPULeaseInfo, error) {
	m, err := gpulease.OpenAt(lockPath, stateDir)
	if err != nil {
		return GPULeaseInfo{}, err
	}
	info := m.Inspect()
	return GPULeaseInfo{Held: info.Held, Reason: info.Reason}, nil
}

// runPowerShell shells out with no visible window — every spawned console on
// this operator's machines must stay hidden (house rule; see
// deps_windows.go's hideWindow, applied here too since this file is the one
// that actually invokes powershell.exe).
func runPowerShell(ctx context.Context, timeout time.Duration, command string) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// extractTarGz extracts a .tar.gz produced the same way every deploy record
// built one (`tar -czf render-<sha>.tar.gz render`) into destDir, returning
// the number of regular files written. Pure stdlib (archive/tar +
// compress/gzip): no new dependency, works identically on every OS the
// harness ships to.
func extractTarGz(tarGzPath, destDir string) (int, error) {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, fmt.Errorf("reading tar entry: %w", err)
		}
		// hdr.Name is typically "render/foo.mjs" (the tarball wraps a top-
		// level "render" dir, per every deploy record); strip the first
		// path element so the contents land directly under destDir.
		name := hdr.Name
		if parts := strings.SplitN(filepath.ToSlash(name), "/", 2); len(parts) == 2 {
			name = parts[1]
		} else {
			continue // the bare top-level dir entry itself
		}
		if name == "" {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(name))
		// Reject a path that escapes destDir (a malformed or hostile
		// tarball) before creating anything.
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return n, fmt.Errorf("tar entry %q escapes destination directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return n, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return n, err
			}
			if err := out.Close(); err != nil {
				return n, err
			}
			n++
		default:
			// symlinks etc.: the harness's render tree is plain files and
			// dirs (confirmed by every deploy record's file-count table);
			// skip anything else rather than fail the whole extraction.
		}
	}
	if n == 0 {
		return 0, errors.New("tarball contained no regular files")
	}
	return n, nil
}
