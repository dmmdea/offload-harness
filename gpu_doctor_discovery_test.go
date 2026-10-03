package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// writeFakeLeaseReader writes a 2 MiB executable-shaped file under any name: it carries the
// import path of the package that reads the lease (what every harness binary that can read
// the lease directory carries) and the format signature only when aware.
func writeFakeLeaseReader(t *testing.T, dir, name string, aware bool) string {
	t.Helper()
	body := bytes.Repeat([]byte("\x00MZ-padding-"), (2<<20)/12)
	body = append(body, []byte("\x00github.com/dmmdea/offload-harness/internal/gpulease.(*Manager).Inspect\x00")...)
	if aware {
		body = append(body, []byte("\x00"+gpulease.FormatSignature+"\x00")...)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A wrapper copy that a media repository carries under a name of its own is found by what
// it is, not what it is called, when the doctor is pointed at the repository.
func TestGPUDoctorFindsARenamedCopyUnderAScanRoot(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	repo := t.TempDir()
	old := writeFakeLeaseReader(t, filepath.Join(repo), "media-wrapper.exe", false)
	if err := runGPUDoctor([]string{"--config", cfg, "--scan", repo}); err == nil {
		t.Fatalf("a pre-format copy under another name must fail the audit:\n%s", out)
	}
	if !strings.Contains(out.String(), old) || !strings.Contains(out.String(), "OLD") {
		t.Fatalf("the renamed copy must be listed as OLD:\n%s", out)
	}
	// Once it is replaced by an aware build the same scan is green.
	out.Reset()
	writeFakeLeaseReader(t, repo, "media-wrapper.exe", true)
	if err := runGPUDoctor([]string{"--config", cfg, "--scan", repo}); err != nil {
		t.Fatalf("an aware renamed copy is green: %v\n%s", err, out)
	}
}

// What the walk did not enter is printed and is in the JSON, and --depth widens it.
func TestGPUDoctorNamesWhatItDidNotSearchAndDepthWidensIt(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	repo := t.TempDir()
	deep := filepath.Join(repo, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := writeFakeLeaseReader(t, deep, "media-wrapper.exe", false)

	if err := runGPUDoctor([]string{"--config", cfg, "--scan", repo}); err != nil {
		t.Fatalf("the copy is below the default cap, so the audit does not reach it: %v\n%s", err, out)
	}
	for _, want := range []string{"not searched", deep, filepath.Join(repo, "node_modules"), "--depth"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output must say what it did not search (%q):\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "GREEN: every reader this audit reached understands the per-epoch fence\n") {
		t.Errorf("a green that left directories unsearched must say so on the verdict line:\n%s", out)
	}

	out.Reset()
	if err := runGPUDoctor([]string{"--config", cfg, "--scan", repo, "--json"}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		NotSearched []string `json:"not_searched"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || !strings.Contains(strings.Join(doc.NotSearched, "\n"), deep) {
		t.Fatalf("--json must carry not_searched: %v\n%s", err, out)
	}

	out.Reset()
	if err := runGPUDoctor([]string{"--config", cfg, "--scan", repo, "--depth", "6"}); err == nil || !strings.Contains(out.String(), old) {
		t.Fatalf("--depth 6 reaches the copy, which fails the audit: %v\n%s", err, out)
	}
}

// The agent binary reads the lease before it loads a seat, and is deployed under its own
// name: a running one is audited by its image and one on PATH is found by name.
func TestGPUDoctorCoversTheAgentBinary(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	runningImagesFn = func(want func(int, string) string) ([]gpulease.ProcImage, error) {
		for _, name := range []string{"local-agent.exe", "local-agent", "local-agent.serve"} {
			if want(77, name) == "" {
				t.Errorf("a running %s process must be wanted, whatever extension the OS reports", name)
			}
		}
		return nil, nil
	}
	pathDir := t.TempDir()
	stale := writeFakeBinary(t, pathDir, "local-agent.exe", false)
	doctorPathEnv = func() string { return pathDir }
	if err := runGPUDoctor([]string{"--config", cfg}); err == nil || !strings.Contains(out.String(), stale) {
		t.Fatalf("a stale local-agent on PATH must fail the audit: %v\n%s", err, out)
	}
}

// THE LINKER MUST KEEP THE SIGNATURE IN EVERY BINARY THAT CAN READ THE LEASE, not only the
// one the doctor happens to be part of. The agent binary links the lease package through
// modelaffinity and is deployed beside the harness; when only a reachable Audit kept the
// literal, a build of it carried no signature and a doctor run would have called a
// pre-format agent and a current one the same.
func TestEveryBinaryThatLinksTheLeaseReaderCarriesTheSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the product binaries")
	}
	pkgs := packagesLinkingTheLeaseReader(t)
	if len(pkgs) < 2 {
		t.Fatalf("expected at least the harness and the agent to link the lease reader, found %v", pkgs)
	}
	sawAgent := false
	for _, pkg := range pkgs {
		name := filepath.Base(pkg)
		if name == "local-agent" {
			sawAgent = true
		}
		exe := filepath.Join(t.TempDir(), name)
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", exe, pkg).CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
		r := gpulease.Audit(gpulease.AuditOptions{Files: []string{exe}})
		if len(r.Items) != 1 || !r.Items[0].Aware {
			t.Errorf("%s links the lease reader and must carry the format signature: %+v", pkg, r.Items)
		}
	}
	if !sawAgent {
		t.Fatalf("the agent binary must be one of the binaries checked: %v", pkgs)
	}
}

// The agent built from this tree, copied to a name nobody listed, is found by content and
// judged aware: the same file a real renamed wrapper would be.
func TestARenamedCopyOfABuiltBinaryIsFoundAndJudged(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent binary")
	}
	built := filepath.Join(t.TempDir(), "local-agent")
	if runtime.GOOS == "windows" {
		built += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", built, "./cmd/local-agent").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	b, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	scan := t.TempDir()
	copyName := filepath.Join(scan, "media-wrapper.exe")
	if err := os.WriteFile(copyName, b, 0o755); err != nil {
		t.Fatal(err)
	}
	r := gpulease.Audit(gpulease.AuditOptions{Roots: []string{scan}})
	if len(r.Items) != 1 || r.Items[0].Path != copyName || !r.Items[0].Aware || !r.Green {
		t.Fatalf("a renamed copy of the real binary must be found by content and judged aware: %+v", r)
	}
}

// packagesLinkingTheLeaseReader lists the ./cmd/... main packages whose dependencies
// include the lease package.
func packagesLinkingTheLeaseReader(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}\t{{.Name}}\t{{join .Deps \" \"}}", "./cmd/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(ln), "\t", 3)
		if len(parts) != 3 || parts[1] != "main" {
			continue
		}
		for _, dep := range strings.Fields(parts[2]) {
			if strings.HasSuffix(dep, "/internal/gpulease") {
				pkgs = append(pkgs, parts[0])
				break
			}
		}
	}
	return pkgs
}
