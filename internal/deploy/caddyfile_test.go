package deploy

import (
	"strings"
	"testing"
)

// caddyfilePath is the production Caddy config. caddyfileSmokePath is the
// second, hand-maintained Caddyfile make smoke-prod actually runs (mounted
// in place of caddyfilePath by
// deploy/docker-compose.prod-smoke.yml) — tls internal + a literal
// orbeat.localhost in place of ACME + {$ORBEAT_DOMAIN}. Nothing generates one
// from the other, so TestSmokeCaddyfileMatchesProductionRouting exists to
// keep them from silently diverging: without it, make smoke-prod proves a
// topology that is not production's, which is exactly the false confidence
// docs/specs/2026-08-19-orbeat-prod-config-gate-design.md's postmortem
// ("a dev-only integration gate gives false confidence on prod-specific
// config") warns about — just one file lower.
const (
	caddyfilePath      = "deploy/caddy/Caddyfile"
	caddyfileSmokePath = "deploy/caddy/Caddyfile.smoke"
)

// caddyBlock is one top-level `{ ... }` block: either the global options
// block (header == "") or a site block (header == the site address, e.g.
// "auth.{$ORBEAT_DOMAIN}").
type caddyBlock struct {
	header string
	lines  []string // trimmed, non-empty, non-comment body lines at depth 1
}

// parseCaddyfile is a deliberately minimal, single-level parser — the file
// is 25 lines of Caddy config, not YAML, and pulling in a real Caddy
// dependency just to read site-block headers and their reverse_proxy
// directives would be a heavier and less legible dependency than this repo
// carries for any other config format. It asserts on structure (which
// blocks exist, in what order, and what directives each contains), not on
// exact whitespace, and refuses (via t.Fatalf) rather than silently
// misparsing anything nested deeper than one level — this file has none, so
// a future Caddyfile that does would fail loudly here instead of parsing
// wrong.
func parseCaddyfile(t *testing.T, raw string) []caddyBlock {
	t.Helper()
	var blocks []caddyBlock
	var cur *caddyBlock
	depth := 0
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case depth == 0 && strings.HasSuffix(line, "{"):
			header := strings.TrimSpace(strings.TrimSuffix(line, "{"))
			blocks = append(blocks, caddyBlock{header: header})
			cur = &blocks[len(blocks)-1]
			depth = 1
		case depth == 1 && line == "}":
			depth = 0
			cur = nil
		case depth == 1:
			cur.lines = append(cur.lines, line)
		default:
			t.Fatalf("Caddyfile parser does not support nesting beyond one level, got %q at depth %d — this parser is deliberately minimal, extend it if the file grows real nesting", line, depth)
		}
	}
	if depth != 0 {
		t.Fatalf("Caddyfile has an unclosed block (parser depth %d at EOF)", depth)
	}
	return blocks
}

func caddySiteBlock(t *testing.T, blocks []caddyBlock, header string) caddyBlock {
	t.Helper()
	for _, b := range blocks {
		if b.header == header {
			return b
		}
	}
	t.Fatalf("no site block with header %q found", header)
	return caddyBlock{}
}

// TestCaddyfileHasExactlyThreeSiteBlocks pins the topology's shape: a fourth
// host appearing (or one disappearing) changes what's exposed and is easy
// to miss in review, since the global options block (the bare `{ email
// {$ACME_EMAIL} }` at the top) also opens a `{ ... }` and must be excluded
// from the count.
func TestCaddyfileHasExactlyThreeSiteBlocks(t *testing.T) {
	blocks := parseCaddyfile(t, string(repoFile(t, caddyfilePath)))
	var siteHeaders []string
	for _, b := range blocks {
		if b.header == "" {
			continue // the global options block, not a site
		}
		siteHeaders = append(siteHeaders, b.header)
	}
	want := []string{"{$ORBEAT_DOMAIN}", "auth.{$ORBEAT_DOMAIN}", "mcp.{$ORBEAT_DOMAIN}"}
	if len(siteHeaders) != len(want) {
		t.Fatalf("Caddyfile has %d site blocks %v, want exactly %d: %v", len(siteHeaders), siteHeaders, len(want), want)
	}
	for i, h := range want {
		if siteHeaders[i] != h {
			t.Errorf("site block %d = %q, want %q", i, siteHeaders[i], h)
		}
	}
}

// TestApexRoutesAPIBeforePortal pins ORDER, not just presence. Caddy matches
// a site block's directives in the order they are written and the first
// match wins, so a test that only checks both directives exist would pass
// even with them swapped — and swapped, every API call falls through to the
// unqualified `*` and reaches the portal instead of the api.
//
// Red-proof: swap the two reverse_proxy lines in the apex block. A
// presence-only assertion passes on the swap; this one does not.
func TestApexRoutesAPIBeforePortal(t *testing.T) {
	blocks := parseCaddyfile(t, string(repoFile(t, caddyfilePath)))
	apex := caddySiteBlock(t, blocks, "{$ORBEAT_DOMAIN}")

	var directives []string
	for _, l := range apex.lines {
		if strings.HasPrefix(l, "reverse_proxy") {
			directives = append(directives, l)
		}
	}
	if len(directives) != 2 {
		t.Fatalf("apex block has %d reverse_proxy directives %v, want exactly 2 (api, then portal)", len(directives), directives)
	}
	if !strings.Contains(directives[0], "/v1/*") || !strings.Contains(directives[0], "api:8080") {
		t.Errorf("first reverse_proxy = %q, want the /v1/* -> api:8080 route FIRST — Caddy matches in written order, so a catch-all placed before it would route every API call to the portal", directives[0])
	}
	if !strings.Contains(directives[1], "portal:8081") {
		t.Errorf("second reverse_proxy = %q, want the catch-all -> portal:8081 route second", directives[1])
	}
}

// TestMCPSiteProxiesGatewayAtRoot pins the topology's central fix: the
// gateway serves RFC 9728 discovery
// (/.well-known/oauth-protected-resource) and /mcp at its origin ROOT, so
// any path prefix or matcher on this site block breaks MCP OAuth discovery.
// This was a Critical found in an approved spec and the review reversed the
// topology from two subdomains to three specifically to give the gateway an
// unprefixed origin (v1.20.0).
func TestMCPSiteProxiesGatewayAtRoot(t *testing.T) {
	blocks := parseCaddyfile(t, string(repoFile(t, caddyfilePath)))
	mcp := caddySiteBlock(t, blocks, "mcp.{$ORBEAT_DOMAIN}")

	if len(mcp.lines) != 1 {
		t.Fatalf("mcp. block has %d directives %v, want exactly 1: an unprefixed reverse_proxy", len(mcp.lines), mcp.lines)
	}
	want := "reverse_proxy gateway:8090"
	if mcp.lines[0] != want {
		t.Errorf("mcp. directive = %q, want %q — any path matcher here breaks RFC 9728 discovery (v1.20.0)", mcp.lines[0], want)
	}
}

// TestAuthSiteProxiesKeycloakWithNoPathMatcher pins a DELIBERATE, accepted
// exposure, not a restriction. Keycloak's admin console
// (auth.<domain>/admin) is reachable from the public internet because this
// block is a bare reverse_proxy with no path matcher — recorded as entry 17
// of docs/threat-model.md, not an oversight: Keycloak must be
// browser-reachable for interactive SSO and the Dynamic Client Registration
// Claude Code requires, and its host-relative admin/asset paths are exactly
// why the topology uses three subdomains instead of path prefixes on one
// (docs/specs/2026-08-19-orbeat-prod-config-gate-design.md §2). This test
// adds NO new restriction — it pins the exposure so that anyone who later
// adds a path matcher or an IP allow-list here must update threat-model
// entry 17 in the same change, the same move
// internal/gateway.TestDialGuardAllowsPrivateRFC1918Address already makes
// for the SSRF guard's deliberate permissiveness.
//
// Red-proof for the absence half: this repo's standing lesson is that
// asserting something is ABSENT is trivially true on a file that never had
// it. This test's assertion is on the exact directive text (want ==
// "reverse_proxy keycloak:8080", nothing more), so adding a path matcher —
// e.g. "reverse_proxy /admin* keycloak:8080" — changes the line and fails
// this test, rather than the test only checking the line is missing a
// substring it never had a reason to contain.
func TestAuthSiteProxiesKeycloakWithNoPathMatcher(t *testing.T) {
	blocks := parseCaddyfile(t, string(repoFile(t, caddyfilePath)))
	auth := caddySiteBlock(t, blocks, "auth.{$ORBEAT_DOMAIN}")

	if len(auth.lines) != 1 {
		t.Fatalf("auth. block has %d directives %v, want exactly 1: an unprefixed reverse_proxy", len(auth.lines), auth.lines)
	}
	want := "reverse_proxy keycloak:8080"
	if auth.lines[0] != want {
		t.Errorf("auth. directive = %q, want %q — threat-model entry 17 records Keycloak's admin console as deliberately public; restricting it needs a threat-model update in the same change", auth.lines[0], want)
	}
}

// TestSmokeCaddyfileMatchesProductionRouting closes the drift risk this
// package's own gates left open: they pin deploy/caddy/Caddyfile, but
// deploy/docker-compose.prod-smoke.yml mounts a SECOND, hand-maintained file
// (Caddyfile.smoke) in its place for `make smoke-prod`. Nothing generates
// one from the other, so a routing change made to production and never
// mirrored into the smoke file would go live with `make smoke-prod` still
// green — proving a topology that isn't production's, which is exactly the
// false confidence the v1.20.0 postmortem names ("a dev-only integration
// gate gives false confidence on prod-specific config; the prod path needs
// its own gate").
//
// The two files' headers differ legitimately (production interpolates
// auth.{$ORBEAT_DOMAIN}; the smoke file hard-codes auth.orbeat.localhost, a
// resolvable local host for tls internal) and every smoke block carries a
// "tls internal" directive that production has no equivalent of — production
// gets its certificate from a single top-level `{ email {$ACME_EMAIL} }`
// options block instead, which does not exist in the smoke file at all. This
// test does not compare headers (that's the point of the smoke file) or the
// "tls internal" line (stripTLSInternal removes it, and separately requires
// it be present, so a smoke block that lost it fails loudly instead of
// silently passing a now-vacuous comparison). Everything else in each block
// must match byte-for-byte.
//
// Correspondence is derived by POSITION, not by parsing a subdomain prefix
// out of two differently-formatted hostnames ({$ORBEAT_DOMAIN} vs
// orbeat.localhost). This package already treats block order as meaningful
// (TestCaddyfileHasExactlyThreeSiteBlocks pins production's exact block
// order), the diff between the two files preserves that order today, and —
// because every block proxies a different upstream — a positional
// comparison still catches a reorder that isn't mirrored on both sides: it
// would compare two blocks with genuinely different routing and fail, just
// under a header pairing that names the mismatch rather than assuming it.
//
// Red-proof (both directions must fail; a consistent edit to both files must
// pass — proven manually, not committed as separate test cases, since each
// run requires temporarily editing a real deploy file):
//  1. Change a routing directive in Caddyfile only → fails.
//  2. Change the corresponding directive in Caddyfile.smoke only → fails.
//  3. Change both consistently → passes (proves correspondence, not a
//     hard-coded snapshot).
func TestSmokeCaddyfileMatchesProductionRouting(t *testing.T) {
	prodBlocks := parseCaddyfile(t, string(repoFile(t, caddyfilePath)))
	smokeBlocks := parseCaddyfile(t, string(repoFile(t, caddyfileSmokePath)))

	var prodSites []caddyBlock
	for _, b := range prodBlocks {
		if b.header == "" {
			continue // production's global ACME options block; Caddyfile.smoke has none
		}
		prodSites = append(prodSites, b)
	}

	if len(smokeBlocks) != len(prodSites) {
		t.Fatalf("Caddyfile.smoke has %d site blocks %v, production has %d %v — the two files must define the same site blocks, in the same order", len(smokeBlocks), headers(smokeBlocks), len(prodSites), headers(prodSites))
	}

	for i := range prodSites {
		prod, smoke := prodSites[i], smokeBlocks[i]
		smokeRouting := stripTLSInternal(t, smoke)

		if len(smokeRouting) != len(prod.lines) {
			t.Errorf("block %d: production %q has %d directive(s) %v, smoke %q has %d non-tls-internal directive(s) %v", i, prod.header, len(prod.lines), prod.lines, smoke.header, len(smokeRouting), smokeRouting)
			continue
		}
		for j := range prod.lines {
			if prod.lines[j] != smokeRouting[j] {
				t.Errorf("block %d: production %q directive %d = %q, smoke %q directive %d = %q — Caddyfile and Caddyfile.smoke have drifted", i, prod.header, j, prod.lines[j], smoke.header, j, smokeRouting[j])
			}
		}
	}
}

// headers extracts each block's header, for use in a t.Fatalf message —
// caddyBlock values don't print usefully via %v (lines is unexported and
// %v on a struct with an unexported slice is noisy), and the header is what
// identifies a block to a person reading the failure.
func headers(blocks []caddyBlock) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.header
	}
	return out
}

// stripTLSInternal removes the "tls internal" directive Caddyfile.smoke adds
// to every site block in place of production's global ACME options block —
// the one legitimate, deliberate difference in a smoke block's directive
// set. It fails loudly if a block doesn't carry it: a smoke block missing
// "tls internal" would (a) not actually work as a local TLS smoke stack and
// (b) make the routing comparison above compare against one fewer directive
// than intended, silently passing for the wrong reason.
func stripTLSInternal(t *testing.T, b caddyBlock) []string {
	t.Helper()
	out := make([]string, 0, len(b.lines))
	found := false
	for _, l := range b.lines {
		if l == "tls internal" {
			found = true
			continue
		}
		out = append(out, l)
	}
	if !found {
		t.Fatalf("smoke block %q has no \"tls internal\" directive %v — Caddyfile.smoke is expected to add one to every site block in place of production's ACME options", b.header, b.lines)
	}
	return out
}
