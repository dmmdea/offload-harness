package agent

// githubtool.go gives the agent real GitHub capability via the REST API, using a
// token supplied out-of-band (GITHUB_TOKEN). Three tools:
//   - github_api           : any authenticated REST call (full, uncapped access)
//   - github_create_repo   : convenience — create a repo (auto-initialised)
//   - github_upload_file   : convenience — create/update a worktree file in a repo
// All go through the hardened fetch client + the broker's egress allowlist
// (ActFetch on api.github.com), so they inherit the same SSRF/host gating as
// web_fetch. The token is a secret carried only in the Authorization header.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const githubTimeout = 25 * time.Second

// githubDo performs one authenticated GitHub REST call. path is like
// "/user/repos". Returns (status, body, err); a non-nil err is a transport/gate
// failure (the caller surfaces it as data, defer-not-crash).
func githubDo(ctx context.Context, client *http.Client, pol *Policy, token, method, path string, body []byte) (int, []byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse("https://api.github.com" + path)
	if err != nil {
		return 0, nil, err
	}
	if err := pol.validateURL(u); err != nil { // host must be on the egress allowlist
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), u.String(), rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "local-offload-agent")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxReadBytes)+1))
	return resp.StatusCode, data, nil
}

// GitHubTools builds the GitHub toolset. token is required (empty => the tools
// refuse, defer-not-crash). defaultRepo (owner/name) is used when a call omits
// the repo. worktreeRoot is where github_upload_file reads local files from.
func GitHubTools(pol *Policy, token, defaultRepo, worktreeRoot string) []Tool {
	client := newFetchClient(pol)
	s := &scope{root: worktreeRoot}
	need := func() (string, bool) {
		if strings.TrimSpace(token) == "" {
			return "NOT performed: no GitHub token configured (set GITHUB_TOKEN when starting the server)", false
		}
		return "", true
	}

	apiTool := Tool{
		ToolSpec: ToolSpec{
			Name:        "github_api",
			Description: "Make an authenticated GitHub REST API call. method: GET/POST/PUT/PATCH/DELETE. path: e.g. /user/repos or /repos/OWNER/REPO/contents/FILE. body: optional JSON string. Returns the HTTP status and response body. Full GitHub access.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"method":{"type":"string"},"path":{"type":"string","description":"API path beginning with / (host api.github.com is implied)"},"body":{"type":"string","description":"optional JSON request body"}},"required":["method","path"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			if msg, ok := need(); !ok {
				return msg, nil
			}
			var in struct{ Method, Path, Body string }
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Method) == "" || strings.TrimSpace(in.Path) == "" {
				return "NOT performed: github_api requires method and path", nil
			}
			var body []byte
			if strings.TrimSpace(in.Body) != "" {
				body = []byte(in.Body)
			}
			status, data, err := githubDo(ctx, client, pol, token, in.Method, in.Path, body)
			if err != nil {
				return fmt.Sprintf("github_api failed: %v", err), nil
			}
			return fmt.Sprintf("HTTP %d\n%s", status, string(data)), nil
		},
	}

	createRepo := Tool{
		ToolSpec: ToolSpec{
			Name:        "github_create_repo",
			Description: "Create a new GitHub repository under the authenticated account (auto-initialised with a README so it has a default branch). Returns the repo full_name and URL.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"private":{"type":"boolean","description":"default false"},"description":{"type":"string"}},"required":["name"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			if msg, ok := need(); !ok {
				return msg, nil
			}
			var in struct {
				Name        string `json:"name"`
				Private     bool   `json:"private"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Name) == "" {
				return "NOT performed: github_create_repo requires name", nil
			}
			body, _ := json.Marshal(map[string]any{"name": in.Name, "private": in.Private, "description": in.Description, "auto_init": true})
			status, data, err := githubDo(ctx, client, pol, token, http.MethodPost, "/user/repos", body)
			if err != nil {
				return fmt.Sprintf("github_create_repo failed: %v", err), nil
			}
			if status < 200 || status >= 300 {
				return fmt.Sprintf("github_create_repo HTTP %d: %s", status, string(data)), nil
			}
			var r struct {
				FullName string `json:"full_name"`
				HTMLURL  string `json:"html_url"`
			}
			_ = json.Unmarshal(data, &r)
			return fmt.Sprintf("created repo %s -> %s", r.FullName, r.HTMLURL), nil
		},
	}

	uploadFile := Tool{
		ToolSpec: ToolSpec{
			Name:        "github_upload_file",
			Description: "Upload (create or update) a file from the worktree to a GitHub repo via the Contents API. path = worktree file to upload. repo = OWNER/NAME (omit to use the configured default). dest = path in the repo (omit to reuse the file's path). Returns the file URL.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"worktree file to upload"},"repo":{"type":"string","description":"OWNER/NAME; optional if a default repo is configured"},"dest":{"type":"string","description":"destination path in the repo; defaults to path"},"message":{"type":"string","description":"commit message"}},"required":["path"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			if msg, ok := need(); !ok {
				return msg, nil
			}
			var in struct{ Path, Repo, Dest, Message string }
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Path) == "" {
				return "NOT performed: github_upload_file requires path", nil
			}
			repo := strings.TrimSpace(in.Repo)
			if repo == "" {
				repo = defaultRepo
			}
			if repo == "" {
				return "NOT performed: no repo given and no default repo configured (set GITHUB_REPO)", nil
			}
			dest := strings.TrimSpace(in.Dest)
			if dest == "" {
				dest = in.Path
			}
			// Read the worktree file (os.Root-confined).
			r, rel, err := s.open(in.Path)
			if err != nil {
				return "", err
			}
			defer r.Close()
			f, err := r.OpenFile(rel, os.O_RDONLY, 0)
			if err != nil {
				return fmt.Sprintf("NOT performed: cannot open %s (%v)", in.Path, err), nil
			}
			content, rerr := io.ReadAll(f)
			f.Close()
			if rerr != nil {
				return "", rerr
			}
			msg := in.Message
			if strings.TrimSpace(msg) == "" {
				msg = "add " + dest + " via local-agent"
			}
			// If the file already exists we must send its blob sha to update it.
			sha := ""
			if st, sd, gerr := githubDo(ctx, client, pol, token, http.MethodGet, "/repos/"+repo+"/contents/"+dest, nil); gerr == nil && st == 200 {
				var cur struct {
					SHA string `json:"sha"`
				}
				_ = json.Unmarshal(sd, &cur)
				sha = cur.SHA
			}
			put := map[string]any{"message": msg, "content": base64.StdEncoding.EncodeToString(content)}
			if sha != "" {
				put["sha"] = sha
			}
			body, _ := json.Marshal(put)
			status, data, err := githubDo(ctx, client, pol, token, http.MethodPut, "/repos/"+repo+"/contents/"+dest, body)
			if err != nil {
				return fmt.Sprintf("github_upload_file failed: %v", err), nil
			}
			if status < 200 || status >= 300 {
				return fmt.Sprintf("github_upload_file HTTP %d: %s", status, string(data)), nil
			}
			var res struct {
				Content struct {
					HTMLURL string `json:"html_url"`
				} `json:"content"`
			}
			_ = json.Unmarshal(data, &res)
			return fmt.Sprintf("uploaded %s to %s -> %s", in.Path, repo+"/"+dest, res.Content.HTMLURL), nil
		},
	}

	return []Tool{apiTool, createRepo, uploadFile}
}
