package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/floors"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// Asset is one downloadable file of a candidate with the publisher's own integrity value.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Digest string `json:"digest"` // "sha256:<hex>" (GitHub) or "sha512-<base64>" (npm integrity)
	Size   int64  `json:"size"`
}

// Candidate is the newest complete build of a component for this node, from its publisher.
type Candidate struct {
	Component string   `json:"component"`
	ID        string   `json:"id"`
	Source    string   `json:"source"`
	Variant   string   `json:"variant,omitempty"`
	Assets    []Asset  `json:"assets"`
	Channel   string   `json:"channel,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	// Checksums is the publisher's own checksums file when the release has one (a second source for the digest).
	Checksums *Asset `json:"-"`
}

// ErrUnsupported means the publisher ships no prebuilt for this node (a source build is the way); the component is skipped.
var ErrUnsupported = errors.New("no prebuilt for this node")

// ErrBelowFloor means the newest complete build is under a floor, so none is selected.
var ErrBelowFloor = errors.New("below the floor")

// slot is one required asset: any one of its alternative names satisfies it.
// Patterns carry <TAG> (the whole tag), <N> (its number) and <SHA7> (a master-<N>-<sha7> tag's commit). No version
// number appears in this table.
type slot []string

// assetTable is the required-asset set per component and "<os>/<arch>/<variant>".
var assetTable = map[string]map[string][]slot{
	Llama: {
		"windows/amd64/cuda13": {{"llama-<TAG>-bin-win-cuda-13.4-x64.zip"}, {"cudart-llama-bin-win-cuda-13.4-x64.zip"}},
		"windows/amd64/cuda12": {{"llama-<TAG>-bin-win-cuda-12.4-x64.zip"}, {"cudart-llama-bin-win-cuda-12.4-x64.zip"}},
		"windows/amd64/vulkan": {{"llama-<TAG>-bin-win-vulkan-x64.zip"}},
		"windows/amd64/cpu":    {{"llama-<TAG>-bin-win-cpu-x64.zip"}},
		"windows/arm64/cuda13": {{"llama-<TAG>-bin-win-cuda-13.4-arm64.zip"}, {"cudart-llama-bin-win-cuda-13.4-arm64.zip"}},
		"windows/arm64/vulkan": {{"llama-<TAG>-bin-win-vulkan-arm64.zip"}},
		"windows/arm64/cpu":    {{"llama-<TAG>-bin-win-cpu-arm64.zip"}},
		"linux/amd64/cuda13":   {{"llama-<TAG>-bin-ubuntu-cuda-13.4-x64.tar.gz"}, {"cudart-llama-<TAG>-bin-ubuntu-cuda-13.4-x64.tar.gz"}},
		"linux/amd64/cuda12":   {{"llama-<TAG>-bin-ubuntu-cuda-12.8-x64.tar.gz"}, {"cudart-llama-<TAG>-bin-ubuntu-cuda-12.8-x64.tar.gz"}},
		"linux/amd64/vulkan":   {{"llama-<TAG>-bin-ubuntu-vulkan-x64.tar.gz"}},
		"linux/amd64/cpu":      {{"llama-<TAG>-bin-ubuntu-x64.tar.gz"}},
		"linux/arm64/cuda13":   {{"llama-<TAG>-bin-ubuntu-cuda-13.4-arm64.tar.gz"}, {"cudart-llama-<TAG>-bin-ubuntu-cuda-13.4-arm64.tar.gz"}},
		"linux/arm64/vulkan":   {{"llama-<TAG>-bin-ubuntu-vulkan-arm64.tar.gz"}},
		"linux/arm64/cpu":      {{"llama-<TAG>-bin-ubuntu-arm64.tar.gz"}},
	},
	LlamaSwap: {
		"windows/amd64": {{"llama-swap_<N>_windows_amd64.zip"}},
		"linux/amd64":   {{"llama-swap_<N>_linux_amd64.tar.gz"}},
		"linux/arm64":   {{"llama-swap_<N>_linux_arm64.tar.gz"}},
	},
	Whisper: {
		"windows/amd64/cuda13": {{"whisper-bin-win-cuda-12.4.0-x64.zip", "whisper-cublas-12.4.0-bin-x64.zip"}},
		"windows/amd64/cuda12": {{"whisper-bin-win-cuda-12.4.0-x64.zip", "whisper-cublas-12.4.0-bin-x64.zip"}},
		"windows/amd64/vulkan": {{"whisper-bin-x64.zip"}},
		"windows/amd64/cpu":    {{"whisper-bin-x64.zip"}},
		"linux/amd64/cpu":      {{"whisper-bin-ubuntu-x64.tar.gz"}},
		"linux/arm64/cpu":      {{"whisper-bin-ubuntu-arm64.tar.gz"}},
	},
	SDCpp: {
		"windows/amd64/cuda13": {{"sd-master-<SHA7>-bin-win-cuda12-x64.zip"}, {"cudart-sd-bin-win-cu12-x64.zip"}},
		"windows/amd64/cuda12": {{"sd-master-<SHA7>-bin-win-cuda12-x64.zip"}, {"cudart-sd-bin-win-cu12-x64.zip"}},
		"windows/amd64/vulkan": {{"sd-master-<SHA7>-bin-win-vulkan-x64.zip"}},
		"windows/amd64/cpu":    {{"sd-master-<SHA7>-bin-win-cpu-x64.zip"}},
		"linux/amd64/vulkan":   {{"sd-master-<SHA7>-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip"}},
		"linux/amd64/cpu":      {{"sd-master-<SHA7>-bin-Linux-Ubuntu-24.04-x86_64.zip"}},
	},
}

// requiredSlots picks the table row for this node. A variant with no row falls back to nothing: a CPU build
// never silently replaces a GPU one.
func requiredSlots(comp, variant string) ([]slot, error) {
	rows := assetTable[comp]
	osarch := runtime.GOOS + "/" + runtime.GOARCH
	if s, ok := rows[osarch+"/"+variant]; ok {
		return s, nil
	}
	if s, ok := rows[osarch]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: %s on %s/%s (%s) is a source build", ErrUnsupported, comp, runtime.GOOS, runtime.GOARCH, variant)
}

// Resolver talks to the publishers.
type Resolver struct {
	HTTP    *http.Client
	GitHub  string // API base
	NPM     string
	Token   func() string
	Floors  floors.Set
	Variant string
	Channel string
	Log     func(format string, args ...any)
}

type ghAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
	State  string `json:"state"`
}

type ghRelease struct {
	Tag    string    `json:"tag_name"`
	Draft  bool      `json:"draft"`
	Assets []ghAsset `json:"assets"`
}

// get performs one GET with the token and an optional ETag. notModified is true on a 304.
func (r *Resolver) get(ctx context.Context, rawURL, etag string, github bool) (body []byte, newETag string, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("User-Agent", "local-offload-update")
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if r.Token != nil {
			if t := r.Token(); t != "" {
				req.Header.Set("Authorization", "Bearer "+t)
			}
		}
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("GET %s: HTTP %d %s", rawURL, resp.StatusCode, truncate(strings.TrimSpace(string(b)), 200))
	}
	return b, resp.Header.Get("ETag"), false, nil
}

// Resolve returns the newest complete candidate for def. etag is the stored ETag of the first listing page
// ("" to force a fresh read); notModified is true when the publisher answered 304.
func (r *Resolver) Resolve(ctx context.Context, def *compDef, etag string) (c *Candidate, newETag string, notModified bool, err error) {
	if def.Rule == ruleNPM {
		return r.resolveNPM(ctx, def, etag)
	}
	variant := r.Variant
	if def.Name == LlamaSwap {
		variant = "" // one build per OS and arch
	}
	slots, err := requiredSlots(def.Name, variant)
	if err != nil {
		return nil, "", false, err
	}
	if def.Name == Llama && r.Channel == "release" {
		return r.resolveBlessed(ctx, def, slots, etag)
	}
	re := tagRegex[def.Rule]
	var all []ghRelease
	for page := 1; page <= 5; page++ {
		u := fmt.Sprintf("%s/repos/%s/releases?per_page=10&page=%d", r.GitHub, def.Repo, page)
		sent := ""
		if page == 1 {
			sent = etag
		}
		body, et, nm, gerr := r.get(ctx, u, sent, true)
		if gerr != nil {
			return nil, "", false, gerr
		}
		if nm {
			return nil, etag, true, nil
		}
		if page == 1 {
			newETag = et
		}
		var batch []ghRelease
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, "", false, fmt.Errorf("releases of %s: %w", def.Repo, err)
		}
		for _, rel := range batch {
			if !rel.Draft && re.MatchString(rel.Tag) {
				all = append(all, rel)
			}
		}
		sort.Slice(all, func(i, j int) bool { return floors.Compare(all[i].Tag, all[j].Tag) > 0 })
		var notes []string
		for _, rel := range all {
			cand, miss := buildCandidate(def, variant, rel, slots)
			if cand == nil {
				notes = append(notes, fmt.Sprintf("%s skipped: incomplete (%s)", rel.Tag, miss))
				continue
			}
			cand.Notes = notes
			cand.Channel = "nightly"
			return r.applyFloor(def, cand, newETag)
		}
		if len(batch) < 10 {
			break
		}
	}
	return nil, "", false, fmt.Errorf("%s: no complete release found in the newest listing pages", def.Repo)
}

func (r *Resolver) applyFloor(def *compDef, c *Candidate, etag string) (*Candidate, string, bool, error) {
	if def.Upstream != "" {
		below, compared := r.Floors.Below(def.Upstream, c.ID)
		if below {
			return nil, "", false, fmt.Errorf("%w: the newest complete %s build %s is under the floor %s; nothing selected", ErrBelowFloor, def.Name, c.ID, r.Floors.Floor(def.Upstream))
		}
		if !compared && r.Floors.Floor(def.Upstream) != "" {
			c.Notes = append(c.Notes, fmt.Sprintf("floor %s is not comparable with id %s (different shape); not enforced by id", r.Floors.Floor(def.Upstream), c.ID))
		}
	}
	return c, etag, false, nil
}

var numRe = regexp.MustCompile(`\d+`)

// npmVersionShape is a semver without build metadata: what npm enforces at publish, checked again here because the
// value names a directory.
var npmVersionShape = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

func expand(p, tag string) string {
	n := numRe.FindString(tag)
	sha := ""
	if m := tagRegex[ruleMaster].FindStringSubmatch(tag); m != nil {
		n, sha = m[1], m[2]
		if len(sha) > 7 {
			sha = sha[:7]
		}
	}
	p = strings.ReplaceAll(p, "<TAG>", tag)
	p = strings.ReplaceAll(p, "<N>", n)
	return strings.ReplaceAll(p, "<SHA7>", sha)
}

// buildCandidate returns the candidate when every required slot is satisfied by an uploaded asset with a
// non-empty digest, else nil and what is missing (the caller steps back one tag).
func buildCandidate(def *compDef, variant string, rel ghRelease, slots []slot) (*Candidate, string) {
	c := &Candidate{Component: def.Name, ID: rel.Tag, Source: def.Repo, Variant: variant}
	for _, s := range slots {
		found := false
		var missing []string
		for _, alt := range s {
			name := expand(alt, rel.Tag)
			ok := false
			for _, a := range rel.Assets {
				if a.Name != name {
					continue
				}
				if (a.State == "" || a.State == "uploaded") && strings.HasPrefix(a.Digest, "sha256:") && len(a.Digest) > len("sha256:") {
					c.Assets = append(c.Assets, Asset{Name: a.Name, URL: a.URL, Digest: a.Digest, Size: a.Size})
					ok = true
				}
				break
			}
			if ok {
				found = true
				break
			}
			missing = append(missing, name)
		}
		if !found {
			return nil, strings.Join(missing, " | ")
		}
	}
	for _, a := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), "checksums.txt") && (a.State == "" || a.State == "uploaded") {
			c.Checksums = &Asset{Name: a.Name, URL: a.URL}
		}
	}
	return c, ""
}

// resolveBlessed is the llama.cpp "release" channel: /releases/latest is a stub whose nightly-tag.txt names the blessed b-tag.
func (r *Resolver) resolveBlessed(ctx context.Context, def *compDef, slots []slot, etag string) (*Candidate, string, bool, error) {
	body, et, nm, err := r.get(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", r.GitHub, def.Repo), etag, true)
	if err != nil {
		return nil, "", false, err
	}
	if nm {
		return nil, etag, true, nil
	}
	var stub ghRelease
	if err := json.Unmarshal(body, &stub); err != nil {
		return nil, "", false, err
	}
	var txt string
	for _, a := range stub.Assets {
		if a.Name == "nightly-tag.txt" {
			b, _, _, gerr := r.get(ctx, a.URL, "", false)
			if gerr != nil {
				return nil, "", false, gerr
			}
			txt = strings.TrimSpace(string(b))
		}
	}
	if !tagRegex[ruleB].MatchString(txt) {
		return nil, "", false, fmt.Errorf("release channel: nightly-tag.txt does not name a b-tag (%q)", truncate(txt, 40))
	}
	body, _, _, err = r.get(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", r.GitHub, def.Repo, url.PathEscape(txt)), "", true)
	if err != nil {
		return nil, "", false, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, "", false, err
	}
	cand, miss := buildCandidate(def, r.Variant, rel, slots)
	if cand == nil {
		return nil, "", false, fmt.Errorf("release channel: blessed tag %s is incomplete (%s)", txt, miss)
	}
	cand.Channel = "release"
	return r.applyFloor(def, cand, et)
}

func (r *Resolver) resolveNPM(ctx context.Context, def *compDef, etag string) (*Candidate, string, bool, error) {
	body, et, nm, err := r.get(ctx, fmt.Sprintf("%s/%s/latest", r.NPM, def.Repo), etag, false)
	if err != nil {
		return nil, "", false, err
	}
	if nm {
		return nil, etag, true, nil
	}
	var v struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, "", false, err
	}
	if v.Version == "" || v.Dist.Integrity == "" || v.Dist.Tarball == "" {
		return nil, "", false, fmt.Errorf("npm %s: the registry answer has no version or integrity", def.Repo)
	}
	// The version names a generation directory and an npm spec: only a semver shape is ever used there.
	if !npmVersionShape.MatchString(v.Version) {
		return nil, "", false, fmt.Errorf("npm %s: the registry answered an unusable version %q (not a semver)", def.Repo, v.Version)
	}
	c := &Candidate{Component: def.Name, ID: v.Version, Source: "npm:" + def.Repo, Channel: "latest",
		Assets: []Asset{{Name: def.Repo + "-" + v.Version + ".tgz", URL: v.Dist.Tarball, Digest: v.Dist.Integrity}}}
	return r.applyFloor(def, c, et)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GitHubToken finds a read token without storing it: GH_TOKEN, GITHUB_TOKEN, else `gh auth token` (for the account
// named by OFFLOAD_UPDATE_GH_USER when set). Empty means unauthenticated.
func GitHubToken() func() string {
	var cached string
	var done bool
	return func() string {
		if done {
			return cached
		}
		done = true
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				cached = v
				return v
			}
		}
		gh, err := exec.LookPath("gh")
		if err != nil {
			return ""
		}
		args := []string{"auth", "token"}
		if u := strings.TrimSpace(os.Getenv("OFFLOAD_UPDATE_GH_USER")); u != "" {
			args = append(args, "--user", u)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := winexec.CommandContext(ctx, gh, args...).Output()
		if err == nil {
			cached = strings.TrimSpace(string(out))
		}
		return cached
	}
}

// DetectVariant picks the build family for this node: cuda13, cuda12, vulkan or cpu. backend is the serving
// backend the installer recorded ("" when unknown).
func DetectVariant(backend string) string {
	smi, _ := exec.LookPath("nvidia-smi")
	b := strings.ToLower(backend)
	switch {
	case strings.Contains(b, "vulkan"):
		return "vulkan"
	case b == "cpu":
		return "cpu"
	case strings.Contains(b, "cuda"), b == "" && smi != "":
		return cudaFamily(smi)
	case b == "":
		if runtime.GOOS == "windows" {
			return "vulkan"
		}
		return "cpu"
	}
	return "cpu"
}

var cudaVerRe = regexp.MustCompile(`CUDA (?:UMD )?Version:\s*(\d+)\.(\d+)`)

// cudaFamily reads the driver's CUDA major the way the installer does: a 13.x driver takes the cuda-13.4 assets,
// anything else cuda-12; a card older than compute 7.5 (Maxwell, Pascal, Volta: CUDA 13 dropped them) is always 12.
func cudaFamily(smi string) string {
	if smi == "" {
		return "cuda12"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := winexec.CommandContext(ctx, smi).Output()
	if err != nil {
		return "cuda12"
	}
	m := cudaVerRe.FindSubmatch(out)
	if m == nil {
		return "cuda12"
	}
	if major, _ := strconv.Atoi(string(m[1])); major < 13 {
		return "cuda12"
	}
	if cc, err := winexec.CommandContext(ctx, smi, "--query-gpu=compute_cap", "--format=csv,noheader").Output(); err == nil {
		for _, line := range strings.Split(string(cc), "\n") {
			if f, perr := strconv.ParseFloat(strings.TrimSpace(line), 64); perr == nil && f < 7.5 {
				return "cuda12"
			}
		}
	}
	return "cuda13"
}
