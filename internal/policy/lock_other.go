//go:build !unix

package policy

import (
	"sync"
	"time"
)

var lockMu sync.Mutex

// lockFile serialises writers within this process only (Pilothouse ships
// for macOS and Linux; this keeps other builds compiling).
func lockFile(path string, timeout time.Duration) (func(), error) {
	lockMu.Lock()
	return lockMu.Unlock, nil
}
