package version

import "testing"

// TestDefaultIsDev pins the zero-override default. It is deliberately "dev",
// not empty or a fake semver: an unset -ldflags must be visible as obviously
// non-release, not silently masquerade as a version number.
func TestDefaultIsDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want the zero-override default %q (this test must run before "+
			"any -ldflags -X override — if it fails under a normal `go test`, the default itself "+
			"changed)", Version, "dev")
	}
}
