package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// KVCacheServers is the box's cache-server bindings, ONE PER vLLM SEAT.
//
// It is a LIST because the store is not a property of one favoured seat: the
// operator directive of 2026-09-10 is that the second device's store backs EVERY
// vLLM seat while that device is online — the tensor-parallel pair, the opt-in
// 3-card layout, and the second box's own small seat, each with its own store
// directory and namespace. The shape this replaces bound one `kv_cache_server.seat`
// to a single seat name, which made "the other seats have no tier" indistinguishable
// from "the other seats were never considered"; that is the recorded defect (B-01).
//
// BACKWARD COMPATIBLE BY CONSTRUCTION: a config written against 0.117 and earlier
// carries ONE object, and it decodes here to a one-element list bound to its own
// `seat` (or to the box default when it named none). Nothing an operator already
// deployed has to change to keep loading.
type KVCacheServers []*KVCacheServer

// UnmarshalJSON accepts BOTH shapes — the list (current) and the single object
// (0.117 and earlier). A decode failure names the key and both shapes, because the
// one thing an operator must not have to guess is which of two spellings a file was
// rejected for.
func (l *KVCacheServers) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || string(t) == "null" {
		*l = nil
		return nil
	}
	if t[0] == '[' {
		var out []*KVCacheServer
		if err := json.Unmarshal(t, &out); err != nil {
			return fmt.Errorf("kv_cache_server: %w (it is a LIST of per-seat bindings; the pre-0.121 single object is still accepted)", err)
		}
		*l = out
		return nil
	}
	var one KVCacheServer
	if err := json.Unmarshal(t, &one); err != nil {
		return fmt.Errorf("kv_cache_server: %w (expected a LIST of per-seat bindings, or the pre-0.121 single object)", err)
	}
	*l = KVCacheServers{&one}
	return nil
}

// For returns the binding that backs seat: the one that NAMES it, else the box
// default (a binding that names no seat), else nil. An exact match always wins, so
// a box can declare one default store and still give a single seat its own
// directory and namespace.
func (l KVCacheServers) For(seat string) *KVCacheServer {
	seat = strings.TrimSpace(seat)
	var def *KVCacheServer
	for _, k := range l {
		if k == nil {
			continue
		}
		name := strings.TrimSpace(k.Seat)
		if name != "" && name == seat {
			return k
		}
		if name == "" && def == nil {
			def = k
		}
	}
	return def
}

// Default is the binding that names no seat (the box-wide default), or nil.
func (l KVCacheServers) Default() *KVCacheServer {
	for _, k := range l {
		if k != nil && strings.TrimSpace(k.Seat) == "" {
			return k
		}
	}
	return nil
}

// AnyEnabled reports whether ANY binding is an enabled store — the one-bit answer
// to "does this box have a cache-server tier at all".
func (l KVCacheServers) AnyEnabled() bool {
	for _, k := range l {
		if k != nil && k.Enabled && !k.Storeless {
			return true
		}
	}
	return false
}

// Covers reports whether seat has a DELIBERATE binding: an enabled store, or an
// explicit storeless opt-out. A binding that is merely present and disabled does
// not count — "the tier is off for this seat and nobody said why" is precisely the
// silence the gate exists to catch.
func (l KVCacheServers) Covers(seat string) bool {
	b := l.For(seat)
	if b == nil {
		return false
	}
	return b.Storeless || b.Enabled
}

// UnboundSeats names the seats of `seats` that no binding covers. `doctor` fails on
// it and `offload_status` publishes it, from ONE implementation, so the gate and the
// report can never disagree about which seats are uncovered.
func (l KVCacheServers) UnboundSeats(seats []string) []string {
	var out []string
	for _, s := range seats {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if !l.Covers(s) {
			out = append(out, s)
		}
	}
	return out
}

// BoundSeatsNotDeclared names the seats a binding claims that `vllm_seats` does not
// list. It is reported, never fatal: a binding left behind after a seat was renamed
// is a store nothing reads, and the operator should see it — but refusing the load
// would break a box the moment it retired a seat, which is the wrong trade for a
// tier that is optional by charter.
func (l KVCacheServers) BoundSeatsNotDeclared(seats []string) []string {
	declared := map[string]bool{}
	for _, s := range seats {
		declared[strings.TrimSpace(s)] = true
	}
	var out []string
	for _, k := range l {
		if k == nil {
			continue
		}
		name := strings.TrimSpace(k.Seat)
		if name != "" && !declared[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// generation is the stack generation a binding declares — the pair B-45's namespace
// rule compares: the KV dtype and the tensor-parallel width that wrote the pages.
func (k KVCacheServer) generation() string {
	return strings.ToLower(strings.TrimSpace(k.KVDtype)) + "/tp" + strconv.Itoa(k.TensorParallel)
}

// generationDeclared reports whether the binding said which layout writes its pages.
func (k KVCacheServer) generationDeclared() bool {
	return strings.TrimSpace(k.KVDtype) != "" && k.TensorParallel > 0
}

// seatLabel names a binding in a refusal: its seat, or "(box default)".
func (k KVCacheServer) seatLabel() string {
	if s := strings.TrimSpace(k.Seat); s != "" {
		return strconv.Quote(s)
	}
	return "(box default)"
}

// ValidateKVCacheServers validates every binding AND the two rules that exist only
// BETWEEN bindings:
//
//  1. One binding per seat, and at most one box default. Two bindings for one seat
//     is not a merge — it is two stores whose order in the file decides which one
//     the seat gets.
//  2. A key_prefix shared by two seats must be shared DELIBERATELY: every binding on
//     it must declare the SAME stack generation (kv_dtype + tensor_parallel). Pages
//     written under another layout are not stale, they are unreadable — LMCache
//     fails the read with "value size exceeds buffer capacity" and the tier serves
//     nothing while reporting success (measured 2026-09-03 00:25). An UNDECLARED
//     generation on a shared prefix is refused for the same reason: it cannot be
//     shown to be safe, and "probably fine" is how that one shipped.
func ValidateKVCacheServers(l KVCacheServers) error {
	seen := map[string]bool{}
	for i, k := range l {
		if k == nil {
			return fmt.Errorf("kv_cache_server[%d]: a null binding — remove it", i)
		}
		if err := ValidateKVCacheServer(k); err != nil {
			return fmt.Errorf("kv_cache_server binding %d %s: %w", i, k.seatLabel(), err)
		}
		seat := strings.TrimSpace(k.Seat)
		if seen[seat] {
			return fmt.Errorf("kv_cache_server: two bindings for seat %s — one binding per seat (the second is a store that seat never gets)", k.seatLabel())
		}
		seen[seat] = true
	}
	// Rule 2 runs over STORE bindings only: a storeless opt-out writes no pages and
	// therefore shares no namespace.
	byPrefix := map[string][]*KVCacheServer{}
	for _, k := range l {
		if !k.Enabled || k.Storeless {
			continue
		}
		p := k.EffectiveKeyPrefix()
		byPrefix[p] = append(byPrefix[p], k)
	}
	prefixes := make([]string, 0, len(byPrefix))
	for p := range byPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		group := byPrefix[p]
		if len(group) < 2 {
			continue
		}
		for _, k := range group {
			if !k.generationDeclared() {
				return fmt.Errorf("kv_cache_server.key_prefix: %q is shared by %d seats and binding %s declares no stack generation — set kv_dtype and tensor_parallel on every binding that shares a namespace, or give this seat its own key_prefix (one key_prefix per stack generation)",
					p, len(group), k.seatLabel())
			}
		}
		want := group[0].generation()
		for _, k := range group[1:] {
			if k.generation() != want {
				return fmt.Errorf("kv_cache_server.key_prefix: %q is shared by seats of DIFFERENT stack generations (%s is %s, %s is %s) — one key_prefix per generation; a seat reading another layout's pages fails every read with \"value size exceeds buffer capacity\" and serves nothing while reporting success",
					p, group[0].seatLabel(), want, k.seatLabel(), k.generation())
			}
		}
	}
	return nil
}
