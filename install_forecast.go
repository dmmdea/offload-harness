package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/forecastserve"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The sources of the forecast lane (docs: plans/offload-harness/2026-10-24-forecast-lane.md). Nothing here is a
// version: the python package is installed from the head of its git default branch and the weights from the head of
// the Hugging Face repository's main branch, both resolved at install time. The checkpoint is verified after the
// fetch against the publisher's own digest (the HF tree API's lfs.oid and size, read at install time), so a changed
// or truncated file is an error, not a quiet difference. forecastPythonFloor is a floor (uv picks the newest
// interpreter at or above it).
const (
	forecastTimestralRepo = "https://github.com/mistralai/timestral.git"
	forecastHFRepo        = "mistralai/Timestral-209M"
	forecastModelDirName  = "timestral-209m"
	forecastWeightsFile   = "model.safetensors"
	forecastPythonFloor   = ">=3.13"
	forecastHFAPI         = "https://huggingface.co/api/models/"
)

var forecastHFPatterns = []string{"config.json", forecastWeightsFile, "LICENSE", "README.md"}

var forecastFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// forecastHTTPGet reads a URL's body; a seam so the resolver has one place that touches the network.
var forecastHTTPGet = func(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// forecastWeights is the checkpoint as the publisher describes it right now.
type forecastWeights struct {
	Revision string // the commit `main` points at
	SHA256   string // lfs.oid of the weights file
	Size     int64  // lfs size of the weights file
}

// resolveForecastWeights reads the revision `main` points at and the weights' lfs.oid and size from the Hugging
// Face API. The tree is read at the resolved revision, so the digest and the files fetched are the same commit even
// when main moves between the two calls.
func resolveForecastWeights(ctx context.Context) (forecastWeights, error) {
	var w forecastWeights
	body, err := forecastHTTPGet(ctx, forecastHFAPI+forecastHFRepo+"/revision/main")
	if err != nil {
		return w, fmt.Errorf("resolving %s main: %w", forecastHFRepo, err)
	}
	var rev struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &rev); err != nil || !forecastFullSHA.MatchString(rev.SHA) {
		return w, fmt.Errorf("resolving %s main: no commit in the answer", forecastHFRepo)
	}
	w.Revision = rev.SHA
	body, err = forecastHTTPGet(ctx, forecastHFAPI+forecastHFRepo+"/tree/"+url.PathEscape(rev.SHA)+"?recursive=true")
	if err != nil {
		return w, fmt.Errorf("listing %s@%s: %w", forecastHFRepo, rev.SHA[:8], err)
	}
	var tree []struct {
		Path string `json:"path"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return w, fmt.Errorf("listing %s@%s: %w", forecastHFRepo, rev.SHA[:8], err)
	}
	for _, f := range tree {
		if f.Path == forecastWeightsFile && f.LFS != nil && f.LFS.OID != "" && f.LFS.Size > 0 {
			w.SHA256, w.Size = f.LFS.OID, f.LFS.Size
			return w, nil
		}
	}
	return w, fmt.Errorf("%s@%s lists no %s with an lfs digest", forecastHFRepo, rev.SHA[:8], forecastWeightsFile)
}

// resolveTimestralHead is the commit the head of the package's default branch is at (git ls-remote HEAD).
func resolveTimestralHead(ctx context.Context) (string, error) {
	var out strings.Builder
	cmd := winexec.CommandContext(ctx, "git", "ls-remote", forecastTimestralRepo, "HEAD")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote %s HEAD: %w", forecastTimestralRepo, err)
	}
	f := strings.Fields(out.String())
	if len(f) == 0 || !forecastFullSHA.MatchString(f[0]) {
		return "", fmt.Errorf("git ls-remote %s HEAD: no commit in %q", forecastTimestralRepo, strings.TrimSpace(out.String()))
	}
	return f[0], nil
}

// forecastInstallResult is the final line of `install forecast --json`.
type forecastInstallResult struct {
	OK            bool   `json:"ok"`
	ForecastDir   string `json:"forecast_dir"`
	VenvPython    string `json:"venv_python"`
	Torch         string `json:"torch"`
	CudaAvailable bool   `json:"cuda_available"`
	Timestral     string `json:"timestral"`
	ModelDir      string `json:"model_dir"`
	SHA256        string `json:"sha256"`
	Error         string `json:"error,omitempty"`
}

// runInstallForecast builds the forecast lane's runtime on a node: a python venv with torch and the
// newest timestral package, the newest weights (verified against the publisher's digest) and the embedded server file. Every
// step verifies what is already there and skips when it is right, so a second run changes nothing.
// It never loads the model and starts no server: the card is the seat's.
func runInstallForecast(args []string) error {
	fs := flag.NewFlagSet("install forecast", flag.ExitOnError)
	home := fs.String("home", "", "install root; the lane lives in <home>/forecast (default: the resolved harness home)")
	models := fs.String("models", "", "directory holding model files; the weights go to <models>/"+forecastModelDirName+" (default: <home>/models)")
	ref := fs.String("ref", "", "git sha or branch of the timestral package to install (default: the head of its default branch, resolved now)")
	backend := fs.String("torch-backend", "auto", "uv --torch-backend for torch (auto picks the build for this machine's driver)")
	asJSON := fs.Bool("json", false, "print the result as one JSON line on stdout (progress goes to stderr)")
	timeout := fs.Duration("timeout", 60*time.Minute, "give up after this long (the torch wheels and the 837 MB checkpoint included)")
	_ = fs.Parse(args)

	h := *home
	if h == "" {
		h = config.DefaultBase()
	}
	m := *models
	if m == "" {
		m = filepath.Join(h, "models")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := installForecast(ctx, filepath.Join(h, "forecast"), m, *ref, *backend, os.Stderr)
	if *asJSON {
		res.OK = err == nil
		if err != nil {
			res.Error = err.Error()
		}
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
		return err
	}
	if err != nil {
		return err
	}
	fmt.Printf("OK    forecast lane ready in %s (torch %s, cuda_available=%t, timestral %s, weights %s verified)\n",
		res.ForecastDir, res.Torch, res.CudaAvailable, res.Timestral, res.ModelDir)
	return nil
}

func installForecast(ctx context.Context, dir, models, ref, backend string, log io.Writer) (forecastInstallResult, error) {
	res := forecastInstallResult{ForecastDir: dir, VenvPython: forecastserve.VenvPython(dir), ModelDir: filepath.Join(models, forecastModelDirName)}
	say := func(format string, a ...any) { fmt.Fprintf(log, "forecast: "+format+"\n", a...) }

	uv, err := exec.LookPath("uv")
	if err != nil {
		return res, fmt.Errorf("install forecast: uv is not on PATH; install it first (https://docs.astral.sh/uv/getting-started/installation/ ; on Windows `winget install astral-sh.uv`, elsewhere `curl -LsSf https://astral.sh/uv/install.sh | sh`)")
	}
	py := res.VenvPython
	venv := filepath.Join(dir, "venv")

	// 1. The venv.
	if out, perr := runPython(ctx, py, "import sys; print('%d.%d' % sys.version_info[:2])"); perr == nil {
		say("venv ok (%s, python %s)", venv, strings.TrimSpace(out))
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, fmt.Errorf("install forecast: %w", err)
		}
		a := []string{"venv", "--python", forecastPythonFloor}
		if _, serr := os.Stat(venv); serr == nil {
			a = append(a, "--clear") // a directory without a working python is a broken venv of ours: rebuild it
		}
		say("creating the venv (uv venv --python %s)", forecastPythonFloor)
		if err := runLogged(ctx, log, uv, append(a, venv)...); err != nil {
			return res, fmt.Errorf("install forecast: uv venv: %w", err)
		}
	}

	// 2. torch.
	tv, cuda, err := probeTorch(ctx, py)
	if err != nil {
		say("installing torch (uv pip install torch --torch-backend=%s)", backend)
		if err := runLogged(ctx, log, uv, "pip", "install", "--python", py, "torch", "--torch-backend="+backend); err != nil {
			return res, fmt.Errorf("install forecast: torch: %w", err)
		}
		if tv, cuda, err = probeTorch(ctx, py); err != nil {
			return res, fmt.Errorf("install forecast: torch did not import after its install: %w", err)
		}
	} else {
		say("torch ok (%s)", tv)
	}
	res.Torch, res.CudaAvailable = tv, cuda

	// 3. timestral, at the head of its default branch (or the ref the operator named).
	if _, gerr := exec.LookPath("git"); gerr != nil {
		return res, fmt.Errorf("install forecast: git is not on PATH; uv installs timestral from %s", forecastTimestralRepo)
	}
	if ref == "" {
		head, rerr := resolveTimestralHead(ctx)
		if rerr != nil {
			return res, fmt.Errorf("install forecast: %w", rerr)
		}
		ref = head
	}
	tsv, tsCommit, err := probeTimestral(ctx, py)
	if err == nil && (forecastFullSHA.MatchString(ref) && tsCommit == ref) {
		say("timestral ok (%s at %s)", tsv, tsCommit)
	} else {
		say("installing timestral at %s", ref)
		if err := runLogged(ctx, log, uv, "pip", "install", "--python", py, "timestral @ git+"+forecastTimestralRepo+"@"+ref); err != nil {
			return res, fmt.Errorf("install forecast: timestral: %w", err)
		}
		if tsv, _, err = probeTimestral(ctx, py); err != nil {
			return res, fmt.Errorf("install forecast: timestral did not import after its install: %w", err)
		}
	}
	res.Timestral = tsv

	// 4. The weights, verified against the digest the publisher lists now.
	want, werr := resolveForecastWeights(ctx)
	if werr != nil {
		return res, fmt.Errorf("install forecast: %w", werr)
	}
	weights := filepath.Join(res.ModelDir, forecastWeightsFile)
	sum, herr := hashIfSized(weights, want.Size)
	if herr == nil && sum == want.SHA256 {
		say("weights ok (%s)", weights)
	} else {
		say("fetching %s@%s into %s", forecastHFRepo, want.Revision[:8], res.ModelDir)
		if err := os.MkdirAll(res.ModelDir, 0o755); err != nil {
			return res, fmt.Errorf("install forecast: %w", err)
		}
		code := "import sys, json\nfrom huggingface_hub import snapshot_download\n" +
			"snapshot_download(repo_id=" + pyStr(forecastHFRepo) + ", revision=" + pyStr(want.Revision) +
			", local_dir=sys.argv[1], allow_patterns=" + pyList(forecastHFPatterns) + ")\n"
		cmd := winexec.CommandContext(ctx, py, "-I", "-c", code, res.ModelDir)
		cmd.Env = append(os.Environ(), "HF_HUB_DISABLE_SYMLINKS_WARNING=1")
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Run(); err != nil {
			return res, fmt.Errorf("install forecast: snapshot_download: %w", err)
		}
		if sum, herr = hashIfSized(weights, want.Size); herr != nil {
			return res, fmt.Errorf("install forecast: %s after the download: %w", weights, herr)
		}
	}
	res.SHA256 = sum
	if sum != want.SHA256 {
		return res, fmt.Errorf("install forecast: %s hashes to %s, the publisher lists %s (revision %s): the checkpoint is not the one %s serves; delete it and run the install again", weights, sum, want.SHA256, want.Revision, forecastHFRepo)
	}
	say("weights licence: Modified MIT (keep the \"2026 - Mistral AI\" notice; no rights for a company whose monthly revenue exceeds USD 20M)")

	// 5. The embedded server file.
	script, wrote, err := forecastserve.EnsureScript(dir)
	if err != nil {
		return res, fmt.Errorf("install forecast: %w", err)
	}
	if wrote {
		say("wrote %s", script)
	} else {
		say("server file ok (%s)", script)
	}
	return res, nil
}

// runPython runs `python -I -c code` and returns its stdout.
func runPython(ctx context.Context, py, code string) (string, error) {
	if fi, err := os.Stat(py); err != nil || fi.IsDir() {
		return "", fmt.Errorf("no python at %s", py)
	}
	var out strings.Builder
	cmd := winexec.CommandContext(ctx, py, "-I", "-c", code)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// lastJSONLine decodes the last non-empty stdout line, so a library warning printed before it does not matter.
func lastJSONLine(out string, v any) error {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), v)
}

// probeTorch reports the installed torch and whether it sees a card. cuda.is_available() asks the
// driver how many devices there are; it creates no context and allocates nothing.
func probeTorch(ctx context.Context, py string) (version string, cuda bool, err error) {
	out, err := runPython(ctx, py, "import json, torch\nprint(json.dumps({'v': torch.__version__, 'cuda': bool(torch.cuda.is_available())}))")
	if err != nil {
		return "", false, err
	}
	var r struct {
		V    string `json:"v"`
		Cuda bool   `json:"cuda"`
	}
	if err := lastJSONLine(out, &r); err != nil {
		return "", false, fmt.Errorf("unreadable torch probe %q: %w", strings.TrimSpace(out), err)
	}
	return r.V, r.Cuda, nil
}

// probeTimestral reports the installed timestral version and the git commit it was installed from
// (pip's direct_url.json), and proves it imports.
func probeTimestral(ctx context.Context, py string) (version, commit string, err error) {
	const code = "import json, importlib.metadata as m\nimport timestral\n" +
		"d = m.distribution('timestral')\ndu = d.read_text('direct_url.json')\n" +
		"c = json.loads(du).get('vcs_info', {}).get('commit_id', '') if du else ''\n" +
		"print(json.dumps({'v': d.version, 'c': c}))"
	out, err := runPython(ctx, py, code)
	if err != nil {
		return "", "", err
	}
	var r struct{ V, C string }
	if err := lastJSONLine(out, &r); err != nil {
		return "", "", fmt.Errorf("unreadable timestral probe %q: %w", strings.TrimSpace(out), err)
	}
	return r.V, r.C, nil
}

// runLogged runs a command with its output on the log writer.
func runLogged(ctx context.Context, log io.Writer, name string, args ...string) error {
	cmd := winexec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = log, log
	return cmd.Run()
}

// hashIfSized is the sha256 of path, read only when the file has the size the publisher lists: a missing or
// short file is an error without a hash (so a partial download is refetched, not hashed).
func hashIfSized(path string, size int64) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() != size {
		return "", fmt.Errorf("%s is %d bytes, the published checkpoint is %d", path, fi.Size(), size)
	}
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

func pyStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func pyList(ss []string) string { b, _ := json.Marshal(ss); return string(b) }
