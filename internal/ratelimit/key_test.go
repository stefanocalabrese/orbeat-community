package ratelimit

import (
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

func TestKeyForSeparatesClients(t *testing.T) {
	a := KeyFor(auth.Principal{Subject: "u1", ClientID: "claude"})
	b := KeyFor(auth.Principal{Subject: "u1", ClientID: "codex"})
	if a == b {
		t.Fatalf("same key %q for two clients of one subject", a)
	}
}

// The fallback must degrade to subject-keying, NEVER to an empty key: an empty
// key merges every caller into one bucket, a global limit wearing a
// per-principal costume.
func TestKeyForFallsBackToSubject(t *testing.T) {
	k := KeyFor(auth.Principal{Subject: "u1"})
	if k == "" {
		t.Fatal("empty key for a principal with no client id")
	}
	if k == KeyFor(auth.Principal{Subject: "u2"}) {
		t.Fatal("two different subjects share a key in the fallback path")
	}
}
