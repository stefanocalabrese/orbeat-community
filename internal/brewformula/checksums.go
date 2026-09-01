package brewformula

import (
	"fmt"
	"regexp"
	"strings"
)

// checksumLine matches one line of `sha256sum`-style output: 64 lowercase hex
// digits, exactly two spaces (sha256sum's text-mode separator: one space plus
// a mode-character space, never an asterisk here since these are always
// plain files), then a non-empty filename.
var checksumLine = regexp.MustCompile(`^([0-9a-f]{64})  (.+)$`)

// ParseChecksums parses a checksums.txt in `sha256sum` output format into a
// per-platform checksum map covering exactly the platforms in Platforms().
// A single trailing newline is tolerated. Lines naming an asset Platforms()
// does not cover (the SBOM, the .sigstore.json cosign bundles) are ignored
// rather than rejected, since a release's checksums.txt covers more files
// than the formula does. Everything else that could silently drop or
// silently pick a checksum is a hard error instead: a line that is not
// well-formed "<64 lowercase hex><two spaces><filename>" (including a CRLF
// line ending, which would otherwise leave every filename capture with a
// trailing "\r" and silently fail every lookup below), and a second line
// for an asset already seen, which would otherwise silently keep whichever
// line came last.
func ParseChecksums(data []byte) (map[Platform]string, error) {
	text := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(text, "\n")

	byAsset := make(map[string]Platform, len(Platforms()))
	for _, p := range Platforms() {
		byAsset[AssetName(p)] = p
	}

	out := make(map[Platform]string, len(Platforms()))
	for i, line := range lines {
		m := checksumLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf(
				"brewformula: checksums line %d is not <64 lowercase hex><two spaces><filename>: %q",
				i+1, line)
		}
		sum, filename := m[1], m[2]
		// A Windows line ending leaves a trailing "\r" attached to the
		// filename capture (splitting only on "\n" does not strip it, and
		// the checksumLine regex's "." matches "\r"). Left unguarded, that
		// "\r" makes the filename fail every byAsset lookup below, so a
		// CRLF file would silently skip EVERY line as "uncovered" and
		// Render would later report a plain "missing checksum" - true, but
		// not the actual cause. Catch it here instead, at the line that is
		// actually malformed.
		if strings.HasSuffix(filename, "\r") {
			return nil, fmt.Errorf(
				"brewformula: checksums line %d has a Windows line ending (CRLF); "+
					"expected LF-only sha256sum output: %q", i+1, line)
		}
		p, ok := byAsset[filename]
		if !ok {
			continue // an asset this formula does not cover (sbom, .sigstore.json, ...)
		}
		if _, dup := out[p]; dup {
			return nil, fmt.Errorf(
				"brewformula: checksums line %d is a duplicate entry for %s (already seen earlier "+
					"in this file)", i+1, filename)
		}
		out[p] = sum
	}
	return out, nil
}
