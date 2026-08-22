//go:build unix

package syncclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked reports that another orbeat-sync process holds the run lock.
var ErrLocked = errors.New("another orbeat-sync is running")

// AcquireLock takes an exclusive advisory lock on <configDir>/.lock, serializing
// whole runs (sync / project remove / connect) across processes. Without it,
// concurrent runs are last-write-wins on the manifest (silently discarding the
// other run's ledger preservation) and writeFileAtomic's stale-temp cleanup
// deletes the other run's in-flight temp files.
//
// Contention returns ErrLocked immediately (LOCK_NB — never blocks; the caller
// reports "another orbeat-sync is running" and exits 1, retryable). The returned
// release func unlocks and closes the file. The lock is tied to the open file
// description, so a crashed process releases it automatically — no stale-lock
// recovery needed. The .lock file itself is deliberately never removed:
// unlinking a lock file while another process may be opening it lets two
// processes lock two different inodes at the same path.
func AcquireLock(configDir string) (release func(), err error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("lock: mkdir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(configDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock: flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
