package api

import (
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
)

// TestNewWiresLimitsFromTheEditionExtensionPoint pins the one statement about
// Server.limits that is true in BOTH editions: New reads the extension point
// (limits.ee.go / limits.community.go) rather than deciding for itself. The
// Enterprise VALUE assertions, communityLimits() == editionLimits{} and New's
// default being that zero value, moved to limits.ee_test.go, which the
// generator drops, because in a Community tree both are false by design
// (limits.community.go returns {Servers: 10, Roles: 1}).
//
// Stated honestly, the same caveat as
// TestNewWiresAutoApproveFromTheEditionExtensionPoint: in THIS build both
// sides are the zero value, so a New that hard-coded editionLimits{} would
// still pass here and only limits.ee_test.go's value pin would catch it. In a
// generated Community tree it is the decisive wiring check, and the only one
// that can live in a file surviving generation.
func TestNewWiresLimitsFromTheEditionExtensionPoint(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	if srv.limits != communityLimits() {
		t.Fatalf("New()'s limits = %+v, want communityLimits()'s %+v, New must read the "+
			"edition extension point, not decide for itself", srv.limits, communityLimits())
	}
}

// TestSetContactEmailIgnoresEmpty pins New's default (authz.DefaultContactEmail)
// and SetContactEmail's nil/empty-ignore contract. Edition-agnostic: the
// contact address is the same in both builds.
func TestSetContactEmailIgnoresEmpty(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	if srv.contactEmail != authz.DefaultContactEmail {
		t.Fatalf("default contactEmail = %q, want %q", srv.contactEmail, authz.DefaultContactEmail)
	}
	srv.SetContactEmail("")
	if srv.contactEmail != authz.DefaultContactEmail {
		t.Fatal("SetContactEmail(\"\") must not blank the default")
	}
	srv.SetContactEmail("ops@example.com")
	if srv.contactEmail != "ops@example.com" {
		t.Fatalf("SetContactEmail did not override: %q", srv.contactEmail)
	}
}
