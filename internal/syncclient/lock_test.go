package syncclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// S3: while one run holds the lock, a second acquire must fail with ErrLocked;
// after release, acquisition must succeed again. (flock is per open file
// description, so two opens in the same process contend just like two
// processes do — this exercises the real mechanism.)
func TestAcquireLockContention(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire while held: err = %v, want ErrLocked", err)
	}

	release()
	release2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

// AcquireLock must create a missing config dir (first run on a fresh machine)
// and keep the lock file private.
func TestAcquireLockCreatesConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "orbeat")

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("acquire with missing dir: %v", err)
	}
	defer release()

	info, err := os.Stat(filepath.Join(dir, ".lock"))
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lock file perms = %v, want 0600", perm)
	}
}
