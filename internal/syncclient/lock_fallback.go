//go:build !unix

package syncclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked reports that another orbeat-sync process holds the run lock.
var ErrLocked = errors.New("another orbeat-sync is running")

// AcquireLock is the non-unix fallback: an O_CREATE|O_EXCL sentinel file at
// <configDir>/.lock. Semantics are deliberately WEAKER than the unix flock
// version: the lock is not tied to the process lifetime, so a run that crashes
// (or is killed) before release leaves a stale .lock behind, and every
// subsequent run reports ErrLocked until the file is removed manually. This is
// accepted for the fallback — mutual exclusion is preserved (the failure mode
// is over-locking, never two concurrent runs), and orbeat-sync's supported
// platforms are unix. See lock.go for the full rationale for the lock itself.
func AcquireLock(configDir string) (release func(), err error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("lock: mkdir: %w", err)
	}
	path := filepath.Join(configDir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock: open: %w", err)
	}
	_ = f.Close()
	// Removing the sentinel IS the release; unlike flock there is no
	// kernel-held state to drop.
	return func() { _ = os.Remove(path) }, nil
}
