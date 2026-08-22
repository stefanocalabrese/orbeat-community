package gateway

import (
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

// TestGatewayImplementationTracksVersion proves the MCP Implementation the
// gateway advertises to both its server-facing leg (server.go's mcpServer)
// and its upstream-facing leg (broker.go's connectUpstream client) is never
// independently hardcoded — it must track internal/version.Version, the
// single build-time product version every orbeat binary reports
// (fable-audit §7 #15: this constant had drifted to a stale hardcoded
// "0.3.0" while the product shipped v1.25.0, invisible to every prior test
// because none of them asserted it against anything other than itself).
//
// The comparison value is chosen per-run and distinctive from both the
// package's "dev" default and any past literal (the old "0.3.0"), so a
// revert to EITHER a hardcoded literal or a copy frozen at package-init time
// cannot coincidentally pass.
func TestGatewayImplementationTracksVersion(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	version.Version = "gatewaytest-9f3a1c"
	got := gatewayImplementation()
	if got.Version != version.Version {
		t.Fatalf("gatewayImplementation().Version = %q, want %q (must track internal/version.Version live, not a hardcoded literal or a frozen copy)",
			got.Version, version.Version)
	}
	if got.Name != "orbeat-gateway" {
		t.Fatalf("gatewayImplementation().Name = %q, want %q", got.Name, "orbeat-gateway")
	}
}
