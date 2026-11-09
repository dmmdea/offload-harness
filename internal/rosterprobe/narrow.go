package rosterprobe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// OutsideRosterError is a call that named a fleet node that is not in delegate_remotes. Base is the node as the caller
// spelled it, trimmed; Error prints it redacted, because an entry may carry a token and these lines reach chats.
type OutsideRosterError struct{ Base string }

func (e *OutsideRosterError) Error() string {
	return fmt.Sprintf("remote %s is not in delegate_remotes: a call may narrow the configured fleet, never extend it",
		Member{Base: e.Base}.Shown())
}

// Narrow is the ONE rule every lane's `remotes` parameter follows: the fleet nodes a call asked for, each of which must also
// be one of the configured delegate_remotes, so a call can narrow the fleet and never reach past the operator's list. Blank
// entries and trailing slashes are ignored. With nothing asked it returns nil and no error, which a caller reads as "the
// whole configured roster". A base outside the roster is an *OutsideRosterError naming it.
func Narrow(configured, asked []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, b := range configured {
		if b = normalize(b); b != "" {
			allowed[b] = true
		}
	}
	var out []string
	for _, b := range asked {
		if b = normalize(b); b == "" {
			continue
		}
		if !allowed[b] {
			return nil, &OutsideRosterError{Base: b}
		}
		out = append(out, b)
	}
	return out, nil
}

// NarrowConfig is cfg with delegate_remotes narrowed to the nodes a call asked for (see Narrow), so a lane that probes
// cfg.DelegateRemotes needs no other change to be pinned to a node. With nothing asked it returns cfg untouched. cfg is a
// value: the caller's config is never edited.
// NarrowFor is Narrow over cfg's delegate_remotes, with one refinement: a named node that is outside the roster because
// the caller-scope policy removed it (delegate_remote_scopes) is refused with that reason, not as "not in delegate_remotes",
// so a caller learns the node exists and why it may not use it.
func NarrowFor(cfg config.Config, asked []string) ([]string, error) {
	bases, err := Narrow(cfg.DelegateRemotes, asked)
	var out *OutsideRosterError
	if errors.As(err, &out) {
		if serr := cfg.ScopeRefusal(out.Base); serr != nil {
			return nil, serr
		}
	}
	return bases, err
}

func NarrowConfig(cfg config.Config, asked []string) (config.Config, error) {
	bases, err := NarrowFor(cfg, asked)
	if err != nil {
		return cfg, err
	}
	if len(bases) > 0 {
		cfg.DelegateRemotes = bases
	}
	return cfg, nil
}

// NarrowLane is NarrowConfig for a door: the single-shot spill lanes (transcribe, vision, text, forecast, compose) call it
// first and run on the config it returns. ok is false when the call named a node outside delegate_remotes; res is then the
// deferred, contract-class answer naming it, and the lane must not run.
func NarrowLane(cfg config.Config, asked []string) (narrowed config.Config, res core.Result, ok bool) {
	narrowed, err := NarrowConfig(cfg, asked)
	if err != nil {
		res = core.Deferf(err.Error(), "", core.Meta{})
		res.DeferClass = core.DeferClassContract
		return cfg, res, false
	}
	return narrowed, core.Result{}, true
}

func normalize(b string) string { return strings.TrimRight(strings.TrimSpace(b), "/") }
