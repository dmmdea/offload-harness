//go:build !windows

package wslcache

import "context"

// Off Windows there is no WSL VM: a Linux host's own page cache already counts as available memory.
var system runner = none{}

type none struct{}

func (none) running(context.Context) ([]string, error)       { return nil, errUnsupported }
func (none) meminfo(context.Context, string) ([]byte, error) { return nil, errUnsupported }
func (none) drop(context.Context, string) error              { return errUnsupported }
