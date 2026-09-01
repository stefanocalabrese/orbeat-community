package syncclient

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// installFile is the on-disk shape of ~/.config/orbeat/install.json: one
// random opaque uuid naming THIS machine's copy of the client, so a deployment
// report can be grouped per machine rather than per person (spec sec 4.1: one
// developer with a laptop and a desktop otherwise overwrites herself, and the
// desk that is actually behind is the one the answer hides).
//
// It is not the hostname. A hostname leaks a name the operator did not need,
// collides across a fleet of localhosts, and changes on a rename, which
// produces a phantom second machine with none of the compensating benefit.
// os.Hostname is called nowhere in this client, and this file does not
// introduce the first call.
//
// It is not stored under ~/.claude either: that tree is the reconciler's
// managed root, swept by Reconcile's removal loop. ~/.config/orbeat is the
// client's own state, alongside credentials.json, projects.json and
// connect.json, and nothing sweeps it.
type installFile struct {
	InstallID string `json:"installId"`
}

// installIDRe mirrors the server's uuidRe (internal/api/admin_audit.go): the
// report endpoint 400s an installId that is not a uuid, so an id this client
// would refuse to send and an id the server would refuse to accept are kept
// the same set. Both cases are accepted for the same reason the server accepts
// both: Postgres' uuid text form is case-insensitive, and a hand-edited file
// carrying an upper-case id names the same machine.
var installIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DefaultInstallPath is ~/.config/orbeat/install.json, a sibling of
// credentials.json / projects.json / connect.json in the same 0700 state dir.
func DefaultInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("install path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "install.json"), nil
}

// LoadInstallID reads the install identity.
//
// An ABSENT file is ("", nil), not an error: the id is created lazily on the
// first report (spec sec 4.2), so a client that has never reported, or faces a
// server that does not record deployments, legitimately has no file. Every
// other failure IS an error, including a file that parses but carries no
// usable id: the caller must never treat "cannot read it" as "there is none",
// because the only repair for "there is none" is to write a new one, and a new
// one forks this machine's history into two identities the server will count
// twice.
func LoadInstallID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("load install id: %w", err)
	}
	var f installFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("load install id: parse %s: %w", path, err)
	}
	if !installIDRe.MatchString(f.InstallID) {
		return "", fmt.Errorf("load install id: %s carries %q, which is not a uuid", path, f.InstallID)
	}
	return f.InstallID, nil
}

// EnsureInstallID returns this machine's install id, creating it on first use.
//
// A read failure is returned rather than repaired. Regenerating over a file
// that exists but will not parse is the one behaviour this function must never
// have: the old id keeps whatever rows the server already holds for it, a new
// id starts a second set, and the two are indistinguishable from two machines
// for as long as retention keeps them. `orbeat-sync doctor` reports the
// unparseable file as a PROBLEM for the same reason.
//
// The parent directory is created 0700 first: writeFileAtomic's own MkdirAll
// uses 0755 and never re-modes an existing directory, so pre-creating it is
// what keeps this file's directory as private as the token store's.
//
// Not safe against a concurrent writer, and it does not need to be: every
// mutating command holds the exclusive run lock (AcquireLock), so two syncs
// cannot race to create this file.
func EnsureInstallID(path string) (string, error) {
	id, err := LoadInstallID(path)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	id, err = newInstallID()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(installFile{InstallID: id}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("save install id: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("save install id: mkdir: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("save install id: %w", err)
	}
	return id, nil
}

// newInstallID returns a random RFC 4122 version-4 uuid (crypto/rand, never
// math/rand: an id a colleague can predict is an id a colleague can report
// under).
//
// A rand.Read failure is returned, not defaulted: internal/logging's request
// id can fall back to a constant because a duplicate request id costs a
// confusing log line, while a constant install id would merge every machine in
// the fleet into one row set.
func newInstallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("new install id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
