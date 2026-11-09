package config

// The caller-scope policy (2026-11-09). A node may be declared to admit only some callers' work
// (delegate_remote_scopes), and the scope of THIS process's caller is resolved once when its config loads
// (internal/callscope, from caller_scope_roots). Every node list this config hands out is then filtered by the
// policy here, in the load, so no lane can reach an excluded node through the config: delegate_remotes,
// cascade_remote_lanes, seat_endpoints and fleet_queue_holder are filtered together, and the media ticket path,
// which re-reads delegate_remotes from the file, re-applies it through WithRoster.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/callscope"
)

// ScopeExclusion is one node base the caller-scope policy removed from this process's config: the base, the
// config key it was removed from, and the scopes the node admits.
type ScopeExclusion struct {
	Base   string   `json:"base"`
	List   string   `json:"list"`
	Admits []string `json:"admits"`
}

// scopeHostKey is the host a scope policy entry is keyed by: lower case, no scheme, port or path, and a DNS name
// cut to its first label, so "http://node-b:18811", "http://node-b:11436" and "http://node-b.tailnet.example:18811"
// are one machine. A policy is about a machine, never a port: a node's seat endpoints and its fleet node answer on
// different ports of the same box. An IP literal is kept whole (it cannot be matched to a name without DNS, so the
// operator lists the node under every spelling the node lists use; the load validator checks the keys exist).
func scopeHostKey(base string) string {
	s := strings.TrimSpace(base)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host
}

// scopeAdmits is the policy entry for base, and false when the policy does not list the machine.
func (c Config) scopeAdmits(base string) ([]string, bool) {
	want := scopeHostKey(base)
	if want == "" {
		return nil, false
	}
	for k, admits := range c.DelegateRemoteScopes {
		if scopeHostKey(k) == want {
			return admits, true
		}
	}
	return nil, false
}

// RemoteAdmitsScope reports whether the node at base may be given work of the named caller scope. A machine the
// policy does not list admits every caller, which is how every node behaved before the key. A listed machine
// admits only the scopes listed for it, and never "unknown" or an empty scope: the policy fails closed, so a
// caller that could not be identified is kept off every node that admits only some callers.
func (c Config) RemoteAdmitsScope(base, scope string) bool {
	admits, listed := c.scopeAdmits(base)
	if !listed {
		return true
	}
	if scope != callscope.Business && scope != callscope.Personal {
		return false
	}
	for _, a := range admits {
		if strings.ToLower(strings.TrimSpace(a)) == scope {
			return true
		}
	}
	return false
}

// ForScope returns c with the caller scope recorded and every node list filtered by delegate_remote_scopes:
// delegate_remotes, the bases of cascade_remote_lanes and seat_endpoints, and fleet_queue_holder. What it removes
// is listed in ScopeExcluded (offload_status and doctor print it). The maps are copied, never edited in place: a
// Config is a value shared by copy.
func (c Config) ForScope(s callscope.Scope) Config {
	c.CallerScope = s
	c.ScopeExcluded = nil
	if len(c.DelegateRemoteScopes) == 0 {
		return c
	}
	excluded := func(base, list string) {
		admits, _ := c.scopeAdmits(base)
		c.ScopeExcluded = append(c.ScopeExcluded, ScopeExclusion{Base: base, List: list, Admits: append([]string(nil), admits...)})
	}
	if len(c.DelegateRemotes) > 0 {
		keep := make([]string, 0, len(c.DelegateRemotes))
		for _, b := range c.DelegateRemotes {
			if c.RemoteAdmitsScope(b, s.Name) {
				keep = append(keep, b)
			} else {
				excluded(b, "delegate_remotes")
			}
		}
		c.DelegateRemotes = keep
	}
	filterMap := func(m map[string]string, list string) map[string]string {
		if len(m) == 0 {
			return m
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]string, len(m))
		for _, k := range keys {
			if c.RemoteAdmitsScope(m[k], s.Name) {
				out[k] = m[k]
			} else {
				excluded(m[k], list+"["+k+"]")
			}
		}
		return out
	}
	c.CascadeRemoteLanes = filterMap(c.CascadeRemoteLanes, "cascade_remote_lanes")
	c.SeatEndpoints = filterMap(c.SeatEndpoints, "seat_endpoints")
	if h := strings.TrimSpace(c.FleetQueueHolder); h != "" && !c.RemoteAdmitsScope(h, s.Name) {
		excluded(h, "fleet_queue_holder")
		c.FleetQueueHolder = ""
	}
	return c
}

// WithRoster is c with delegate_remotes replaced by a re-read roster and the caller-scope policy applied to it
// again. The media ticket path re-reads delegate_remotes from the config file while a session runs (a node added
// to the file joins it); the list it reads must pass the same filter the loaded one passed, or a re-read would put
// an excluded node back. A Config that never went through ForScope has no caller scope, which no policy admits.
func (c Config) WithRoster(remotes []string) Config {
	c.DelegateRemotes = remotes
	if len(c.DelegateRemoteScopes) == 0 {
		return c
	}
	keep := make([]string, 0, len(remotes))
	for _, b := range remotes {
		if c.RemoteAdmitsScope(b, c.CallerScope.Name) {
			keep = append(keep, b)
		}
	}
	c.DelegateRemotes = keep
	return c
}

// ScopeRefusal is the error a call that NAMES an excluded node gets: the node, its policy and the caller's scope,
// so the caller sees why instead of a bare "not in delegate_remotes". nil when the node admits the caller.
func (c Config) ScopeRefusal(base string) error {
	if c.RemoteAdmitsScope(base, c.CallerScope.Name) {
		return nil
	}
	admits, _ := c.scopeAdmits(base)
	scope := c.CallerScope.Name
	if scope == "" {
		scope = callscope.Unknown
	}
	why := c.CallerScope.Why
	if why == "" {
		why = "no caller scope was resolved"
	}
	return fmt.Errorf("node %s admits only %s callers (delegate_remote_scopes); this caller resolves %s (%s)", RedactBase(base), strings.Join(admits, ", "), scope, why)
}

// applyCallerScope resolves this process's caller scope and filters the node lists by the policy. It runs on
// every exit of the load (loadArmed), like the GPU gate, so no verb can proceed on an unfiltered config. A config
// with neither scope key and no override changes nothing and resolves nothing.
func applyCallerScope(c Config) Config {
	if len(c.DelegateRemoteScopes) == 0 && len(c.CallerScopeRoots) == 0 && strings.TrimSpace(os.Getenv(callscope.EnvScope)) == "" {
		return c
	}
	if s, ok := callscope.Process(); ok {
		return c.ForScope(s)
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	callscope.SetProcess(callscope.Resolve(callscope.Inputs{
		Getenv: os.Getenv,
		PPID:   os.Getppid(),
		Home:   home,
		Cwd:    cwd,
		Roots:  c.CallerScopeRoots,
		Wait:   1500 * time.Millisecond,
	}))
	s, _ := callscope.Process()
	return c.ForScope(s)
}

// validateCallerScope refuses the scope keys by name: a scope word that is not business or personal, an empty
// root or admit list, and a delegate_remote_scopes key that names no node any node list of this config uses. A
// policy that fails open on a typo is worse than none: a misspelled host would fence nothing, and the node it was
// meant for would still be sent the work.
func validateCallerScope(c Config) error {
	valid := func(w string) bool {
		w = strings.ToLower(strings.TrimSpace(w))
		return w == callscope.Business || w == callscope.Personal
	}
	rootKeys := make([]string, 0, len(c.CallerScopeRoots))
	for k := range c.CallerScopeRoots {
		rootKeys = append(rootKeys, k)
	}
	sort.Strings(rootKeys)
	for _, k := range rootKeys {
		if !valid(k) {
			return fmt.Errorf("caller_scope_roots[%q]: unknown scope (valid: %q, %q)", k, callscope.Business, callscope.Personal)
		}
		for _, r := range c.CallerScopeRoots[k] {
			if strings.TrimSpace(r) == "" {
				return fmt.Errorf("caller_scope_roots[%q] has an empty root", k)
			}
		}
	}
	for _, w := range c.FleetAdmitScopes {
		if !valid(w) {
			return fmt.Errorf("fleet_admit_scopes: unknown scope %q (valid: %q, %q)", w, callscope.Business, callscope.Personal)
		}
	}
	if len(c.DelegateRemoteScopes) == 0 {
		return nil
	}
	hosts := map[string]bool{}
	for _, b := range c.DelegateRemotes {
		hosts[scopeHostKey(b)] = true
	}
	for _, b := range c.CascadeRemoteLanes {
		hosts[scopeHostKey(b)] = true
	}
	for _, b := range c.SeatEndpoints {
		hosts[scopeHostKey(b)] = true
	}
	if h := strings.TrimSpace(c.FleetQueueHolder); h != "" {
		hosts[scopeHostKey(h)] = true
	}
	keys := make([]string, 0, len(c.DelegateRemoteScopes))
	for k := range c.DelegateRemoteScopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		hk := scopeHostKey(k)
		if hk == "" || !hosts[hk] {
			return fmt.Errorf("delegate_remote_scopes[%q] names no node of delegate_remotes, cascade_remote_lanes, seat_endpoints or fleet_queue_holder, so it would fence nothing; spell the node's host as those lists do", RedactBase(k))
		}
		if len(c.DelegateRemoteScopes[k]) == 0 {
			return fmt.Errorf("delegate_remote_scopes[%q] lists no scope; a node that admits no caller is removed from delegate_remotes instead", RedactBase(k))
		}
		for _, w := range c.DelegateRemoteScopes[k] {
			if !valid(w) {
				return fmt.Errorf("delegate_remote_scopes[%q]: unknown scope %q (valid: %q, %q)", RedactBase(k), w, callscope.Business, callscope.Personal)
			}
		}
	}
	return nil
}
