// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D): the global --host flag and the named-remote table.
// Not a command — no pp:data-source marker.

package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
)

// baseURLEnv is the environment variable the generated config loader applies
// LAST, after the config file. --host is wired through it deliberately: every
// base-URL helper in this CLI (the generated client, the spine's
// spineBaseURL, the measurement family's mcBaseURL, the config family's
// loopbackBaseURL) reads config.Load, so one assignment redirects all of them.
// The alternative — teaching four separate helpers about remotes — would leave
// whichever one was missed silently pointed at the local box.
const baseURLEnv = "LLAMASWAP_BASE_URL"

// hostFlagByFlags maps a root flag set to its --host value. A package-level
// string would be shared between the CLI root and the MCP mirror's root, which
// both call RootCmd() in the same process.
var hostFlagByFlags sync.Map // *rootFlags -> *string

// init registers the --host persistent flag and wraps the generated root's
// PersistentPreRunE so the override is applied before any command runs.
// Additive: the generated hook is called, never replaced.
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		if root.PersistentFlags().Lookup("host") != nil {
			return
		}
		host := new(string)
		hostFlagByFlags.Store(flags, host)
		root.PersistentFlags().StringVar(host, "host", "",
			"Target a named remote from the CLI config's \"remotes\" map, or a bare URL/host:port. Default: the configured base_url (this box).")

		generated := root.PersistentPreRunE
		root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
			// Applied BEFORE the generated hook: anything it does that reads
			// configuration must already see the remote.
			if err := applyHostOverride(flags); err != nil {
				return err
			}
			if generated != nil {
				return generated(cmd, args)
			}
			return nil
		}
	})
}

// remoteTable is the named-remote map read from the CLI's own config.json.
// Parsed into a wave-local struct so the generated config type stays
// generator-owned.
type remoteTable struct {
	// Remotes maps a short name to a base URL.
	Remotes map[string]string `json:"remotes"`
	// Source records which file the table came from, for error messages.
	Source string `json:"-"`
}

// loadRemotes reads the "remotes" map from the CLI config. A missing or
// unparseable file yields an empty table rather than an error: --host with a
// literal URL must keep working on a box that has no remotes configured.
func loadRemotes(flags *rootFlags) remoteTable {
	out := remoteTable{Remotes: map[string]string{}}
	path := ""
	if flags != nil {
		path = flags.configPath
	}
	if path == "" {
		if env := strings.TrimSpace(os.Getenv("LLAMASWAP_CONFIG")); env != "" {
			path = env
		} else if dir, err := cliutil.ConfigDir(); err == nil {
			path = dir + string(os.PathSeparator) + "config.json"
		}
	}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var parsed remoteTable
	if err := json.Unmarshal(data, &parsed); err != nil {
		return out
	}
	if parsed.Remotes == nil {
		parsed.Remotes = map[string]string{}
	}
	parsed.Source = path
	return parsed
}

// remoteNames returns the configured remote names, sorted, for help and error
// messages.
func (t remoteTable) remoteNames() []string {
	names := make([]string, 0, len(t.Remotes))
	for n := range t.Remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveHost turns a --host value into a base URL.
//
// Precedence: a configured remote name wins over URL interpretation, so a
// remote may be named after a hostname without becoming ambiguous. Anything
// else is treated as a URL, gaining an http:// scheme if it has none.
//
// MagicDNS / LAN hostnames are allowed here and are NOT rewritten to a loopback
// literal. The 127.0.0.1 rule in this CLI is about the LOCAL proxy — a
// hostname that resolves off-box is exactly what a remote is.
func resolveHost(value string, table remoteTable) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil
	}
	if target, ok := table.Remotes[v]; ok {
		return strings.TrimRight(strings.TrimSpace(target), "/"), nil
	}
	looksLikeURL := strings.Contains(v, "://")
	if !looksLikeURL {
		// A bare name with no dot, no colon, and no slash was almost certainly
		// meant as a remote name. Guessing "http://<name>" for a typo'd remote
		// produces a confusing connection error instead of a usable one.
		if !strings.ContainsAny(v, ".:/") {
			names := table.remoteNames()
			if len(names) == 0 {
				return "", usageErr(fmt.Errorf(
					"--host %q is not a URL and no remotes are configured; add {\"remotes\": {%q: \"http://host:11436\"}} to the CLI config, or pass a full URL", v, v))
			}
			return "", usageErr(fmt.Errorf(
				"--host %q is not a configured remote; available: %s (from %s)", v, strings.Join(names, ", "), table.Source))
		}
		// host:port or a bare hostname with a dot — assume plain HTTP, which
		// is what llama-swap serves.
		if _, _, err := net.SplitHostPort(v); err != nil {
			v += ":11436"
		}
		v = "http://" + v
	}
	return strings.TrimRight(v, "/"), nil
}

// applyHostOverride resolves --host and points the whole process at it.
func applyHostOverride(flags *rootFlags) error {
	raw, ok := hostFlagByFlags.Load(flags)
	if !ok {
		return nil
	}
	value := strings.TrimSpace(*raw.(*string))
	if value == "" {
		return nil
	}
	base, err := resolveHost(value, loadRemotes(flags))
	if err != nil {
		return err
	}
	if base == "" {
		return nil
	}
	return os.Setenv(baseURLEnv, base)
}
