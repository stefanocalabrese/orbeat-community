package gateway

import (
	"testing"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

func TestPrincipalRoundTripsThroughTokenInfo(t *testing.T) {
	p := auth.Principal{Subject: "kc-1", Email: "a@x.io", Roles: []string{"orbeat-user", "orbeat-admin"}}
	ti := principalToTokenInfo(p)
	if ti.UserID != "kc-1" {
		t.Fatalf("UserID = %q", ti.UserID)
	}
	got, ok := principalFromTokenInfo(ti)
	if !ok {
		t.Fatal("principalFromTokenInfo: not found")
	}
	if got.Subject != p.Subject || got.Email != p.Email || len(got.Roles) != 2 || got.Roles[0] != "orbeat-user" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestPrincipalFromNilOrEmpty(t *testing.T) {
	if _, ok := principalFromTokenInfo(nil); ok {
		t.Fatal("nil TokenInfo should not yield a principal")
	}
}

func TestPrincipalFromEmptyExtra(t *testing.T) {
	if _, ok := principalFromTokenInfo(&mcpauth.TokenInfo{}); ok {
		t.Fatal("empty Extra should not yield a principal")
	}
}
