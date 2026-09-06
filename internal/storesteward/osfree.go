package storesteward

import "github.com/dmmdea/offload-harness/internal/volumes"

// osFree is the production free-space source: the bytes an unprivileged
// writer may still put on the volume holding path.
func osFree(path string) (uint64, error) { return volumes.FreeBytes(path) }
