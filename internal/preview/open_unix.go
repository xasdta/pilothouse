//go:build unix

package preview

import (
	"os"
	"syscall"
)

// openNoBlock opens p for reading without blocking if it was swapped for
// a FIFO after the caller's stat: a preview must never hang a hook.
func openNoBlock(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
