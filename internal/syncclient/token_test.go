package syncclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenStoreRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "credentials.json")
	tok := Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}

	if err := SaveToken(path, tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v, want 0600", info.Mode().Perm())
	}

	got, err := LoadToken(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" || !got.Expiry.Equal(tok.Expiry) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.Valid() {
		t.Fatal("token should be valid (expiry in the future)")
	}

	if err := ClearToken(path); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := LoadToken(path); err == nil {
		t.Fatal("expected error loading cleared token")
	}
}

func TestSaveTokenTightensPermsOnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	tok := Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}
	if err := SaveToken(path, tok); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("overwrite perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestTokenValidExpired(t *testing.T) {
	if (Token{AccessToken: "x", Expiry: time.Now().Add(-time.Minute)}).Valid() {
		t.Fatal("expired token must not be Valid")
	}
	if (Token{Expiry: time.Now().Add(time.Hour)}).Valid() {
		t.Fatal("token with empty AccessToken must not be Valid")
	}
}

// S8b: a token expiring within the 60s skew window must be treated as already
// expired — it can expire in-flight, surfacing as a bare 401 with no hint that
// a refresh would have fixed it.
func TestTokenValidExpirySkew(t *testing.T) {
	if (Token{AccessToken: "x", Expiry: time.Now().Add(30 * time.Second)}).Valid() {
		t.Fatal("a token inside the 60s expiry-skew window must not be Valid")
	}
	// Exactly-at-the-boundary: Valid uses strict Before, so a token with
	// precisely 60s of life must already count as expired.
	if (Token{AccessToken: "x", Expiry: time.Now().Add(expirySkew)}).Valid() {
		t.Fatal("a token expiring exactly at the skew boundary must not be Valid")
	}
	if !(Token{AccessToken: "x", Expiry: time.Now().Add(2 * time.Minute)}).Valid() {
		t.Fatal("a token with more than 60s of life left must be Valid")
	}
}
