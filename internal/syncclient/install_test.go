package syncclient

import (
	"os"
	"path/filepath"
	"testing"
)

// The install id names one machine in every deployment report it files, so the
// two facts worth gating are that it SURVIVES (a regenerated id splits one
// machine's history into two the server counts separately) and that a file
// which will not read is never repaired by overwriting it, which is the same
// failure wearing a different hat.

func TestEnsureInstallIDCreatesOnceAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".config", "orbeat", "install.json")

	first, err := EnsureInstallID(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !installIDRe.MatchString(first) {
		t.Fatalf("generated id %q is not a uuid, and the server 400s anything else", first)
	}

	second, err := EnsureInstallID(path)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second != first {
		t.Fatalf("a second call generated a new id (%q then %q): one machine would report as two", first, second)
	}

	loaded, err := LoadInstallID(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded != first {
		t.Fatalf("LoadInstallID = %q, want the created %q", loaded, first)
	}
}

// Two calls in a row must not both mint an id. A generator that ignored the
// file would pass a "the id is a uuid" test on every run.
func TestEnsureInstallIDIsUniquePerMachine(t *testing.T) {
	a, err := EnsureInstallID(filepath.Join(t.TempDir(), "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureInstallID(filepath.Join(t.TempDir(), "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two independent machines got the same install id %q", a)
	}
}

func TestEnsureInstallIDWritesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".config", "orbeat", "install.json")
	if _, err := EnsureInstallID(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("install.json mode = %o, want 600", perm)
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir mode = %o, want 700 (writeFileAtomic's MkdirAll uses 0755, so the dir must be pre-created)", perm)
	}
}

// THE ONE THAT MATTERS: a file that will not parse is an error, and the bytes
// on disk are left exactly as they were. Regenerating would restore reporting
// at the cost of a second identity for the same machine, and nothing would
// ever say so.
func TestEnsureInstallIDRefusesToOverwriteAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	const corrupt = `{"installId": "abc`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := EnsureInstallID(path)
	if err == nil {
		t.Fatalf("an unparseable install.json produced id %q instead of an error", id)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != corrupt {
		t.Fatalf("install.json was rewritten (%q): a new id starts a second install identity", after)
	}
}

func TestLoadInstallIDAbsentIsNotAnError(t *testing.T) {
	id, err := LoadInstallID(filepath.Join(t.TempDir(), "install.json"))
	if err != nil {
		t.Fatalf("an absent install.json must not be an error (it is the normal state before the first report): %v", err)
	}
	if id != "" {
		t.Fatalf("LoadInstallID = %q on an absent file, want \"\"", id)
	}
}

// A file that parses but carries something the server would reject is an
// error, not an id: the report would 400 on every run and the client would
// have no idea why.
func TestLoadInstallIDRejectsANonUUID(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      `{"installId": ""}`,
		"not a uuid": `{"installId": "laptop-1"}`,
		"no key":     `{}`,
		"truncated":  `{"installId": "11111111-1111-4111-8111"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "install.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if id, err := LoadInstallID(path); err == nil {
				t.Fatalf("%s produced id %q instead of an error", body, id)
			}
		})
	}
}
