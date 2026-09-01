package brewformula

import "fmt"

// Platform is one release target: a GOOS/GOARCH pair. It is comparable, so it
// works directly as a map key for per-platform checksums.
type Platform struct {
	GOOS   string
	GOARCH string
}

// Platforms returns the release targets orbeat-sync ships binaries for, in
// the same order as the "cli" matrix in .github/workflows/release.yml. This
// is the single source of truth for that set inside this package: checksum
// validation and checksums.txt parsing derive their platform list from this
// call rather than repeating the (goos, goarch) literals, so a later drift
// gate comparing this list against release.yml's matrix has exactly one
// place in this package to compare against.
//
// Render's own template is the one place that does NOT loop over this list:
// it fills three named platform-specific fields (one macOS-arm slot, two
// Linux slots), not a generic list of N. render.go's requirePlatform and
// verifyCoverage exist specifically because of that gap - they check, in
// both directions, that this hardcoded shape and Platforms() still agree,
// so a change to this function alone cannot silently drift from what the
// template renders.
//
// There is deliberately no darwin/amd64: capo decided 2026-08-29 that Intel
// macOS is out of scope (Apple's macOS Tahoe 26 is the last Intel-compatible
// release, and Homebrew itself moves Intel to Tier 3 in September 2026 and
// stops running CI on it in September 2027).
func Platforms() []Platform {
	return []Platform{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "arm64"},
	}
}

// AssetName returns the GitHub Release asset name for p. The format matches
// the real names confirmed against the v1.24.0 release: orbeat-sync-<goos>-<goarch>.
func AssetName(p Platform) string {
	return fmt.Sprintf("orbeat-sync-%s-%s", p.GOOS, p.GOARCH)
}
