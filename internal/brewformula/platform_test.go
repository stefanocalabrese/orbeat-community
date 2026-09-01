package brewformula

import "testing"

// TestPlatforms pins the exact release-target set and its order. This is the
// literal ground truth .github/workflows/release_platforms_test.go's
// TestReleaseMatrixMatchesBrewformulaPlatforms compares against
// .github/workflows/release.yml's "cli" build matrix, so any change here is a
// change to what orbeat-sync ships for, not just a test fixture.
func TestPlatforms(t *testing.T) {
	want := []Platform{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "arm64"},
	}
	got := Platforms()
	if len(got) != len(want) {
		t.Fatalf("Platforms() = %+v, want %+v", got, want)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("Platforms()[%d] = %+v, want %+v", i, got[i], p)
		}
	}
}

// TestPlatformsNoDarwinAmd64 pins the deliberate omission: capo decided
// 2026-08-29 that Intel macOS is unsupported, so a regression that adds
// darwin/amd64 back (e.g. someone "completing" what looks like a matrix)
// must fail loudly rather than silently widen what the formula promises.
func TestPlatformsNoDarwinAmd64(t *testing.T) {
	for _, p := range Platforms() {
		if p.GOOS == "darwin" && p.GOARCH == "amd64" {
			t.Fatalf("Platforms() includes darwin/amd64, which is deliberately unsupported")
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		p    Platform
		want string
	}{
		{Platform{GOOS: "linux", GOARCH: "amd64"}, "orbeat-sync-linux-amd64"},
		{Platform{GOOS: "linux", GOARCH: "arm64"}, "orbeat-sync-linux-arm64"},
		{Platform{GOOS: "darwin", GOARCH: "arm64"}, "orbeat-sync-darwin-arm64"},
	}
	for _, c := range cases {
		if got := AssetName(c.p); got != c.want {
			t.Errorf("AssetName(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}
