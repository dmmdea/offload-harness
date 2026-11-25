package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/servedwindow"
)

// writeAgentWindowSection prints one row comparing the context window this box's agent_ctx_tokens key
// states with what the seat's engine serves, so a key left behind by an edit to the serving config (or a
// vLLM seat that sized itself at load) is named instead of advertised (ADR 0106).
//
// What the seat serves comes from two places, never from the engine itself (doctor loads nothing): the
// window the engine last reported to a run or a status call on this box (seat-rates.json, valid only while
// the seat's serving-config entry is the one it was read under), else the window the serving config's own
// flags state for a llama.cpp seat (--ctx-size over --parallel, or whole under --kv-unified). A vLLM seat
// states no window in the file, so until it has served once the row is UNKNOWN.
//
//	window:     OK    agent_ctx_tokens 49152 = <seat> serving config states 49152 (<yaml>)
//	window:     WARN  agent_ctx_tokens 131072 exceeds what <seat> serves (49152, serving config states it in <yaml>); this node advertises 49152; correct the key
//	window:     WARN  agent_ctx_tokens 8192 is below what <seat> serves (113600, the engine reported it at <time>); this node advertises 113600; raise the key
//	window:     UNKNOWN agent_ctx_tokens 163840: <why nothing says what the seat serves>
//
// It is informational: it never changes doctor's exit code, and it prints nothing on a config with no
// agent seat.
func writeAgentWindowSection(w io.Writer, cfg config.Config) {
	seat := strings.TrimSpace(cfg.AgentModel)
	if seat == "" {
		return
	}
	key := cfg.AgentCtxTokens
	have := fmt.Sprintf("agent_ctx_tokens %d", key)
	if key <= 0 {
		have = "agent_ctx_tokens is unset"
	}
	f := servedwindow.Look(cfg, seat)

	var truth int
	var where string
	switch {
	case f.RememberedOK:
		truth = f.Remembered
		where = "the engine reported it at " + f.RememberedAt.Format(time.RFC3339)
		if !f.Confirmed {
			where += ", and the seat has loaded since"
		}
		if f.Serving > 0 && f.Serving != f.Remembered {
			where += fmt.Sprintf("; the serving config states %d", f.Serving)
		}
	case f.Serving > 0:
		truth = f.Serving
		where = "the serving config states it in " + f.Path
	default:
		why := f.ServingWhy
		if why == "" {
			why = "the serving config states no window"
		}
		fmt.Fprintf(w, "window:     UNKNOWN %s: nothing says what agent seat %q serves, and it has not served a window this box remembers: %s\n", have, seat, why)
		return
	}
	advertised := servedwindow.Known(cfg, seat, key).Tokens
	adv := fmt.Sprintf("this node advertises %d", advertised)
	if advertised == 0 {
		adv = "this node advertises no window"
	}
	switch {
	case key == truth:
		fmt.Fprintf(w, "window:     OK    %s = what %s serves (%d, %s)\n", have, seat, truth, where)
	case key <= 0:
		fmt.Fprintf(w, "window:     OK    %s, so no window is advertised; %s serves %d (%s); set agent_ctx_tokens %d to advertise it\n", have, seat, truth, where, truth)
	case key > truth:
		fmt.Fprintf(w, "window:     WARN  %s exceeds what %s serves (%d, %s); %s; correct the key\n", have, seat, truth, where, adv)
	default:
		fmt.Fprintf(w, "window:     WARN  %s is below what %s serves (%d, %s); %s; raise the key (a window the key understates is work the delegator never sends)\n", have, seat, truth, where, adv)
	}
}
