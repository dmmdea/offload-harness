// Package servingtmpl renders a llama-swap serving config from a tier's parameters.
//
// Rendering lived in install.ps1, so a Linux node had no way to produce a serving
// config at all — every Linux deployment hand-wrote one. On the measured 6 GB node
// the first two hand-written topologies each broke the box (an exclusive media group
// returned 502s for a full TTL after any render; the embedder inside the swapping
// tier evicted the chat model on every RAG query), which is what a rendered,
// reviewed template exists to prevent.
//
// Substitution is deliberately dumb — tokens in, values out — with one guard that
// matters: Render REFUSES to emit a config that still contains a token. install.ps1
// carries a comment about exactly that failure ("the fully-tokenized templates would
// otherwise leave __CTX__ etc. in a config"), and a llama-swap that starts with
// `--ctx-size __CTX__` fails in a way that reads like a model problem.
package servingtmpl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Params are the per-machine values a tier's template needs. Everything here comes
// from the classified tier (ctx/kv/flash/moe), the install layout (bin/models), or
// the box itself (threads) — never from the machine doing the rendering.
type Params struct {
	LlamaBin  string // dir holding llama-server AND its shared objects (LD_LIBRARY_PATH)
	ModelsDir string
	Listen    string // "127.0.0.1:11436"
	Ctx       int
	KVType    string // applied to both K and V; kept symmetric on purpose
	FlashAttn string // "on"/"off"
	MoE26B    string // the flag form the tier chose: -ngl 99 | --cpu-moe | --n-cpu-moe N
	Threads   int
	// Include26B false removes the 26B seat entirely — model block AND group
	// membership. A member naming a model that does not exist is a config llama-swap
	// rejects at startup, so dropping one without the other bricks the service.
	Include26B bool
}

var tokenRe = regexp.MustCompile(`__[A-Z0-9_]+__`)

// Render substitutes p into tmpl and returns the serving config.
func Render(tmpl string, p Params) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	out := tmpl
	if !p.Include26B {
		var err error
		if out, err = drop26B(out); err != nil {
			return "", err
		}
	}
	for from, to := range map[string]string{
		"__LLAMA_BIN__":  strings.TrimRight(p.LlamaBin, "/"),
		"__MODELS__":     strings.TrimRight(p.ModelsDir, "/"),
		"__LISTEN__":     p.Listen,
		"__CTX__":        fmt.Sprint(p.Ctx),
		"__KV_K__":       p.KVType,
		"__KV_V__":       p.KVType,
		"__FLASH_ATTN__": p.FlashAttn,
		"__MOE_26B__":    p.MoE26B,
		"__NTHREADS__":   fmt.Sprint(p.Threads),
	} {
		out = strings.ReplaceAll(out, from, to)
	}
	// A leftover token would start a server with a literal "__CTX__" argument, which
	// fails looking like a model problem. Refuse instead, naming what is unresolved.
	if left := uniqueTokens(out); len(left) > 0 {
		return "", fmt.Errorf("unresolved template token(s) after rendering: %s", strings.Join(left, ", "))
	}
	return out, nil
}

func (p Params) validate() error {
	var missing []string
	if p.LlamaBin == "" {
		missing = append(missing, "llama bin dir")
	}
	if p.ModelsDir == "" {
		missing = append(missing, "models dir")
	}
	if p.Listen == "" {
		missing = append(missing, "listen address")
	}
	if p.Ctx <= 0 {
		missing = append(missing, "ctx size")
	}
	if p.KVType == "" {
		missing = append(missing, "kv type")
	}
	if p.FlashAttn == "" {
		missing = append(missing, "flash-attn")
	}
	if p.Threads <= 0 {
		missing = append(missing, "threads")
	}
	if p.Include26B && strings.TrimSpace(p.MoE26B) == "" {
		missing = append(missing, "26B MoE placement (the tier includes the 26B but named no placement flag)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("serving template needs: %s", strings.Join(missing, ", "))
	}
	return nil
}

// drop26B removes the 26B model block and every group membership naming it. Both,
// or llama-swap refuses the config at startup for referencing an unknown member.
func drop26B(tmpl string) (string, error) {
	const model = "gemma4-26b-a4b"
	lines := strings.Split(tmpl, "\n")
	var out []string
	skipping := false
	for _, l := range lines {
		if strings.HasPrefix(l, "  "+model+":") {
			skipping = true
			continue
		}
		if skipping {
			// The block ends at the next key at the same indent (two spaces, no more).
			if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ":") {
				skipping = false
			} else {
				continue
			}
		}
		if strings.Contains(l, "members:") && strings.Contains(l, model) {
			l = removeMember(l, model)
		}
		out = append(out, l)
	}
	if skipping {
		return "", fmt.Errorf("template ended inside the %s block — its shape changed", model)
	}
	res := strings.Join(out, "\n")
	if strings.Contains(res, model) {
		return "", fmt.Errorf("%s still referenced after removal — a config llama-swap would reject", model)
	}
	return res, nil
}

// removeMember drops one entry from an inline YAML flow sequence.
func removeMember(line, member string) string {
	open, close := strings.Index(line, "["), strings.LastIndex(line, "]")
	if open < 0 || close < open {
		return line
	}
	var kept []string
	for _, m := range strings.Split(line[open+1:close], ",") {
		if strings.TrimSpace(m) != member {
			kept = append(kept, strings.TrimSpace(m))
		}
	}
	return line[:open+1] + strings.Join(kept, ", ") + line[close:]
}

func uniqueTokens(s string) []string {
	seen := map[string]bool{}
	for _, m := range tokenRe.FindAllString(s, -1) {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
