package api

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// muxHandleSite is one mux.Handle(pattern, handler) call site, found by
// walking the package's real syntax tree rather than matching text against a
// regex. patternLiteral is false whenever the first argument is anything
// other than a plain double-quoted string: a variable, a constant, string
// concatenation, a function call, anything computed rather than written out
// verbatim at the call site. When it is false, method and path are
// meaningless and this site cannot be classified as under /v1/admin/ or not
// by this derivation at all; that is a finding TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite
// must report loudly, not a reason to drop the site silently. See
// allMuxHandleSites' doc comment for why this replaced a second regex.
type muxHandleSite struct {
	pos            string // file:line, for failure messages
	patternLiteral bool
	method, path   string
	wrapper        string // "" if the handler argument isn't a single wrapper(s.<name>) call
	handler        string
}

// allMuxHandleSites walks every non-test *.go file in the package (the same
// file set packageGoSource, openapi_test.go, reads, listed here directly
// since go/parser needs paths rather than concatenated source) and returns
// one muxHandleSite per mux.Handle(...) call found via go/ast.
//
// WHY AST, NOT A REGEX. A regex-based extraction can only ever report what it
// matched; it has no way to report what exists in the source but did not
// match its assumed shape. That is exactly how PUT
// /v1/admin/artifacts/{id}/min-revision escaped this test's OWN earlier fix:
// this file used to require the mux.Handle pattern to be a quoted literal
// directly inside the call (adminPrefixRouteRe, since removed), and
//
//	minRevPath := "PUT /v1/admin/artifacts/{id}/min-revision"
//	mux.Handle(minRevPath, authed(s.handleSetArtifactMinRevision))
//
// simply is not that shape: the pattern is a variable, not a literal, so the
// regex produced no match and no error, and the route silently left the
// derived set at the exact moment it was downgraded to authed(...). go/ast
// instead enumerates the call site itself, whatever its arguments look like,
// so this function reports on EVERY mux.Handle call, literal pattern or not,
// and derivedAdminRoutes below turns "not a literal" into a hard failure
// rather than a silent gap.
//
// SCOPE, STATED RATHER THAN IMPLIED. This closes the class of "the pattern
// argument isn't shaped the way the extractor expected". It does not close
// every way a route could be invisible to this derivation:
//   - A mux.Handle call split across many lines is fully covered: go/parser
//     builds the same AST regardless of line breaks, so formatting cannot
//     hide a call site from ast.Inspect the way it could from a
//     single-line-oriented regex.
//   - A route registered from a file this function does not read is NOT
//     covered beyond the same boundary packageGoSource already has: this
//     walks every non-test *.go file in THIS package directory
//     (internal/api) via filepath.Glob("*.go"), the same set every sibling
//     derivation in openapi_test.go reads. A route registered by calling
//     into a different package that itself does mux.Handle(...) on the
//     mux passed to it would not be found; nothing in this codebase does
//     that today (registerEnterpriseRoutes takes the mux as a parameter but
//     lives in this same package), and closing that would mean scanning
//     arbitrary imported packages, which is a different, much larger
//     mechanism than this test needs today.
//   - A SECOND, differently-named router is NOT covered: this only
//     recognizes calls of the exact form mux.Handle(...), matching on the
//     receiver identifier "mux" by name, because that is the one and only
//     name Handler() and registerEnterpriseRoutes ever bind their
//     *http.ServeMux to. A route registered on a renamed variable, or on a
//     second ServeMux instance under any other name, would be invisible to
//     this check with no error at all. This is not a new gap: every sibling
//     derivation in openapi_test.go (codeRoutes, derivedPaginatedRoutes,
//     derivedGuardedRoutes) hard-codes the identical "mux" identifier and
//     shares the exact same limitation; broadening the match to any
//     identifier's .Handle(...) would trade a named, narrow limitation for
//     unpredictable false positives on unrelated .Handle-shaped calls
//     elsewhere in the package, which is not a mechanism this test needs
//     today either.
func allMuxHandleSites(t *testing.T) []muxHandleSite {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	fset := token.NewFileSet()
	var sites []muxHandleSite
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "mux" {
				return true
			}
			if len(call.Args) != 2 {
				return true
			}
			sites = append(sites, muxHandleSiteFrom(fset, call))
			return true
		})
	}
	if len(sites) == 0 {
		t.Fatal("no mux.Handle(...) call sites found in package source, extraction is stale")
	}
	return sites
}

// muxHandleSiteFrom decodes one mux.Handle(pattern, handler) *ast.CallExpr,
// already known to have exactly two arguments.
func muxHandleSiteFrom(fset *token.FileSet, call *ast.CallExpr) muxHandleSite {
	site := muxHandleSite{pos: fset.Position(call.Pos()).String()}

	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if pattern, err := strconv.Unquote(lit.Value); err == nil {
			site.patternLiteral = true
			if method, path, found := strings.Cut(pattern, " "); found {
				site.method, site.path = method, path
			} else {
				site.path = pattern
			}
		}
	}

	if wrap, ok := call.Args[1].(*ast.CallExpr); ok && len(wrap.Args) == 1 {
		if wrapName, ok := wrap.Fun.(*ast.Ident); ok {
			if arg, ok := wrap.Args[0].(*ast.SelectorExpr); ok {
				if recv, ok := arg.X.(*ast.Ident); ok && recv.Name == "s" {
					site.wrapper = wrapName.Name
					site.handler = arg.Sel.Name
				}
			}
		}
	}
	return site
}

// adminRouteMeta is the best-effort handler/wrapper pair derivedAdminRoutes
// attaches to each route, best-effort because set membership does not
// require a route to carry a shape muxHandleSiteFrom can parse. A route with
// wrapper "admin" is exactly a route registered through the literal
// admin(...) RBAC gate; any other value, including the zero value, is not,
// and TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite treats that as a
// failure to report, never as a reason to skip the route.
type adminRouteMeta struct {
	handler string // "" if no wrapper(s.handler) shape was found for this route
	wrapper string // "" likewise; "admin" is the only value the RBAC gate is satisfied by
}

// derivedAdminRoutes reads the package's source and returns every {method,
// path} registered under the /v1/admin/ prefix, each mapped to its
// best-effort handler/wrapper metadata (for failure messages and the wrapper
// assertion in TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite; NOT what
// determines membership). Not a hand-maintained list, so a newly added admin
// route is automatically covered without anyone remembering to add a case,
// mirroring the derivedPaginatedRoutes / derivedGuardedRoutes idiom in
// openapi_test.go.
//
// Every mux.Handle(...) call site allMuxHandleSites finds is accounted for,
// not just the ones whose pattern happens to be a literal: a call site whose
// pattern this derivation cannot read at all is a hard failure, naming the
// exact file:line, rather than a route quietly absent from the set. This is
// the fix for the second-order version of this test's own original defect:
// the first fix (deriving the set from the /v1/admin/ path prefix instead of
// from the admin(...) wrapper) closed "matched, but wrapped wrong"; this one
// closes "not matched at all", which a route registered through a path
// variable, a helper, or string concatenation would otherwise exploit
// exactly the way a wrapper downgrade used to, and did, on this route in
// review before this fix landed.
func derivedAdminRoutes(t *testing.T) map[string]adminRouteMeta {
	t.Helper()
	sites := allMuxHandleSites(t)

	var unparseable []string
	routes := map[string]adminRouteMeta{}
	for _, s := range sites {
		if !s.patternLiteral {
			unparseable = append(unparseable, s.pos)
			continue
		}
		if !strings.HasPrefix(s.path, "/v1/admin/") {
			continue
		}
		routes[s.method+" "+s.path] = adminRouteMeta{handler: s.handler, wrapper: s.wrapper}
	}
	if len(unparseable) > 0 {
		sort.Strings(unparseable)
		t.Fatalf("mux.Handle call site(s) whose route pattern is not a plain string literal, so this "+
			"derivation cannot tell whether they belong under /v1/admin/ and must be admin-gated: %s. "+
			"Register every mux.Handle pattern as a literal string so this check can classify it: a route "+
			"hidden behind a variable, a constant, or any computed expression is precisely how "+
			"PUT /v1/admin/artifacts/{id}/min-revision escaped this test's own earlier fix, downgraded to "+
			"authed(...) at the same time its path became unparseable", strings.Join(unparseable, ", "))
	}
	if len(routes) == 0 {
		t.Fatal("no routes under the /v1/admin/ prefix found in package source, extraction is stale")
	}
	return routes
}

// TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite is the class-wide fix for
// finding 1 (spec+quality review of the role-deletion slice, commit
// 2c24164): roleDeleteRequest's doc comment claimed that driving the real
// router "cannot catch... the admin gate missing" — false, because every
// test in the package only ever sends an admin token, so an admin route
// silently downgraded to authed(...) (a copy-paste from the authed(...)
// block in api.go, or a bad merge) would fail NOTHING.
//
// THIS TEST ITSELF LATER SHIPPED THE SAME CLASS OF DEFECT, TWICE, ONE LEVEL
// DEEPER EACH TIME. Its original version derived the expected route set from
// the literal admin(...) wrapper, so a route downgraded to authed(...)
// simply left the derived set instead of failing in it: no subtest was
// generated, nothing went red (open-points.md, "cannot detect the one
// mutant its own doc comment names"; reproduced live on
// GET /v1/admin/artifacts/{id}/revisions).
//
// The first fix derived the set from the /v1/admin/ path prefix instead
// (adminPrefixRouteRe, a regex requiring the pattern to be a quoted literal
// directly inside the mux.Handle call). Review then found the SAME
// self-fulfilling shape one level deeper: the new regex could only match a
// pattern it could see as a literal, so
//
//	minRevPath := "PUT /v1/admin/artifacts/{id}/min-revision"
//	mux.Handle(minRevPath, authed(s.handleSetArtifactMinRevision))
//
// left the derived set exactly as it used to leave it under the old wrapper
// regex, this time via a path variable rather than a wrapper downgrade, and
// simultaneously downgraded the route to authed(...). Measured: `go test
// ./internal/api/ -run TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite
// -count=1` stayed green with that mutant applied.
//
// Fixed by replacing regex extraction with go/ast (allMuxHandleSites):
// instead of asking "does this text match the shape I expect", the
// derivation now enumerates every mux.Handle(...) call site that exists,
// literal pattern or not, and treats a call site whose pattern is not a
// literal as a hard failure rather than a silent omission. A regex can only
// ever get cleverer about the shapes it recognizes and stay blind to
// whatever shape comes next; this instead makes "I could not classify this
// call site" impossible to pass through undetected. See allMuxHandleSites'
// doc comment for exactly what this still cannot see (a route registered
// from outside this package's files, or on a router bound to a name other
// than "mux") and why those are accepted, named limits rather than silently
// assumed to be covered.
//
// Every route the derivation accepts is asserted BOTH that it is wrapped in
// the literal admin(...) RBAC gate AND that it answers a non-admin 403
// through the real router (srv.Handler().ServeHTTP), with a valid,
// authenticated non-admin bearer token. Neither assertion substitutes for
// the other: the wrapper check is a source-level fact that catches a
// downgrade, a missing wrapper, or an unparseable pattern before any request
// is sent; the live check is a runtime fact that would still catch a route
// whose wrapper is right but whose gate is broken. Path wildcards ({id}) are
// filled with a well-formed-but-unknown UUID so the request is safe to send
// for every method including DELETE: RequireRole runs before
// resolver.Middleware (audit B4, see middleware_order_test.go), so a denied
// request must never reach a handler or write to the DB regardless of which
// route it targets. The trailing check (no user row for the probing
// subject) generalizes TestAdminRouteDeniesNonAdminBeforeAnyDBWrite's
// single-route assertion across every admin route in one pass, proving that
// ordering holds class-wide, not just for GET /v1/admin/servers.
//
// Red-proven on disk, each mutant applied and reverted in turn
// (allMuxHandleSites reads the checked-out files at go test runtime via
// go/parser, not something the compiler substitutes, so -overlay cannot
// reach it):
//
// (1) admin(s.handleListArtifactRevisions) -> authed(...) in
// routes_enterprise.ee.go (an Enterprise route). Three assertions fail
// together, all inside the one "GET /v1/admin/artifacts/{id}/revisions"
// subtest: the wrapper check ("...is wrapped in authed(...), not the literal
// admin(...) RBAC gate"), the live check (non-admin token got 404, not
// 403; the request reached the real handler), and the trailing no-DB-write
// check (a user row WAS upserted for the denied subject). Every other subtest
// stays green.
//
// (2) admin(s.handleDeleteRole) -> authed(...) in api.go (a core, non-
// Enterprise route). Identical three-way failure, this time scoped to
// "DELETE /v1/admin/roles/{id}" alone, proving the coverage generalizes
// across both files this package's source spans, not just the Enterprise
// one.
//
// (3) admin(s.handleSetArtifactMinRevision) -> the bare
// http.HandlerFunc(s.handleSetArtifactMinRevision), no admin(...), no
// authed(...), no RBAC wrapper of any kind, on PUT
// /v1/admin/artifacts/{id}/min-revision. The wrapper check fails ("...wrapped
// in <no wrapper(s.handler) shape found>(...)") and the live check fails
// (non-admin token got 500 "missing resolved context": the handler ran with
// no middleware ahead of it at all and tripped its own resolved-context
// guard, s.resolved's comma-ok read, rather than reaching real work). The
// trailing no-DB-write check stays green here, no middleware ran so nothing
// wrote, which is itself the point: the wrapper check is what catches this
// mutant reliably, since a handler's own defenses are not guaranteed to fail
// this safely.
//
// (4) the admin closure itself, unwrapping
// authz.RequireRole("orbeat-admin")(...) so admin(...) becomes behaviorally
// identical to authed(...) while every mux.Handle call site still reads
// literally admin(s.<handler>). This is the failure mode the OLD (pre-any-
// of-this) gate already caught, and it must not be lost by adding the
// wrapper and unparseable-pattern checks: with this mutant applied, the
// wrapper assertion stays GREEN on all 32 subtests (the source text is
// untouched), while ALL 32 live checks fail (each route's real handler now
// answers its ordinary non-error status to a non-admin: 200, 400, 404 or 428
// depending on the route, never 403) and the trailing no-DB-write check
// fails too. Proves the wrapper and live assertions are independent: a route
// can be correctly wrapped in source and still be caught here if the shared
// gate itself breaks.
//
// (5) the review mutant that motivated allMuxHandleSites, on the SAME route
// as (3): registering PUT /v1/admin/artifacts/{id}/min-revision as
//
//	minRevPath := "PUT /v1/admin/artifacts/{id}/min-revision"
//	mux.Handle(minRevPath, authed(s.handleSetArtifactMinRevision))
//
// fails derivedAdminRoutes itself, at the t.Fatalf inside it (not a subtest:
// the whole test aborts before any subtest runs), naming the exact call site
// ("mux.Handle call site(s) whose route pattern is not a plain string
// literal ... routes_enterprise.ee.go:<line>").
func TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("admin-gate-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)

	const subject = "kc-admin-gate-nonadmin"
	tok := idp.token(t, subject, []string{"orbeat-user"})

	routes := derivedAdminRoutes(t)
	var keys []string
	for r := range routes {
		keys = append(keys, r)
	}
	sort.Strings(keys)

	for _, route := range keys {
		route := route
		meta := routes[route]
		t.Run(route, func(t *testing.T) {
			// The wrapper assertion. Independent of the live check below (a
			// route can be correctly wrapped and still fail there if the gate
			// itself is broken, and vice versa): t.Errorf rather than
			// t.Fatalf so a route failing both still reports both.
			// Two wrappers are legitimate now: admin(...) and curated(...),
			// the latter being admin PLUS the artifact-manager role. Anything
			// else under /v1/admin/ is a route that escaped both gates.
			//
			// This assertion got WEAKER by one name, so the strength moved to
			// TestArtifactManagerReachesOnlyTheArtifactSurface below, which
			// pins exactly which routes each wrapper is allowed to appear on.
			// Without that second test, `curated` spreading to, say, the
			// virtual-key routes would pass here in silence.
			if meta.wrapper != "admin" && meta.wrapper != "curated" {
				wrapper := meta.wrapper
				if wrapper == "" {
					wrapper = "<no wrapper(s.handler) shape found>"
				}
				t.Errorf("%s is registered under /v1/admin/ but is wrapped in %s(...), not admin(...) or "+
					"curated(...); an authenticated non-admin must never reach it regardless of "+
					"what wraps it", route, wrapper)
			}

			handler := meta.handler
			if handler == "" {
				handler = "<unknown>"
			}
			parts := strings.SplitN(route, " ", 2)
			method, path := parts[0], parts[1]
			target := strings.ReplaceAll(path, "{id}", "00000000-0000-0000-0000-000000000000")

			req := httptest.NewRequest(method, target, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s (handler %s) with a non-admin token = %d, want 403, body=%s",
					route, handler, rec.Code, rec.Body)
			}
		})
	}

	// The gate must run before any DB write (audit B4): a flood of
	// unauthorized admin-route probes must never upsert a tenant/user row for
	// the denied subject, across EVERY admin route, not just one.
	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant lookup: %v", err)
	}
	if _, err := st.GetUserBySubject(ctx, tn.ID, subject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row for the denied non-admin subject, got err=%v", err)
	}
}
