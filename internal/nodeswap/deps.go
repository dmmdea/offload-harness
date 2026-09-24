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
	"strings"
	"time"
)

// DefaultDeps wires the real, cross-platform half of Deps. The two
// platform-specific fields — FindProcessesByExe and StopProcess, which need
// CIM/Win32_Process on Windows and have no equivalent this package implements
// on other OSes — are filled in by platformDeps (deps_windows.go /
// deps_other.go), matching the repo's existing _windows.go /
// crossplatform_lint_test.go convention.
func DefaultDeps() Deps {
	d := Deps{
		Hash:         hashFile,
		ReadHealth:   readHealth,
		RenameFile:   renameFile,
		RemoveAll:    os.RemoveAll,
		Exists:       fileExists,
		MkdirAll:     func(path string) error { return os.MkdirAll(path, 0o755) },
		RunCommand:   runPowerShell,
		ExtractTarGz: extractTarGz,
		Sleep:        time.Sleep,
		Now:          time.Now,
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
