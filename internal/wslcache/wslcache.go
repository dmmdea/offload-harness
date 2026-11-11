// Package wslcache frees the page cache of a Windows host's WSL 2 VM for a GPU lease that is waiting for host RAM
// (config gpu_hostram_wsl_cache_drop, off by default; the lease side is internal/gpulease/relief.go).
//
// Why it exists. WSL 2 runs every distro in one VM, and the host charges that VM's memory to one process (vmmemWSL).
// Inside the VM the page cache (files a distro has read) is clean and can be dropped at any moment; to the host it is
// committed memory that no Windows reclaim can touch. WSL's own reclaim (.wslconfig autoMemoryReclaim) runs on its own
// schedule: on the reference box it left 25 GiB of guest page cache in place for over 40 minutes while a render's lease
// waited on host RAM, and one manual drop moved the host's commit from 127.0 to 103.2 GiB and let the render start.
//
// What it does, and only that: read /proc/meminfo through a distro that is ALREADY running (it never starts one, because
// a distro start adds cache of its own), count the cache a drop can free (Cached less Shmem: tmpfs and shared memory sit
// in Cached and no drop frees them), and only when that covers the shortfall the lease reported, run `sync` and write 1
// to /proc/sys/vm/drop_caches as root. drop_caches=1 frees clean page cache only: sync writes dirty pages first, and no
// memory a process holds is touched. All distros share one kernel, so one drop covers the whole VM. Nothing from a lease
// or a request reaches the command line: the arguments are fixed, and the distro name is one wsl.exe itself listed.
package wslcache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// MinGiB is the least droppable cache worth a drop: below it the cold reads a drop costs the distros buy nothing.
const MinGiB = 1.0

// runner is the three things the relief asks of wsl.exe; tests give it a fake.
type runner interface {
	// running lists the distros that are running now. An error, or none, means there is nothing to read.
	running(ctx context.Context) ([]string, error)
	// meminfo returns /proc/meminfo as read inside the named distro.
	meminfo(ctx context.Context, distro string) ([]byte, error)
	// drop runs sync, then writes 1 to /proc/sys/vm/drop_caches as root, inside the named distro.
	drop(ctx context.Context, distro string) error
}

// errUnsupported is what the runner answers on a platform with no WSL.
var errUnsupported = errors.New("no WSL on this platform")

// Relieve asks the host's WSL 2 VM to drop its page cache when the cache a drop can free is at least shortGiB (and at
// least MinGiB). It returns a one-line report when it dropped, "" when it did nothing (not Windows, no WSL, no running
// distro, a process under an account that sees none, or too little cache to cover the shortfall), and an error only when
// a running distro could not be read or a drop was tried and failed.
func Relieve(ctx context.Context, shortGiB float64) (string, error) {
	return relieve(ctx, system, shortGiB)
}

func relieve(ctx context.Context, r runner, shortGiB float64) (string, error) {
	distros, err := r.running(ctx)
	if err != nil {
		return "", nil // no WSL, or this account has no distros: nothing here to free
	}
	distro := pick(distros)
	if distro == "" {
		return "", nil
	}
	info, err := r.meminfo(ctx, distro)
	if err != nil {
		return "", fmt.Errorf("read the WSL VM's memory: %w", err)
	}
	before, ok := Droppable(info)
	if !ok {
		return "", errors.New("read the WSL VM's memory: /proc/meminfo has no Cached line")
	}
	if before < max(MinGiB, shortGiB) {
		return "", nil
	}
	if err := r.drop(ctx, distro); err != nil {
		return "", fmt.Errorf("drop the WSL VM's page cache (%.1f GiB droppable): %w", before, err)
	}
	after := "unread"
	if info, err := r.meminfo(ctx, distro); err == nil {
		if a, ok := Droppable(info); ok {
			after = fmt.Sprintf("%.1f GiB", a)
		}
	}
	return fmt.Sprintf("asked the WSL VM to drop its page cache for a lease %.1f GiB short of host RAM: droppable cache %.1f GiB before, %s after",
		shortGiB, before, after), nil
}

// pick is the distro to read and drop through: the first running one that is not Docker Desktop's own (its distros are
// managed by Docker and are not ours to run commands in). Any running distro serves, because all share the VM's kernel.
func pick(distros []string) string {
	for _, d := range distros {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(strings.ToLower(d), "docker-desktop") {
			continue
		}
		return d
	}
	return ""
}

// Droppable is the page cache a drop_caches=1 can free, in GiB, from a /proc/meminfo: Cached less Shmem. ok is false when
// the text has no Cached line.
func Droppable(meminfo []byte) (gib float64, ok bool) {
	var cached, shmem float64
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		kib, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		switch name {
		case "Cached":
			cached, ok = kib, true
		case "Shmem":
			shmem = kib
		}
	}
	if !ok {
		return 0, false
	}
	return max(cached-shmem, 0) / (1 << 20), true
}

// decodeList reads wsl.exe's distro list: UTF-8 when WSL_UTF8=1 is honoured, UTF-16LE (with NULs between the ASCII bytes)
// from a wsl.exe that predates it.
func decodeList(out []byte) []string {
	if bytes.IndexByte(out, 0) >= 0 {
		out = bytes.TrimPrefix(out, []byte{0xFF, 0xFE})
		u := make([]uint16, 0, len(out)/2)
		for i := 0; i+1 < len(out); i += 2 {
			u = append(u, uint16(out[i])|uint16(out[i+1])<<8)
		}
		out = []byte(string(utf16.Decode(u)))
	}
	var names []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if line = strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF")); line != "" {
			names = append(names, line)
		}
	}
	return names
}
