package brewformula

import (
	"strings"
	"testing"
)

// sha is a 64-char lowercase hex string, distinguishable per call site by its
// filler byte, so a wrong-platform mixup shows up as a wrong-looking hash
// instead of every test hash being identical.
func sha(fill byte) string {
	return strings.Repeat(string(fill), 64)
}

func sampleChecksumsText() string {
	return sha('a') + "  orbeat-sync-linux-amd64\n" +
		sha('b') + "  orbeat-sync-linux-arm64\n" +
		sha('c') + "  orbeat-sync-darwin-arm64\n" +
		sha('d') + "  orbeat-sync-linux-amd64.sigstore.json\n" +
		sha('e') + "  orbeat-sync-linux-arm64.sigstore.json\n" +
		sha('f') + "  orbeat-sync-darwin-arm64.sigstore.json\n" +
		sha('1') + "  orbeat-sbom.spdx.json\n"
}

// TestParseChecksumsExtractsCoveredAssetsOnly asserts both that the three
// covered assets are parsed with the RIGHT checksum attached to the RIGHT
// platform (a mutant that scrambled the map keys would still pass a mere
// "has 3 entries" check) and that the sbom/.sigstore.json lines are ignored
// rather than rejected.
func TestParseChecksumsExtractsCoveredAssetsOnly(t *testing.T) {
	got, err := ParseChecksums([]byte(sampleChecksumsText()))
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	want := map[Platform]string{
		{GOOS: "linux", GOARCH: "amd64"}:  sha('a'),
		{GOOS: "linux", GOARCH: "arm64"}:  sha('b'),
		{GOOS: "darwin", GOARCH: "arm64"}: sha('c'),
	}
	if len(got) != len(want) {
		t.Fatalf("ParseChecksums returned %d entries (want %d, i.e. sbom/.sigstore.json lines "+
			"must be ignored, not counted): %+v", len(got), len(want), got)
	}
	for p, wantSum := range want {
		gotSum, ok := got[p]
		if !ok {
			t.Errorf("ParseChecksums: missing entry for %+v", p)
			continue
		}
		if gotSum != wantSum {
			t.Errorf("ParseChecksums[%+v] = %q, want %q", p, gotSum, wantSum)
		}
	}
}

// TestParseChecksumsTrailingNewlineTolerated asserts a file with no trailing
// newline parses identically to one that has exactly one.
func TestParseChecksumsTrailingNewlineTolerated(t *testing.T) {
	withNL := sampleChecksumsText()
	withoutNL := strings.TrimSuffix(withNL, "\n")

	got1, err := ParseChecksums([]byte(withNL))
	if err != nil {
		t.Fatalf("ParseChecksums(with trailing newline): %v", err)
	}
	got2, err := ParseChecksums([]byte(withoutNL))
	if err != nil {
		t.Fatalf("ParseChecksums(without trailing newline): %v", err)
	}
	if len(got1) != len(got2) {
		t.Fatalf("trailing newline changed the parse result: %+v vs %+v", got1, got2)
	}
	for p, s := range got1 {
		if got2[p] != s {
			t.Errorf("platform %+v: with-newline=%q without-newline=%q", p, s, got2[p])
		}
	}
}

// TestParseChecksumsRejectsMalformedLine covers the shapes ParseChecksums
// must reject rather than silently drop. Each case pins a specific reason a
// line can fail to be "<64 lowercase hex><two spaces><filename>":
// wrong hash length, uppercase hex (sha256sum never emits uppercase, and
// accepting it would let a hand-edited file past validation silently),
// single space instead of two, and a hash that isn't hex at all.
func TestParseChecksumsRejectsMalformedLine(t *testing.T) {
	cases := map[string]string{
		"short hash":          sha('a')[:63] + "  orbeat-sync-linux-amd64\n",
		"long hash":           sha('a') + "a  orbeat-sync-linux-amd64\n",
		"uppercase hash":      strings.ToUpper(sha('a')) + "  orbeat-sync-linux-amd64\n",
		"single space":        sha('a') + " orbeat-sync-linux-amd64\n",
		"non-hex hash":        strings.Repeat("g", 64) + "  orbeat-sync-linux-amd64\n",
		"empty file":          "",
		"blank line in body":  sha('a') + "  orbeat-sync-linux-amd64\n\n" + sha('b') + "  orbeat-sync-linux-arm64\n",
		"no filename at all":  sha('a') + "  \n",
		"missing hash column": "orbeat-sync-linux-amd64\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseChecksums([]byte(text)); err == nil {
				t.Fatalf("ParseChecksums(%q) succeeded, want an error for a malformed line", text)
			}
		})
	}
}

// TestParseChecksumsRejectsDuplicateAsset asserts a second line for a
// covered asset is an error, not a silent last-wins. The package doc
// comment already claims this cannot happen; before this fix, a duplicate
// with a DIFFERENT hash for the same platform silently picked whichever
// line came last, which is exactly the silent-drop failure mode the rest of
// this function exists to refuse.
func TestParseChecksumsRejectsDuplicateAsset(t *testing.T) {
	text := sha('a') + "  orbeat-sync-linux-amd64\n" +
		sha('f') + "  orbeat-sync-linux-amd64\n" + // duplicate, different hash
		sha('b') + "  orbeat-sync-linux-arm64\n" +
		sha('c') + "  orbeat-sync-darwin-arm64\n"
	if _, err := ParseChecksums([]byte(text)); err == nil {
		t.Fatal("ParseChecksums accepted a duplicate covered-asset line, want an error")
	}
}

// TestParseChecksumsRejectsCRLFLineEndings asserts a CRLF file fails with a
// message naming the real cause. Before this fix, every line's filename
// capture retained a trailing "\r" (splitting only on "\n" leaves it, and
// the checksumLine regex's "." matches "\r"), so every line missed its
// byAsset lookup and was silently treated as "an asset this formula does
// not cover" - ParseChecksums returned an EMPTY map with a nil error, and
// the real cause only surfaced two layers away as Render's generic "missing
// checksum for ..." for all three platforms at once.
func TestParseChecksumsRejectsCRLFLineEndings(t *testing.T) {
	text := strings.ReplaceAll(sampleChecksumsText(), "\n", "\r\n")
	_, err := ParseChecksums([]byte(text))
	if err == nil {
		t.Fatal("ParseChecksums accepted CRLF line endings, want an error naming the real cause")
	}
	if !strings.Contains(err.Error(), "CRLF") {
		t.Errorf("ParseChecksums error %q does not name CRLF as the cause", err.Error())
	}
}
