package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// runInstallClient renders the config of a delegation client: a machine that runs the harness only to
// place work on fleet nodes (agent_delegate, and offload_compose_video with no lane of its own, ADR
// 0070). It has no local model, no media lane and no fleet service, and its config says so explicitly:
// every model route and every script binding that has a default is written empty, so status and
// acceptance never claim a lane this box does not have. The fleet token is read from a file and never
// printed; the config is written with mode 0600.
func runInstallClient(args []string) error {
	fs := flag.NewFlagSet("install client", flag.ExitOnError)
	remotes := fs.String("remotes", "", "comma-separated fleet node bases (http://<node>:18811), in preference order")
	tokenFile := fs.String("token-file", "", "file holding the fleet token (the value every node and delegator configures)")
	home := fs.String("home", "", "the client's harness home; media and state live under it")
	out := fs.String("config", "", "config file to write (default <home>/etc/config.json)")
	force := fs.Bool("force", false, "replace an existing config")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	_ = fs.Parse(args)

	if *home == "" {
		return errors.New("install client: --home is required")
	}
	var bases []string
	for _, r := range strings.Split(*remotes, ",") {
		if r = strings.TrimRight(strings.TrimSpace(r), "/"); r != "" {
			bases = append(bases, r)
		}
	}
	if len(bases) == 0 {
		return errors.New("install client: --remotes is required (at least one fleet node)")
	}
	if *tokenFile == "" {
		return errors.New("install client: --token-file is required")
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("install client: reading the token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return errors.New("install client: the token file is empty")
	}
	path := *out
	if path == "" {
		path = filepath.Join(*home, "etc", "config.json")
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("install client: %s exists; pass --force to replace it", path)
	}

	m := clientConfig(*home, bases, token)
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for _, d := range []string{"media", "state"} {
		if err := os.MkdirAll(filepath.Join(*home, d), 0o755); err != nil {
			return err
		}
	}
	// Written to a fresh temp file (created 0600) and renamed into place: os.WriteFile keeps the mode of
	// a file it replaces, so --force over an old world-readable config would have left the token in one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return werr
	}
	// The file must load as a config the harness accepts, and its remotes must be fleet node bases: on
	// a client they are the only thing it can reach, so what doctor would only warn about (a remote off
	// the fleet port, a loopback base, a /v1 suffix) is a client that can do nothing. Either refusal is
	// reported and the file, which holds the token, removed rather than left half-valid.
	cfg, err := config.Load(path)
	if err != nil {
		return refuseConfig(path, fmt.Errorf("install client: the rendered config does not load: %w", err))
	}
	if warns := config.EndpointWarnings(cfg); len(warns) > 0 {
		return refuseConfig(path, fmt.Errorf("install client: the remotes are not fleet node bases: %s", strings.Join(warns, "; ")))
	}
	unbound := []string{}
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			unbound = append(unbound, k)
		}
	}
	sort.Strings(unbound)
	if *asJSON {
		res := map[string]any{"ok": true, "config": path, "delegate_remotes": bases, "token": "from " + *tokenFile, "unbound": unbound}
		jb, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(jb))
		return nil
	}
	fmt.Printf("OK    delegation client config written to %s (mode 0600)\n", path)
	fmt.Printf("      delegate_remotes: %s\n", strings.Join(bases, ", "))
	fmt.Printf("      unbound here (served by the fleet): %s\n", strings.Join(unbound, ", "))
	fmt.Printf("NEXT  register the MCP server: claude mcp add local-offload --scope user -- <this binary> mcp --config %s\n", path)
	return nil
}

// refuseConfig removes a refused config, which holds the token, and returns why it was refused; a file
// that could not be removed is named, so it is never left behind unsaid.
func refuseConfig(path string, why error) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (and %s, which holds the fleet token, could not be removed: %v)", why, path, err)
	}
	return why
}

// hasLocalModel reports whether the config names any local model at all. A delegation client names
// none, so acceptance and doctor have no local roster or endpoint to check on it.
func hasLocalModel(cfg config.Config) bool {
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" {
			return true
		}
	}
	return false
}

// clientConfig is the delegation client's config as a JSON object: where its own files live, the fleet it
// delegates to, and every model route and default script binding left empty.
func clientConfig(home string, remotes []string, token string) map[string]any {
	m := map[string]any{
		"media_dir":                filepath.ToSlash(filepath.Join(home, "media")),
		"state_dir":                filepath.ToSlash(filepath.Join(home, "state")),
		"cache_path":               filepath.ToSlash(filepath.Join(home, "state", "cache.db")),
		"ledger_path":              filepath.ToSlash(filepath.Join(home, "state", "ledger.jsonl")),
		"delegate_remotes":         remotes,
		"fleet_auth_token":         token,
		"agent_delegation_enabled": true,
	}
	def := config.Default()
	v, t := reflect.ValueOf(def), reflect.TypeOf(def)
	for i := 0; i < t.NumField(); i++ {
		tag := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if strings.HasSuffix(tag, "_script") && v.Field(i).Kind() == reflect.String && v.Field(i).String() != "" {
			m[tag] = ""
		}
	}
	for _, r := range def.ModelRoutes() {
		m[r.Key] = ""
	}
	// ffmpeg is a local CPU tool (offload_media, and the kit's cut scripts run it beside the harness):
	// the default stays when this machine has ffmpeg and ffprobe on PATH, and is unbound when it does
	// not, so the media route never claims a binary the box does not have.
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			m["ffmpeg_path"] = ""
			break
		}
	}
	return m
}
