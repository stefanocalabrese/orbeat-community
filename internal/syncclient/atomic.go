package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a same-directory temp file that is
// fsync'd and then renamed over the target: a crash can never leave a
// truncated/half-written file — a reader sees the old content or the new,
// never a torn one. It also self-heals temp files orphaned by a prior crash.
// That stale-temp cleanup is safe ONLY because every mutating run (sync,
// project add/remove, connect) holds the exclusive run lock (see AcquireLock):
// without it, this glob-and-delete would remove a concurrent run's in-flight
// temp, making its rename fail with ENOENT — a failure orbeat would
// manufacture itself.
//
// Missing parent directories are created 0755; a caller writing into a
// private directory must pre-create it with tighter perms first (MkdirAll
// never re-modes an existing dir).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomic write: mkdir: %w", err)
	}
	if stale, _ := filepath.Glob(filepath.Join(dir, base+".tmp-*")); stale != nil {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomic write: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; a no-op after success.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: write: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil { // CreateTemp makes 0600
		_ = tmp.Close()
		return fmt.Errorf("atomic write: chmod: %w", err)
	}
	if err := tmp.Sync(); err != nil { // durability before the rename makes it visible
		_ = tmp.Close()
		return fmt.Errorf("atomic write: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic write: rename: %w", err)
	}
	return nil
}
