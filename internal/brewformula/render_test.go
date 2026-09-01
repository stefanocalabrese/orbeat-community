package brewformula

import (
	"fmt"
	"strings"
	"testing"
)

func validChecksums() map[Platform]string {
	return map[Platform]string{
		{GOOS: "linux", GOARCH: "amd64"}:  sha('a'),
		{GOOS: "linux", GOARCH: "arm64"}:  sha('b'),
		{GOOS: "darwin", GOARCH: "arm64"}: sha('c'),
	}
}

// TestRenderVersionForms covers the two accepted spellings and the ones that
// must be rejected. "1.26" and "1.26.0.1" are real near-misses (a two- or
// four-component version), not just noise, since a generator that accepted
// them would build a URL pointing at a release that does not exist.
func TestRenderVersionForms(t *testing.T) {
	cases := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"v-prefixed", "v1.26.0", false},
		{"bare", "1.26.0", false},
		{"empty", "", true},
		{"double v", "vv1.26.0", true},
		{"uppercase V", "V1.26.0", true},
		{"two components", "1.26", true},
		{"four components", "1.26.0.1", true},
		{"prerelease suffix", "1.26.0-rc1", true},
		{"leading/trailing space", " 1.26.0 ", true},
		{"non-numeric", "v1.x.0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := Render(Options{Version: c.version, Checksums: validChecksums()})
			if (err != nil) != c.wantErr {
				t.Fatalf("Render(Version=%q) err=%v, wantErr=%v", c.version, err, c.wantErr)
			}
			if err != nil {
				return
			}
			// Two different things are being pinned here, and they are not
			// in tension: the URL PATH must reconstruct "v1.26.0" (a GitHub
			// release tag always carries the "v", so the download URL has
			// to include it), while no quoted "v1.26.0" VERSION LITERAL may
			// appear anywhere - the formula has no explicit `version`
			// statement at all (see the package doc comment for why: an
			// explicit one fails `brew audit` as redundant), so nothing
			// here should ever emit "v1.26.0" as a version VALUE, only as
			// part of the URL path.
			if strings.Contains(out, "v1.26.0/orbeat-sync-linux-amd64") == false {
				t.Errorf("Render(Version=%q): expected asset URL for v1.26.0 tag not found:\n%s", c.version, out)
			}
			if strings.Contains(out, "\"v1.26.0\"") {
				t.Errorf("Render(Version=%q): rendered formula contains a v-prefixed version literal:\n%s", c.version, out)
			}
		})
	}
}

// TestRenderMissingChecksum asserts that omitting the checksum for ANY one
// platform is an error naming that platform's asset - not a formula that
// silently installs an asset nobody verified the hash of. The mutant this
// guards against: validating only len(Checksums) == len(Platforms()) would
// pass with the WRONG platform's checksum substituted in.
func TestRenderMissingChecksum(t *testing.T) {
	for _, missing := range Platforms() {
		t.Run(AssetName(missing), func(t *testing.T) {
			cs := validChecksums()
			delete(cs, missing)
			_, err := Render(Options{Version: "1.26.0", Checksums: cs})
			if err == nil {
				t.Fatalf("Render succeeded with no checksum for %s, want an error", AssetName(missing))
			}
			if !strings.Contains(err.Error(), AssetName(missing)) {
				t.Errorf("Render error %q does not name the missing asset %s", err.Error(), AssetName(missing))
			}
		})
	}
}

// TestRenderRejectsMalformedChecksum covers shapes that are NOT exactly 64
// lowercase hex characters. It does not, and cannot, cover a checksum
// swapped between two platforms in the input map: a swapped value is still a
// syntactically valid 64-lowercase-hex string, so format validation has
// nothing to reject - that swap is a correctness bug outside what this test
// claims to catch, not one of the shapes exercised below.
func TestRenderRejectsMalformedChecksum(t *testing.T) {
	cases := map[string]string{
		"too short":     sha('a')[:63],
		"too long":      sha('a') + "a",
		"uppercase":     strings.ToUpper(sha('a')),
		"non-hex chars": strings.Repeat("z", 64),
		"empty":         "",
		"with newline":  sha('a') + "\n",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			cs := validChecksums()
			cs[Platform{GOOS: "linux", GOARCH: "amd64"}] = bad
			_, err := Render(Options{Version: "1.26.0", Checksums: cs})
			if err == nil {
				t.Fatalf("Render succeeded with checksum %q, want an error", bad)
			}
		})
	}
}

// TestRenderDefaultBaseURL asserts the documented default download root is
// used when Options.BaseURL is empty.
func TestRenderDefaultBaseURL(t *testing.T) {
	out, err := Render(Options{Version: "1.26.0", Checksums: validChecksums()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "https://github.com/stefanocalabrese/orbeat-community-community/releases/download/v1.26.0/orbeat-sync-linux-amd64"
	if !strings.Contains(out, want) {
		t.Errorf("Render output missing default-base-URL asset URL %q:\n%s", want, out)
	}
}

// TestRenderCustomBaseURL asserts a caller-supplied BaseURL is honored
// verbatim (trailing slash tolerated), rather than the default always
// winning regardless of Options.
func TestRenderCustomBaseURL(t *testing.T) {
	for _, base := range []string{
		"https://example.test/dl",
		"https://example.test/dl/",
	} {
		t.Run(base, func(t *testing.T) {
			out, err := Render(Options{Version: "1.26.0", BaseURL: base, Checksums: validChecksums()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			want := "https://example.test/dl/v1.26.0/orbeat-sync-linux-amd64"
			if !strings.Contains(out, want) {
				t.Errorf("Render(BaseURL=%q) missing %q:\n%s", base, want, out)
			}
			if strings.Contains(out, "orbeat-community/releases/download") {
				t.Errorf("Render(BaseURL=%q) still contains the default base URL:\n%s", base, out)
			}
		})
	}
}

// TestRenderContent asserts the structural requirements the task spec lists,
// each pinned to a specific mutant so the assertion can actually fail:
//   - class name and metadata (a mutant renaming the class or dropping
//     license would be a broken/unpublishable formula)
//   - each platform's own url+sha256 pair, checked TOGETHER so a mutant that
//     cross-wires two platforms' checksums is caught (checking url and sha256
//     presence independently would not catch a swap)
//   - the install rename (the downloaded asset is never literally named
//     "orbeat-sync", so a mutant that installed it unrenamed would leave
//     `orbeat-sync` missing from bin)
//   - the version-asserting test block
func TestRenderContent(t *testing.T) {
	out, err := Render(Options{Version: "v1.26.0", Checksums: validChecksums()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	mustContain(t, out, "class OrbeatSync < Formula")
	mustContain(t, out, `license "Apache-2.0"`)
	mustContain(t, out, `homepage "https://github.com/stefanocalabrese/orbeat-community-community"`)

	platformPairs := map[Platform]string{
		{GOOS: "linux", GOARCH: "amd64"}:  sha('a'),
		{GOOS: "linux", GOARCH: "arm64"}:  sha('b'),
		{GOOS: "darwin", GOARCH: "arm64"}: sha('c'),
	}
	for p, sum := range platformPairs {
		url := "https://github.com/stefanocalabrese/orbeat-community-community/releases/download/v1.26.0/" + AssetName(p)
		urlLine := `url "` + url + `"`
		shaLine := `sha256 "` + sum + `"`
		idx := strings.Index(out, urlLine)
		if idx < 0 {
			t.Fatalf("missing url line for %s:\n%q\nin:\n%s", AssetName(p), urlLine, out)
		}
		// The matching sha256 must be the line immediately following its url
		// line - proving they're paired, not just both present anywhere in
		// the file (which a cross-wiring mutant would still satisfy).
		after := out[idx+len(urlLine):]
		parts := strings.SplitN(after, "\n", 3)
		if len(parts) < 2 {
			t.Fatalf("for %s: url line has no following line in:\n%s", AssetName(p), out)
		}
		nextLine := strings.TrimSpace(parts[1])
		if nextLine != shaLine {
			t.Errorf("for %s: line after url is %q, want %q", AssetName(p), nextLine, shaLine)
		}
	}

	mustContain(t, out, `bin.install Dir["orbeat-sync-*"].first => "orbeat-sync"`)
	mustContain(t, out, "test do")
	mustContain(t, out, `assert_match version.to_s, shell_output("#{bin}/orbeat-sync --version")`)
}

// extractBlock returns the body of the first "<marker> ... \n    end\n"
// block in s: the text between the line naming marker and the matching
// 4-space-indented "end" line. It exists so a test can assert a fact holds
// INSIDE one specific Ruby sub-block, not merely somewhere in a larger
// surrounding block that happens to contain several sub-blocks. ok is false
// if marker is not found.
func extractBlock(s, marker string) (body string, ok bool) {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return "", false
	}
	bodyStart := idx + len(marker)
	rel := strings.Index(s[bodyStart:], "\n    end\n")
	if rel < 0 {
		return "", false
	}
	return s[bodyStart : bodyStart+rel], true
}

// TestRenderIntelRefusal asserts the macOS-Intel refusal (odie, the reason,
// the fallback command) lives INSIDE the on_intel sub-block, and that the
// installable Apple Silicon asset (url + its own sha256) lives INSIDE the
// separate on_arm sub-block - not merely that all of these strings appear
// somewhere under on_macos. A prior version of this test checked presence
// only; a reviewer red-proved with `go test -overlay` that swapping the two
// sub-blocks' bodies (odie moved under on_arm, url/sha256 moved under
// on_intel) left the whole package green, while the resulting formula would
// abort on every Apple Silicon Mac and hand the darwin-arm64 binary to an
// Intel one - the opposite of what this formula is for. This is the exact
// shape verified live with `brew audit --formula --os macos --arch intel`
// to actually fire.
func TestRenderIntelRefusal(t *testing.T) {
	checksums := validChecksums()
	out, err := Render(Options{Version: "1.26.0", Checksums: checksums})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	macosIdx := strings.Index(out, "on_macos do")
	if macosIdx < 0 {
		t.Fatal("no on_macos block")
	}
	macosEnd := strings.Index(out, "\n  on_linux do")
	if macosEnd < 0 {
		macosEnd = len(out)
	}
	macosBlock := out[macosIdx:macosEnd]
	mustContain(t, macosBlock, "depends_on arch: :arm64")

	onIntel, ok := extractBlock(macosBlock, "on_intel do\n")
	if !ok {
		t.Fatal("no on_intel block found under on_macos")
	}
	mustContain(t, onIntel, "odie")
	mustContain(t, onIntel, intelFallbackCmd)
	if !strings.Contains(onIntel, "Intel") {
		t.Errorf("on_intel block does not name Intel macOS as the reason for refusal:\n%s", onIntel)
	}
	if strings.Contains(onIntel, "url \"") {
		t.Errorf("on_intel block (the refusal) unexpectedly contains a url line:\n%s", onIntel)
	}

	onArm, ok := extractBlock(macosBlock, "on_arm do\n")
	if !ok {
		t.Fatal("no on_arm block found under on_macos")
	}
	wantURL := `url "https://github.com/stefanocalabrese/orbeat-community-community/releases/download/v1.26.0/orbeat-sync-darwin-arm64"`
	wantSHA := `sha256 "` + checksums[Platform{GOOS: "darwin", GOARCH: "arm64"}] + `"`
	mustContain(t, onArm, wantURL)
	mustContain(t, onArm, wantSHA)
	if strings.Contains(onArm, "odie") {
		t.Errorf("on_arm block (the installable Apple Silicon asset) unexpectedly contains odie:\n%s", onArm)
	}
}

// TestRenderLinuxPlacement is on_linux's sibling to TestRenderIntelRefusal
// above: it asserts the linux/amd64 url+sha256 pair lives INSIDE the
// on_intel sub-block and the linux/arm64 pair lives INSIDE the separate
// on_arm sub-block, not merely that all four strings appear somewhere under
// on_linux. Swapping the two sub-blocks' bodies (amd64 under on_arm, arm64
// under on_intel) left the whole package green before this test existed:
// Homebrew's own checksum verification does not care WHICH platform a url is
// filed under, only that the sha256 on the next line matches the bytes at
// that url, so a swapped formula still passes `brew style` and
// `brew audit --os linux --arch arm` while installing an amd64 binary on
// arm64 hosts and vice versa - the "exec format error" only shows up on a
// real machine, never in this package's tests, unless the placement itself
// is checked.
func TestRenderLinuxPlacement(t *testing.T) {
	checksums := validChecksums()
	out, err := Render(Options{Version: "1.26.0", Checksums: checksums})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	linuxIdx := strings.Index(out, "on_linux do")
	if linuxIdx < 0 {
		t.Fatal("no on_linux block")
	}
	linuxEnd := strings.Index(out, "\n  def install")
	if linuxEnd < 0 {
		linuxEnd = len(out)
	}
	linuxBlock := out[linuxIdx:linuxEnd]

	onArm, ok := extractBlock(linuxBlock, "on_arm do\n")
	if !ok {
		t.Fatal("no on_arm block found under on_linux")
	}
	wantArmURL := `url "https://github.com/stefanocalabrese/orbeat-community-community/releases/download/v1.26.0/orbeat-sync-linux-arm64"`
	wantArmSHA := `sha256 "` + checksums[Platform{GOOS: "linux", GOARCH: "arm64"}] + `"`
	mustContain(t, onArm, wantArmURL)
	mustContain(t, onArm, wantArmSHA)
	if strings.Contains(onArm, "orbeat-sync-linux-amd64") {
		t.Errorf("on_arm block under on_linux unexpectedly references the amd64 asset:\n%s", onArm)
	}

	onIntel, ok := extractBlock(linuxBlock, "on_intel do\n")
	if !ok {
		t.Fatal("no on_intel block found under on_linux")
	}
	wantIntelURL := `url "https://github.com/stefanocalabrese/orbeat-community-community/releases/download/v1.26.0/orbeat-sync-linux-amd64"`
	wantIntelSHA := `sha256 "` + checksums[Platform{GOOS: "linux", GOARCH: "amd64"}] + `"`
	mustContain(t, onIntel, wantIntelURL)
	mustContain(t, onIntel, wantIntelSHA)
	if strings.Contains(onIntel, "orbeat-sync-linux-arm64") {
		t.Errorf("on_intel block under on_linux unexpectedly references the arm64 asset:\n%s", onIntel)
	}
}

// TestRenderOutputCoversEveryPlatform iterates Platforms() against Render's
// REAL output and asserts each asset name appears followed, on the very
// next line, by its own checksum. This is the same pairing TestRenderContent
// checks by hand against a fixed map of the three current platforms;
// this version is driven by Platforms() itself, so it also stands as a
// live (non-synthetic) check that verifyCoverage's guarantee actually holds
// for what Render produces today, not only in the synthetic case
// TestVerifyCoverageCatchesMissingPlatform constructs below.
func TestRenderOutputCoversEveryPlatform(t *testing.T) {
	checksums := validChecksums()
	out, err := Render(Options{Version: "1.26.0", Checksums: checksums})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, p := range Platforms() {
		name := AssetName(p)
		idx := strings.Index(out, name)
		if idx < 0 {
			t.Fatalf("rendered formula does not mention %s", name)
		}
		nl := strings.IndexByte(out[idx:], '\n')
		if nl < 0 {
			t.Fatalf("asset name %s has no following line", name)
		}
		after := out[idx+nl:]
		parts := strings.SplitN(after, "\n", 3)
		if len(parts) < 2 {
			t.Fatalf("asset name %s has no following line in:\n%s", name, out)
		}
		nextLine := strings.TrimSpace(parts[1])
		want := `sha256 "` + checksums[p] + `"`
		if nextLine != want {
			t.Errorf("for %s: line after its mention is %q, want %q", name, nextLine, want)
		}
	}
}

// TestVerifyCoverageCatchesMissingPlatform proves verifyCoverage's mechanism
// directly, independent of Platforms()'s current fixed content (which
// cannot be mutated from a test). It builds a synthetic "rendered" string
// that covers every platform EXCEPT one - simulating exactly the scenario a
// false comment in an earlier version of this package claimed the
// per-checksum completeness loop alone prevented: Platforms() growing past
// what a fixed template renders.
func TestVerifyCoverageCatchesMissingPlatform(t *testing.T) {
	checksums := validChecksums()
	for _, missing := range Platforms() {
		t.Run(AssetName(missing), func(t *testing.T) {
			var b strings.Builder
			for _, p := range Platforms() {
				if p == missing {
					continue // simulate a template that never mentions this platform
				}
				fmt.Fprintf(&b, "url %q\nsha256 %q\n", "https://example.test/v1.0.0/"+AssetName(p), checksums[p])
			}
			err := verifyCoverage(b.String(), "https://example.test", "1.0.0", checksums)
			if err == nil {
				t.Fatalf("verifyCoverage did not catch a rendered formula missing %s", AssetName(missing))
			}
			if !strings.Contains(err.Error(), AssetName(missing)) {
				t.Errorf("verifyCoverage error %q does not name %s", err.Error(), AssetName(missing))
			}
		})
	}
}

// TestRequirePlatformRejectsUnknownCombo pins requirePlatform's error path
// with a combo that is not in Platforms() today and has no plan to be
// (windows/amd64 orbeat-sync does not ship). This does not depend on being
// able to mutate Platforms(), unlike the scenario it exists to guard
// (Platforms() losing one of the three combos the template hardcodes).
func TestRequirePlatformRejectsUnknownCombo(t *testing.T) {
	if _, err := requirePlatform("windows", "amd64"); err == nil {
		t.Fatal("requirePlatform(windows, amd64) succeeded, want an error: this platform is not in Platforms()")
	}
}

// TestRequirePlatformAcceptsEveryTemplateSlot pins the positive case for the
// three combos Render's template actually hardcodes.
func TestRequirePlatformAcceptsEveryTemplateSlot(t *testing.T) {
	for _, tc := range []struct{ goos, arch string }{
		{"darwin", "arm64"},
		{"linux", "arm64"},
		{"linux", "amd64"},
	} {
		if _, err := requirePlatform(tc.goos, tc.arch); err != nil {
			t.Errorf("requirePlatform(%s, %s): %v", tc.goos, tc.arch, err)
		}
	}
}

// TestRenderRejectsUnsafeBaseURL covers the injection surface Go's %q does
// not close: it escapes Go/Ruby string-literal syntax but has no concept of
// Ruby's "#{...}" interpolation, so a BaseURL containing "#{" would reach
// the rendered formula as live Ruby. Also covers the plain scheme check.
func TestRenderRejectsUnsafeBaseURL(t *testing.T) {
	cases := map[string]string{
		"no scheme":          "example.test/dl",
		"ftp scheme":         "ftp://example.test/dl",
		"ruby interpolation": "https://example.test/dl#{`touch /tmp/pwned`}",
		"bare fragment":      "https://example.test/dl#fragment",
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(Options{Version: "1.26.0", BaseURL: base, Checksums: validChecksums()})
			if err == nil {
				t.Fatalf("Render(BaseURL=%q) succeeded, want an error", base)
			}
		})
	}
}

// TestRenderHasNoExplicitVersionStatement pins the package doc comment's
// second empirically-verified claim in Go rather than only in prose: an
// explicit `version "X.Y.Z"` line fails `brew audit` as redundant with the
// version scanned from the URL, and `brew audit` is not part of this repo's
// CI, so nothing else would catch a future template edit that reintroduces
// one. TestRenderVersionForms already forbids the specific "v1.26.0" literal
// as a VALUE, which would not catch a bare `version "1.26.0"` statement
// (no "v" prefix) - this checks for the statement itself.
func TestRenderHasNoExplicitVersionStatement(t *testing.T) {
	out, err := Render(Options{Version: "1.26.0", Checksums: validChecksums()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, `version "`) {
		t.Errorf("rendered formula contains an explicit version statement, which brew audit "+
			"rejects as redundant with the version scanned from the URL:\n%s", out)
	}
}

// TestIntelFallbackCmdHasNoHash pins that intelFallbackCmd never contains a
// "#". render.go's on_intel block renders it unquoted inside a Ruby
// `<<~EOS` heredoc, which Ruby interpolates - unlike the url/sha256/desc/
// homepage/license fields, which all go through Go's %q first. There is no
// live bug today (the const is fixed source, never derived from a caller
// argument, so validateBaseURL's "#" rejection has nothing to do with it),
// but a future edit adding a Markdown-style "#" heading or a literal "#{"
// to this const would become live Ruby the moment it is templated in.
func TestIntelFallbackCmdHasNoHash(t *testing.T) {
	if strings.Contains(intelFallbackCmd, "#") {
		t.Errorf("intelFallbackCmd contains %q: render.go's on_intel heredoc interpolates this "+
			"const unquoted, so a \"#\" would reach Ruby as live text or interpolation", "#")
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output missing %q:\n%s", needle, haystack)
	}
}
