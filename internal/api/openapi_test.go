package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

var handleRe = regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)"`)

// packageGoSource concatenates every non-test *.go file in this package's
// directory into one string. Route-registration derivation (codeRoutes,
// derivedPaginatedRoutes, derivedGuardedRoutes, and admin_gate_test.go's
// derivedAdminRoutes) all scan for mux.Handle(...) call sites; reading a
// single named file (e.g. just "api.go") breaks the moment a registration
// moves to a sibling file (routes_enterprise.go registers the Enterprise
// admin routes api.go no longer names directly) — exactly the kind of drift
// a hand-maintained route list would introduce, which is what these
// derivations exist to avoid in the first place.
func packageGoSource(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	var sb strings.Builder
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if sb.Len() == 0 {
		t.Fatal("no non-test .go files found in package — glob is stale")
	}
	return sb.String()
}

// codeRoutes extracts every "METHOD /path" pair registered via mux.Handle
// across the package's source by regex — deliberately not reflection over
// the live mux, since http.ServeMux exposes no route-enumeration API. If
// this ever goes stale (e.g. a route registered a different way), the "no
// routes found" guard below fails loudly rather than silently reporting zero
// drift.
func codeRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	set := map[string]bool{}
	for _, m := range handleRe.FindAllStringSubmatch(src, -1) {
		set[m[1]+" "+m[2]] = true // "METHOD /path"
	}
	if len(set) == 0 {
		t.Fatal("no mux.Handle routes found in package source — extraction regex is stale")
	}
	return set
}

// specRoutes loads + validates the embedded OpenAPI document and extracts its
// "METHOD /path" set. kin-openapi's Operations() keys are the uppercase
// net/http method constants (GET, POST, ...), matching codeRoutes' regex group.
func specRoutes(t *testing.T) map[string]bool {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI 3.0 document: %v", err)
	}
	set := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			set[method+" "+path] = true
		}
	}
	return set
}

// knownEnterpriseRoutes is the {method, path} set registerEnterpriseRoutes
// (routes_enterprise.ee.go) registers — deliberately TIER-INVARIANT (defined
// once, in this shared file, not behind the .ee.go/.community.go extension
// point): it names a fixed fact ("these nineteen routes are Enterprise-only"),
// not tier-dependent behavior, and TestOpenAPICoversAllRoutes below needs the
// SAME nineteen-route allowance in both tiers. See that test's comment for why
// a tier-VARYING version of this list is exactly backwards.
//
// It was seven until the deployment registry's report route landed, eight
// until its admin read followed, nine until the minimum-revision floor made
// it ten, thirteen once virtual keys added three more, nineteen once
// SCIM's six /scim/v2/Users routes landed (docs/plans/orbeat-scim-2026-08-25.md
// Task 3), twenty-two once its three discovery routes (ServiceProviderConfig,
// ResourceTypes, Schemas) landed in Task 4, twenty-five once usage
// metering's report and quota routes landed
// (docs/plans/orbeat-usage-metering-2026-08-25.md Task 6), and twenty-six
// once the author findings-acknowledgment endpoint landed
// (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md Task 4). The count
// lives in prose no test reads, so it is stated here and in the test below
// in exactly two places, both corrected in the same commit as the entry that
// moves it.
var knownEnterpriseRoutes = []string{
	"GET /v1/admin/audit/export",
	"POST /v1/admin/artifacts/{id}/submit",
	// The author's acknowledgment of the scanner findings their submission
	// produced. No If-Match: the digest carried in the body IS the
	// precondition (handleAcknowledgeFindings' own comment), so this route
	// does NOT appear in enterpriseGuardedOps below.
	"POST /v1/admin/artifacts/{id}/acknowledge-findings",
	"POST /v1/admin/artifacts/{id}/approve",
	"POST /v1/admin/artifacts/{id}/reject",
	"POST /v1/admin/artifacts/{id}/withdraw",
	"GET /v1/admin/artifacts/{id}/revisions",
	"POST /v1/admin/artifacts/{id}/rollback",
	// The registry's admin read. Admin-gated like the seven above, so it is
	// also covered by TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite, which
	// derives its route set from the same package source.
	"GET /v1/admin/artifacts/{id}/deployments",
	// The only non-admin entry, and the only one whose registration is also
	// conditional at runtime (on s.deploymentRegistry). That condition is
	// invisible here on purpose: codeRoutes reads SOURCE, so the mux.Handle
	// line counts whether or not the knob is set, and what makes this route
	// Enterprise-only is the FILE it is registered from, exactly like the
	// eight above.
	"POST /v1/sync/deployments",
	// The artifact minimum-revision floor. Admin-gated, and it is the second
	// If-Match-guarded Enterprise route, so it is named in enterpriseGuardedOps
	// as well (routes_enterprise.ee.go).
	"PUT /v1/admin/artifacts/{id}/min-revision",
	// Virtual keys (docs/specs/2026-08-25-orbeat-virtual-keys-design.md sec
	// 11-12): robot credentials, Enterprise only because Community's single
	// role gives a key no shape distinct from every human's own access.
	// DELETE is the third If-Match-guarded Enterprise route
	// (enterpriseGuardedOps), GET is the second paginated one
	// (enterprisePaginatedOps).
	"POST /v1/admin/virtual-keys",
	"GET /v1/admin/virtual-keys",
	"DELETE /v1/admin/virtual-keys/{id}",
	// SCIM 2.0 (docs/specs/2026-08-25-orbeat-scim-design.md sec 4): Enterprise
	// only, same reasoning as virtual keys above (Community's single role and
	// lazily-upserted users give SCIM provisioning nothing to do). NOT under
	// /v1/admin/, so these six are also the reason Task 5 (not built in this
	// commit) needs its own derived admin-gate coverage rather than relying on
	// admin_gate_test.go's existing path-prefix rule.
	"POST /scim/v2/Users",
	"GET /scim/v2/Users",
	"GET /scim/v2/Users/{id}",
	"PATCH /scim/v2/Users/{id}",
	"DELETE /scim/v2/Users/{id}",
	"PUT /scim/v2/Users/{id}",
	// SCIM discovery (docs/plans/orbeat-scim-2026-08-25.md Task 4; RFC 7644
	// sec 4): ServiceProviderConfig, ResourceTypes and Schemas. Same
	// admin-gated, NOT-under-/v1/admin/ reasoning as the six /Users routes
	// above.
	"GET /scim/v2/ServiceProviderConfig",
	"GET /scim/v2/ResourceTypes",
	"GET /scim/v2/Schemas",
	// Usage metering and quotas (docs/specs/2026-08-25-orbeat-usage-metering-
	// design.md sec 5): Enterprise only, same reasoning as virtual keys above
	// (Community already caps seats, servers and roles, its own throttle).
	// GET is a paginated list but NOT via enterprisePaginatedOps -- it has
	// its own bespoke cursor (store.UsageReportCursor's doc comment,
	// usage.ee.go), so it does not call the shared pageParams( function
	// TestPaginatedOpsListIsExhaustive derives paginatedOps membership from.
	// PUT and DELETE are the fourth and fifth If-Match-guarded Enterprise
	// routes (enterpriseGuardedOps). GET was added to close audit B5: without
	// it, a blind client could only guess If-Match: "1" (the DEFAULT row_version).
	"GET /v1/admin/usage",
	"GET /v1/admin/roles/{id}/quota",
	"PUT /v1/admin/roles/{id}/quota",
	"DELETE /v1/admin/roles/{id}/quota",
}

// TestOpenAPICoversAllRoutes is the drift gate: every route api.go actually
// registers must be documented in openapi.yaml (missingFromSpec), and every
// path openapi.yaml documents that api.go does NOT register must be a KNOWN,
// accounted-for gap rather than silent drift (extraInSpec must be a subset of
// knownEnterpriseRoutes).
//
// In the Enterprise build (this repo, unchanged) codeRoutes already contains
// all nineteen of knownEnterpriseRoutes (registerEnterpriseRoutes registers
// them for real, and codeRoutes reads source, so the report route's runtime
// condition does not hide it), so extraInSpec is empty regardless of the
// allowance: exactly the old bidirectional equality check. In a generated
// Community tree, registerEnterpriseRoutes is a no-op
// (routes_enterprise.community.go), so codeRoutes legitimately omits the
// nineteen Enterprise routes, while openapi.yaml itself is NOT tier-split in
// this slice (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md
// §4 flags this as a known remaining gap: a generated Community binary's GET
// /openapi.yaml still documents nineteen endpoints it does not serve), so
// those nineteen, and ONLY those nineteen, must be let through as extraInSpec.
// This test still fails on any OTHER drift: a twentieth undocumented gap is
// not in knownEnterpriseRoutes and is still reported, in both tiers.
func TestOpenAPICoversAllRoutes(t *testing.T) {
	code := codeRoutes(t)
	spec := specRoutes(t)
	allowedExtra := map[string]bool{}
	for _, r := range knownEnterpriseRoutes {
		allowedExtra[r] = true
	}

	var missingFromSpec, extraInSpec []string
	for r := range code {
		if !spec[r] {
			missingFromSpec = append(missingFromSpec, r)
		}
	}
	for r := range spec {
		if !code[r] && !allowedExtra[r] {
			extraInSpec = append(extraInSpec, r)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(extraInSpec)
	if len(missingFromSpec) > 0 {
		t.Errorf("routes in api.go NOT documented in openapi.yaml (%d):\n  %s",
			len(missingFromSpec), strings.Join(missingFromSpec, "\n  "))
	}
	if len(extraInSpec) > 0 {
		t.Errorf("paths in openapi.yaml with NO matching api.go route, and not an accounted-for "+
			"Enterprise-only gap (%d):\n  %s", len(extraInSpec), strings.Join(extraInSpec, "\n  "))
	}
}

// paginatedOps is the {method, path} set that MUST document limit + cursor as
// query parameters AND limit + nextCursor on the 200 response envelope.
// /v1/admin/audit is deliberately excluded: it keeps its own (ts,id) cursor
// format and its own 100/1000 limits (spec §5.3), already documented on its
// own terms — see AuditPageResponse. This literal is enumerative by itself
// (see TestPaginatedOpsListIsExhaustive below for why that's not a gap).
// The Enterprise-only entry (the revisions list) is appended via
// enterprisePaginatedOps() rather than named here directly, so this literal
// is correct in both the Enterprise build and a generated Community tree
// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).
var paginatedOps = append([]string{
	"GET /v1/admin/servers",
	"GET /v1/admin/roles",
	"GET /v1/admin/entitlements",
	"GET /v1/admin/artifacts",
	"GET /v1/admin/artifact-entitlements",
}, enterprisePaginatedOps()...)

// sortOrderParamsHandlers returns the set of (*Server) method names whose
// body contains a call to the package-level sortOrderParams function
// (paging.go), the marker that identifies a list handler offering
// docs/plans/orbeat-admin-search-sort-2026-08-27.md's ?sort/?order.
func sortOrderParamsHandlers(t *testing.T) map[string]bool {
	t.Helper()
	return handlersCalling(t, "sortOrderParams")
}

// derivedSortableRoutes mirrors derivedPaginatedRoutes exactly (same
// mechanism, different marker function): it cross-references the route table
// against sortOrderParamsHandlers, computing from source, independent of
// and not trusting sortableOps, the exact route set whose handler offers
// ?sort/?order.
func derivedSortableRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	handlers := sortOrderParamsHandlers(t)
	routes := map[string]bool{}
	for _, m := range handleWithHandlerRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		if handlers[handler] {
			routes[method+" "+path] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no sortable routes derived from package source, extraction is stale")
	}
	return routes
}

// sortableOps is the {method, path} set that MUST document sort + order as
// query parameters (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task
// 3). It is a SUBSET of paginatedOps, deliberately: revisions
// (enterprisePaginatedOps' entry) keeps its own always-newest-first order and
// is not one of the six lists this task's allowlist (internal/api/paging.go)
// covers, and /v1/admin/audit was already out of paginatedOps for its own
// reasons. The Enterprise-only entry (virtual-keys) is appended via
// enterpriseSortableOps() rather than named here directly, mirroring
// paginatedOps' own split for the same Community-tree reason.
var sortableOps = append([]string{
	"GET /v1/admin/servers",
	"GET /v1/admin/roles",
	"GET /v1/admin/entitlements",
	"GET /v1/admin/artifacts",
	"GET /v1/admin/artifact-entitlements",
}, enterpriseSortableOps()...)

// sortColumnByOp is sortableOps' allowlisted ?sort value per route, the
// same six constants internal/api/paging.go defines, keyed here by route so
// TestOpenAPIDocumentsSortOrderParams can check the YAML's enum against the
// REAL Go constant rather than a value re-typed by hand into the test (which
// could drift from paging.go exactly the way this whole slice exists to
// prevent for the SQL side).
var sortColumnByOp = map[string]string{
	"GET /v1/admin/servers":               mcpServerSortName,
	"GET /v1/admin/roles":                 roleSortName,
	"GET /v1/admin/entitlements":          entitlementSortName,
	"GET /v1/admin/artifacts":             artifactSortName,
	"GET /v1/admin/artifact-entitlements": artifactEntitlementSortName,
	"GET /v1/admin/virtual-keys":          virtualKeySortName,
}

// TestSortableOpsListIsExhaustive guards sortableOps against drift from what
// the code actually does, mirrors TestPaginatedOpsListIsExhaustive exactly
// (same shape, same reason: sortableOps is otherwise purely enumerative, so
// a handler gaining or losing a sortOrderParams( call without a matching
// edit here would fail nothing in TestOpenAPIDocumentsSortOrderParams, which
// only ever looks at the routes the literal names).
func TestSortableOpsListIsExhaustive(t *testing.T) {
	derived := derivedSortableRoutes(t)
	want := map[string]bool{}
	for _, r := range sortableOps {
		want[r] = true
	}
	var missing, extra []string
	for r := range derived {
		if !want[r] {
			missing = append(missing, r)
		}
	}
	for r := range want {
		if !derived[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("handlers call sortOrderParams( but are missing from sortableOps (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("sortableOps lists a route whose handler does not call sortOrderParams( (%d):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// TestOpenAPIDocumentsSortOrderParams is TestOpenAPIDocumentsPaginationParams'
// sibling for ?sort/?order: every op in sortableOps must document both query
// parameters, order must $ref the shared components.parameters.ListOrder
// (identical everywhere, so it gets the two-layer $ref treatment
// TestOpenAPIPaginationParamsAreSharedAndCorrect already established for
// limit/cursor), and sort's enum must be EXACTLY the one-element set
// {sortColumnByOp[op]}, not merely present, and not a superset that would
// silently document a column the allowlist does not actually accept.
func TestOpenAPIDocumentsSortOrderParams(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	orderRef, ok := doc.Components.Parameters["ListOrder"]
	if !ok || orderRef.Value == nil || orderRef.Value.Schema == nil || orderRef.Value.Schema.Value == nil {
		t.Fatal("components.parameters.ListOrder is missing or has no schema")
	}
	order := orderRef.Value
	if order.Required {
		t.Error(`components.parameters.ListOrder must not be required, an absent "order" defaults to ascending`)
	}
	os := order.Schema.Value
	if len(os.Enum) != 2 || os.Enum[0] != "asc" || os.Enum[1] != "desc" {
		t.Errorf("ListOrder schema enum = %v, want [asc desc]", os.Enum)
	}
	if def, ok := os.Default.(string); !ok || def != "asc" {
		t.Errorf("ListOrder schema default = %v, want \"asc\"", os.Default)
	}

	for _, want := range sortableOps {
		parts := strings.SplitN(want, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", want)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", want)
			continue
		}

		have := queryParams(op)
		for _, p := range []string{"sort", "order"} {
			if !have[p] {
				t.Errorf("%s: query parameter %q is not documented", want, p)
			}
		}

		wantCol, ok := sortColumnByOp[want]
		if !ok {
			t.Fatalf("%s: no entry in sortColumnByOp, this test's own table is stale", want)
		}
		var sortParam, orderParam *openapi3.Parameter
		for _, p := range op.Parameters {
			if p.Value == nil || p.Value.In != "query" {
				continue
			}
			switch p.Value.Name {
			case "sort":
				sortParam = p.Value
			case "order":
				if p.Ref != "#/components/parameters/ListOrder" {
					t.Errorf("%s: order parameter must $ref components.parameters.ListOrder, got ref %q", want, p.Ref)
				}
				orderParam = p.Value
			}
		}
		if sortParam == nil {
			t.Errorf("%s: sort parameter not found among query parameters", want)
			continue
		}
		if orderParam == nil {
			t.Errorf("%s: order parameter not found among query parameters", want)
		}
		if sortParam.Schema == nil || sortParam.Schema.Value == nil {
			t.Errorf("%s: sort parameter has no schema", want)
			continue
		}
		ss := sortParam.Schema.Value
		if len(ss.Enum) != 1 || ss.Enum[0] != wantCol {
			t.Errorf("%s: sort schema enum = %v, want exactly [%s]", want, ss.Enum, wantCol)
		}
		if def, ok := ss.Default.(string); !ok || def != wantCol {
			t.Errorf("%s: sort schema default = %v, want %q", want, ss.Default, wantCol)
		}
	}
}

// searchParamHandlers returns the set of (*Server) method names whose body
// contains a call to the package-level searchParam function (paging.go), the
// marker that identifies a list handler offering
// docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4's ?q= substring
// search.
func searchParamHandlers(t *testing.T) map[string]bool {
	t.Helper()
	return handlersCalling(t, "searchParam")
}

// derivedSearchableRoutes mirrors derivedSortableRoutes exactly (same
// mechanism, different marker function): it cross-references the route table
// against searchParamHandlers, computing from source, independent of and not
// trusting searchableOps, the exact route set whose handler offers ?q=.
func derivedSearchableRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	handlers := searchParamHandlers(t)
	routes := map[string]bool{}
	for _, m := range handleWithHandlerRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		if handlers[handler] {
			routes[method+" "+path] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no searchable routes derived from package source, extraction is stale")
	}
	return routes
}

// searchableOps is the {method, path} set that MUST document `q` as a query
// parameter (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4). It is
// a SUBSET of sortableOps, deliberately: entitlements and
// artifact-entitlements are sortable (on role_id) but NOT searchable: Task
// 4's Decision 1 refuses ?q= on both, since role_id is a uuid with no
// natural text column of its own (refuseSearch, paging.go). The
// Enterprise-only entry (virtual-keys) is appended via
// enterpriseSearchableOps() rather than named here directly, mirroring
// sortableOps' own split for the same Community-tree reason.
var searchableOps = append([]string{
	"GET /v1/admin/servers",
	"GET /v1/admin/roles",
	"GET /v1/admin/artifacts",
}, enterpriseSearchableOps()...)

// TestSearchableOpsListIsExhaustive guards searchableOps against drift from
// what the code actually does, mirrors TestSortableOpsListIsExhaustive
// exactly (same shape, same reason: searchableOps is otherwise purely
// enumerative, so a handler gaining or losing a searchParam( call without a
// matching edit here would fail nothing in TestOpenAPIDocumentsSearchParam,
// which only ever looks at the routes the literal names).
func TestSearchableOpsListIsExhaustive(t *testing.T) {
	derived := derivedSearchableRoutes(t)
	want := map[string]bool{}
	for _, r := range searchableOps {
		want[r] = true
	}
	var missing, extra []string
	for r := range derived {
		if !want[r] {
			missing = append(missing, r)
		}
	}
	for r := range want {
		if !derived[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("handlers call searchParam( but are missing from searchableOps (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("searchableOps lists a route whose handler does not call searchParam( (%d):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// refuseSearchHandlers returns the set of (*Server) method names whose body
// contains a call to the package-level refuseSearch function (paging.go),
// the marker that identifies a list handler explicitly REJECTING ?q= (Task 4
// Decision 1) rather than silently ignoring it.
func refuseSearchHandlers(t *testing.T) map[string]bool {
	t.Helper()
	return handlersCalling(t, "refuseSearch")
}

// derivedRefuseSearchRoutes mirrors derivedSearchableRoutes for the opposite
// marker: the route set whose handler calls refuseSearch(.
func derivedRefuseSearchRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	handlers := refuseSearchHandlers(t)
	routes := map[string]bool{}
	for _, m := range handleWithHandlerRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		if handlers[handler] {
			routes[method+" "+path] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no refuse-search routes derived from package source, extraction is stale")
	}
	return routes
}

// refuseSearchOps is the {method, path} set whose handler must call
// refuseSearch(, the two role_id lists Task 4 Decision 1 refuses ?q= on.
// Guarded against drift the same way searchableOps is, by
// TestRefuseSearchOpsListIsExhaustive below, and disjoint from searchableOps
// by construction: no handler calls both searchParam( and refuseSearch(.
var refuseSearchOps = []string{
	"GET /v1/admin/entitlements",
	"GET /v1/admin/artifact-entitlements",
}

// TestRefuseSearchOpsListIsExhaustive guards refuseSearchOps against drift
// from what the code actually does, mirroring TestSearchableOpsListIsExhaustive.
func TestRefuseSearchOpsListIsExhaustive(t *testing.T) {
	derived := derivedRefuseSearchRoutes(t)
	want := map[string]bool{}
	for _, r := range refuseSearchOps {
		want[r] = true
	}
	var missing, extra []string
	for r := range derived {
		if !want[r] {
			missing = append(missing, r)
		}
	}
	for r := range want {
		if !derived[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("handlers call refuseSearch( but are missing from refuseSearchOps (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("refuseSearchOps lists a route whose handler does not call refuseSearch( (%d):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// TestOpenAPIDocumentsSearchParam is TestOpenAPIDocumentsSortOrderParams'
// sibling for ?q=: every op in searchableOps must document `q` as a query
// parameter, via a $ref to the shared components.parameters.ListSearch (not
// merely a same-named inline parameter that could independently diverge in
// shape: the same two-layer discipline TestOpenAPIPaginationParamsAreSharedAndCorrect
// established for limit/cursor and TestOpenAPIDocumentsSortOrderParams for
// order). The two refuseSearchOps routes must NOT document `q` at all: this
// API rejects the parameter outright rather than accepting and no-op'ing it,
// so advertising it as a valid query parameter would misdocument that.
func TestOpenAPIDocumentsSearchParam(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	searchRef, ok := doc.Components.Parameters["ListSearch"]
	if !ok || searchRef.Value == nil || searchRef.Value.Schema == nil {
		t.Fatal("components.parameters.ListSearch is missing or has no schema")
	}
	if searchRef.Value.Required {
		t.Error(`components.parameters.ListSearch must not be required: an absent "q" means no filter`)
	}

	for _, want := range searchableOps {
		parts := strings.SplitN(want, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", want)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", want)
			continue
		}
		if !queryParams(op)["q"] {
			t.Errorf("%s: query parameter \"q\" is not documented", want)
			continue
		}
		var found bool
		for _, p := range op.Parameters {
			if p.Value == nil || p.Value.In != "query" || p.Value.Name != "q" {
				continue
			}
			found = true
			if p.Ref != "#/components/parameters/ListSearch" {
				t.Errorf("%s: q parameter must $ref components.parameters.ListSearch, got ref %q", want, p.Ref)
			}
		}
		if !found {
			t.Errorf("%s: q parameter not found among query parameters", want)
		}
	}

	for _, refused := range refuseSearchOps {
		parts := strings.SplitN(refused, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", refused)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", refused)
			continue
		}
		if queryParams(op)["q"] {
			t.Errorf("%s: documents \"q\" as a query parameter, but this route REJECTS it with 400 "+
				"(Decision 1): documenting it as accepted would misdocument the real behavior", refused)
		}
	}
}

// guardedOps is the {method, path} set that MUST require an If-Match header
// (optimistic concurrency, design doc
// docs/specs/2026-08-11-orbeat-optimistic-concurrency-design.md §5) and
// document 412 + 428. It is a deliberate SUBSET of the mutating endpoints:
// the four DELETEs, submit/reject/withdraw, and rollback are all guarded by
// their OWN reasons documented in spec §2 (a DELETE is an explicit
// destructive act rather than a replay of stale field values; the
// non-approve transitions don't publish or freeze a snapshot; rollback
// already pins an explicit revision number) — none of that is visible from
// source, so it cannot be derived and is intentionally enumerative here,
// exactly like paginatedOps above. What CAN be derived from source is
// whether this literal still matches which handlers actually call ifMatch(
// — that's what TestPreconditionOpsListIsExhaustive checks below.
//
// The Enterprise-only entry (approve) is appended via enterpriseGuardedOps()
// rather than named here directly — see paginatedOps' comment above for why.
var guardedOps = append([]string{
	"PUT /v1/admin/servers/{id}",
	"PUT /v1/admin/artifacts/{id}",
	"PUT /v1/admin/entitlements/{id}",
	"PUT /v1/admin/roles/{id}",
}, enterpriseGuardedOps()...)

// handleWithHandlerRe extracts (METHOD, path, handler-method-name) from a
// mux.Handle registration of the form used by every admin route in api.go:
// mux.Handle("METHOD /path", admin(s.handlerName)) (or authed(...)).
var handleWithHandlerRe = regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)",\s*\w+\(s\.(\w+)\)\)`)

// handlersCalling returns the set of (*Server) method names whose body
// contains a call to the package-level function named fnName. Parsed via
// go/ast rather than a text/regex boundary heuristic (e.g. "next line
// starting with 'func'") so a multi-line signature or a nested closure
// cannot desync the scan the way a text heuristic could. Shared by
// pageParamsHandlers (marker: pageParams, paging.go) and ifMatchHandlers
// (marker: ifMatch, precondition.go) — same extraction, different marker
// function, so a change to the AST-walking logic cannot drift between the
// two gates.
func handlersCalling(t *testing.T, fnName string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	fset := token.NewFileSet()
	handlers := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil || len(fd.Recv.List) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			recvType, ok := star.X.(*ast.Ident)
			if !ok || recvType.Name != "Server" {
				continue
			}
			callsFn := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == fnName {
					callsFn = true
				}
				return true
			})
			if callsFn {
				handlers[fd.Name.Name] = true
			}
		}
	}
	if len(handlers) == 0 {
		t.Fatalf("no (*Server) method calling %s( found — extraction is stale", fnName)
	}
	return handlers
}

// pageParamsHandlers returns the set of (*Server) method names whose body
// contains a call to the package-level pageParams function (paging.go) — the
// marker that identifies a keyset-paginated list handler.
func pageParamsHandlers(t *testing.T) map[string]bool {
	t.Helper()
	return handlersCalling(t, "pageParams")
}

// ifMatchHandlers returns the set of (*Server) method names whose body
// contains a call to the package-level ifMatch function (precondition.go) —
// the marker that identifies an optimistic-concurrency-guarded mutation
// handler (spec §5).
func ifMatchHandlers(t *testing.T) map[string]bool {
	t.Helper()
	return handlersCalling(t, "ifMatch")
}

// derivedPaginatedRoutes cross-references the package's route table (via
// handleWithHandlerRe, which extracts the handler method name out of the
// mux.Handle("METHOD /path", admin(s.handlerName)) form every admin route
// uses) against pageParamsHandlers, computing from source — independent of
// and not trusting paginatedOps — the exact route set whose handler
// paginates.
func derivedPaginatedRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	handlers := pageParamsHandlers(t)
	routes := map[string]bool{}
	for _, m := range handleWithHandlerRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		if handlers[handler] {
			routes[method+" "+path] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no paginated routes derived from package source — extraction is stale")
	}
	return routes
}

// TestPaginatedOpsListIsExhaustive guards paginatedOps — the literal every
// other pagination test below iterates — against drift from what the code
// actually does. Without this, paginatedOps is purely enumerative: a new
// handler that calls pageParams( but is never added to the literal (or the
// YAML) fails NOTHING in either of the other two tests, because they only
// ever look at the routes the literal names.
//
// Red-proven: temporarily delete "GET /v1/admin/artifacts/{id}/revisions"
// from paginatedOps while ALSO stripping its limit/cursor params from the
// YAML — TestOpenAPIDocumentsPaginationParams and
// TestOpenAPIPaginationParamsAreSharedAndCorrect both stay green (they never
// look at a route that isn't in the literal); only this test, which derives
// its expectation from source instead of trusting the literal, fails.
func TestPaginatedOpsListIsExhaustive(t *testing.T) {
	derived := derivedPaginatedRoutes(t)
	want := map[string]bool{}
	for _, r := range paginatedOps {
		want[r] = true
	}
	var missing, extra []string
	for r := range derived {
		if !want[r] {
			missing = append(missing, r)
		}
	}
	for r := range want {
		if !derived[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("handlers call pageParams( but are missing from paginatedOps (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("paginatedOps lists a route whose handler does not call pageParams( (%d):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// derivedGuardedRoutes cross-references the package's route table against
// ifMatchHandlers, computing from source — independent of and not trusting
// guardedOps — the exact route set whose handler enforces the If-Match
// precondition.
func derivedGuardedRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := packageGoSource(t)
	handlers := ifMatchHandlers(t)
	routes := map[string]bool{}
	for _, m := range handleWithHandlerRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		if handlers[handler] {
			routes[method+" "+path] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no guarded routes derived from package source — extraction is stale")
	}
	return routes
}

// TestPreconditionOpsListIsExhaustive guards guardedOps — the literal
// TestOpenAPIDocumentsPreconditions iterates — against drift from what the
// code actually does. Mirrors TestPaginatedOpsListIsExhaustive exactly
// (same shape, same reason to exist): guardedOps is otherwise purely
// enumerative, so a handler gaining or losing an ifMatch( call without a
// matching edit here (and in the YAML) would fail NOTHING in
// TestOpenAPIDocumentsPreconditions, which only ever looks at the routes
// the literal names.
//
// This closes the blind spot both directions, and each was verified live
// (not just asserted in this comment — see the Task 9 commit message for
// the transcripts):
//   - ADD ifMatch( to a handler not in guardedOps (e.g. handleRejectArtifact)
//     without touching the YAML → this test fails ("missing from
//     guardedOps"), because derivedGuardedRoutes now contains a route
//     guardedOps does not.
//   - REMOVE ifMatch( from a guarded handler (e.g. handleApproveArtifact)
//     without touching guardedOps or the YAML → this test fails ("lists a
//     route whose handler does not call ifMatch("), because guardedOps now
//     names a route derivedGuardedRoutes no longer contains.
//
// Both directions are catchable ONLY because the expectation is derived
// from source rather than trusted as a literal — an allow-list on its own
// (guardedOps existing) gives none of this for free; it takes exactly this
// derive-and-diff test to make the allow-list self-checking.
func TestPreconditionOpsListIsExhaustive(t *testing.T) {
	derived := derivedGuardedRoutes(t)
	want := map[string]bool{}
	for _, r := range guardedOps {
		want[r] = true
	}
	var missing, extra []string
	for r := range derived {
		if !want[r] {
			missing = append(missing, r)
		}
	}
	for r := range want {
		if !derived[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("handlers call ifMatch( but are missing from guardedOps (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("guardedOps lists a route whose handler does not call ifMatch( (%d):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// resolveSchema returns ref's schema, following a $ref by name against the
// document's components if the loader has not already inlined ref.Value.
// kin-openapi's LoadFromData resolves same-document refs during load, so the
// fallback path is normally unreached — it exists so this test fails loudly
// (not with a nil-pointer panic) if that ever changes.
func resolveSchema(t *testing.T, doc *openapi3.T, ref *openapi3.SchemaRef) *openapi3.Schema {
	t.Helper()
	if ref == nil {
		t.Fatal("nil schema ref")
	}
	if ref.Value != nil {
		return ref.Value
	}
	name := strings.TrimPrefix(ref.Ref, "#/components/schemas/")
	sr, ok := doc.Components.Schemas[name]
	if !ok || sr.Value == nil {
		t.Fatalf("cannot resolve schema ref %q", ref.Ref)
	}
	return sr.Value
}

// formatFloatPtr renders a *float64 schema bound for an error message. A
// bare %v on the pointer itself prints an address, not the value it points
// to — this is the dereferenced, nil-safe form.
func formatFloatPtr(f *float64) string {
	if f == nil {
		return "<nil>"
	}
	return strconv.FormatFloat(*f, 'g', -1, 64)
}

// queryParams returns the names of op's `in: query` parameters.
func queryParams(op *openapi3.Operation) map[string]bool {
	have := map[string]bool{}
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.In == "query" {
			have[p.Value.Name] = true
		}
	}
	return have
}

// TestOpenAPIDocumentsPaginationParams exists because TestOpenAPICoversAllRoutes
// CANNOT catch a missing pagination parameter or envelope field: that test
// compares only the {method, path} set, and pagination added no new routes —
// so it stayed green whether or not limit/cursor/state/include/nextCursor
// were documented. This is the real gate.
//
// Red-proven with `go test -overlay` directly against the go:embed'd YAML:
// overlaying internal/api/openapi.yaml with a mutated scratch copy changes
// what `//go:embed openapi.yaml` embeds for that build, so no spec-injection
// seam of this test's own is needed (verified independently: deleting a path
// from an overlaid copy turns TestOpenAPICoversAllRoutes red too — the
// overlay reaches through the embed directive). Six mutations, each its own
// scratch copy: delete limit's $ref from one operation, cursor's $ref from a
// DIFFERENT operation, include from the artifacts list, state from the
// artifacts list, nextCursor from one response schema, limit from a
// different response schema — each fails naming exactly the missing thing,
// and TestOpenAPICoversAllRoutes stays green throughout every one (see the
// Task 9 commit message for the six transcripts).
func TestOpenAPIDocumentsPaginationParams(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	for _, want := range paginatedOps {
		parts := strings.SplitN(want, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", want)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", want)
			continue
		}

		have := queryParams(op)
		for _, p := range []string{"limit", "cursor"} {
			if !have[p] {
				t.Errorf("%s: query parameter %q is not documented", want, p)
			}
		}

		// The response envelope carries limit + nextCursor alongside the
		// list array (see e.g. admin_roles.go's writeJSON call) — a spec
		// whose response schema omits them is exactly as wrong as a missing
		// parameter, and TestOpenAPICoversAllRoutes cannot see it either.
		if op.Responses == nil {
			t.Errorf("%s: no responses documented", want)
			continue
		}
		respRef := op.Responses.Value("200")
		if respRef == nil || respRef.Value == nil {
			t.Errorf("%s: no 200 response documented", want)
			continue
		}
		mt := respRef.Value.Content.Get("application/json")
		if mt == nil || mt.Schema == nil {
			t.Errorf("%s: 200 response has no application/json schema", want)
			continue
		}
		schema := resolveSchema(t, doc, mt.Schema)
		for _, p := range []string{"limit", "nextCursor"} {
			if propRef, ok := schema.Properties[p]; !ok || propRef == nil {
				t.Errorf("%s: response schema is missing property %q", want, p)
			}
		}
	}

	// The artifacts list additionally documents state + include as query
	// params. state's four-value enum is not aspirational: migration
	// 00006_artifact_approval.sql puts a real CHECK on artifact.approval_state
	// restricting it to exactly these four, so the enum can never be an
	// under- or over-approximation of what's in the database. include is new
	// in Task 8 and IS validated by handleListArtifacts (unlike state, whose
	// own unrecognized-value handling stays lenient — see the NOTE on its
	// YAML description).
	item := doc.Paths.Find("/v1/admin/artifacts")
	if item == nil {
		t.Fatal("GET /v1/admin/artifacts: path not in spec")
	}
	op := item.Operations()["GET"]
	if op == nil {
		t.Fatal("GET /v1/admin/artifacts: operation not in spec")
	}
	have := queryParams(op)
	for _, p := range []string{"include", "state"} {
		if !have[p] {
			t.Errorf("GET /v1/admin/artifacts: query parameter %q is not documented", p)
		}
	}

	// The virtual keys list additionally documents revoked as a query param
	// (Enterprise only, but openapi.yaml is one shared document across both
	// editions -- see TestOpenAPICoversAllRoutes' knownEnterpriseRoutes).
	// revoked is new alongside include, so it is validated the same way
	// (handleListVirtualKeys, admin_virtual_keys.ee.go) rather than left
	// lenient like state.
	vkItem := doc.Paths.Find("/v1/admin/virtual-keys")
	if vkItem == nil {
		t.Fatal("GET /v1/admin/virtual-keys: path not in spec")
	}
	vkOp := vkItem.Operations()["GET"]
	if vkOp == nil {
		t.Fatal("GET /v1/admin/virtual-keys: operation not in spec")
	}
	if !queryParams(vkOp)["revoked"] {
		t.Error("GET /v1/admin/virtual-keys: query parameter \"revoked\" is not documented")
	}
}

// TestOpenAPIPaginationParamsAreSharedAndCorrect closes a gap
// TestOpenAPIDocumentsPaginationParams leaves open on its own: that test only
// checks a same-named parameter EXISTS at `in: query` — nothing about its
// shape. A same-named INLINE parameter with the wrong min/max/default/
// required would satisfy it silently.
//
// Red-proven: set components.parameters.ListLimit's schema to
// {minimum:3,maximum:5000,default:7,required:true} on a scratch copy — every
// one of those four contradicts paging.go's real semantics (limit must be
// >=1, clamps at maxListLimit=500, defaults to defaultListLimit=100, and is
// optional) — and TestOpenAPIDocumentsPaginationParams stays green throughout
// because "limit" still exists at `in: query` on all six operations.
//
// The fix is two-layered: (1) each op's limit/cursor parameter must be a
// $ref to the shared components.parameters.ListLimit/ListCursor, not an
// inline parameter that could independently diverge in shape; (2) the SHARED
// parameter is checked ONCE against the real constants, so raising
// maxListLimit in paging.go without touching the YAML fails the build
// instead of quietly drifting. Also pins the 400 response every paginated op
// must document (pageParams/?include validation errors 400) — nothing
// asserted that before this test either.
func TestOpenAPIPaginationParamsAreSharedAndCorrect(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	for _, want := range paginatedOps {
		parts := strings.SplitN(want, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", want)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", want)
			continue
		}
		for _, p := range op.Parameters {
			if p.Value == nil || p.Value.In != "query" {
				continue
			}
			switch p.Value.Name {
			case "limit":
				if p.Ref != "#/components/parameters/ListLimit" {
					t.Errorf("%s: limit parameter must $ref components.parameters.ListLimit, got ref %q", want, p.Ref)
				}
			case "cursor":
				if p.Ref != "#/components/parameters/ListCursor" {
					t.Errorf("%s: cursor parameter must $ref components.parameters.ListCursor, got ref %q", want, p.Ref)
				}
			}
		}
		if op.Responses == nil || op.Responses.Value("400") == nil {
			t.Errorf("%s: no 400 response documented (limit/cursor/include validation can fail)", want)
		}
	}

	limitRef, ok := doc.Components.Parameters["ListLimit"]
	if !ok || limitRef.Value == nil || limitRef.Value.Schema == nil || limitRef.Value.Schema.Value == nil {
		t.Fatal("components.parameters.ListLimit is missing or has no schema")
	}
	limit := limitRef.Value
	if limit.Required {
		t.Error(`components.parameters.ListLimit must not be required — an absent "limit" uses the default`)
	}
	ls := limit.Schema.Value
	if ls.Min == nil || *ls.Min != 1 {
		t.Errorf("ListLimit schema minimum = %s, want 1 (pageParams rejects <= 0 with 400)", formatFloatPtr(ls.Min))
	}
	if ls.Max == nil || *ls.Max != float64(maxListLimit) {
		t.Errorf("ListLimit schema maximum = %s, want %d (paging.go maxListLimit)", formatFloatPtr(ls.Max), maxListLimit)
	}
	if def, ok := ls.Default.(float64); !ok || int(def) != defaultListLimit {
		t.Errorf("ListLimit schema default = %v, want %d (paging.go defaultListLimit)", ls.Default, defaultListLimit)
	}

	cursorRef, ok := doc.Components.Parameters["ListCursor"]
	if !ok || cursorRef.Value == nil {
		t.Fatal("components.parameters.ListCursor is missing")
	}
	if cursorRef.Value.Required {
		t.Error(`components.parameters.ListCursor must not be required — an absent cursor means "first page"`)
	}
}

// TestOpenAPIDocumentsPreconditions exists for the same reason
// TestOpenAPIDocumentsPaginationParams does: TestOpenAPICoversAllRoutes
// compares only the {method, path} set, and guarding an existing route with
// If-Match added no new route — so that test stays green whether or not the
// precondition is documented at all. This is the real gate, for the three
// operations optimistic concurrency guards (design doc
// docs/specs/2026-08-11-orbeat-optimistic-concurrency-design.md §5).
//
// Two things are checked per operation, matching the two-layer shape
// TestOpenAPIPaginationParamsAreSharedAndCorrect established for
// limit/cursor: (1) an If-Match header parameter exists at all, and (2) it
// is a $ref to the shared components.parameters.IfMatch, not an inline
// parameter that could independently diverge in shape (wrong `required`,
// wrong description, etc.) while still satisfying a same-named-parameter
// check. Also asserts both 412 and 428 are documented — a spec that
// requires the header but never says what happens when it's absent or
// stale is not a usable contract.
//
// Red-proven with `go test -overlay` directly against the go:embed'd YAML
// (mirrors TestOpenAPIDocumentsPaginationParams's proof — the overlay
// reaches through the //go:embed openapi.yaml directive, so no
// spec-injection seam of this test's own is needed). Five mutations, each
// its own scratch copy — see the Task 9 commit message for the full
// transcripts:
//  1. delete the If-Match $ref from PUT /v1/admin/servers/{id}'s parameters
//     → fails naming exactly that operation ("no If-Match header parameter
//     documented"); TestOpenAPICoversAllRoutes stays green.
//  2. delete the 412 response from PUT /v1/admin/artifacts/{id}
//     → fails naming exactly that operation ("no 412 response documented").
//  3. delete the 428 response from POST /v1/admin/artifacts/{id}/approve
//     → fails naming exactly that operation ("no 428 response documented").
//  4. replace PUT /v1/admin/artifacts/{id}'s If-Match $ref with an inline
//     copy of the identical parameter (same name/in/required/schema)
//     → THIS test still fails ("must $ref components.parameters.IfMatch"),
//     because it checks the ref, not just parameter-shape equivalence — an
//     inline copy that happens to match today can silently diverge
//     tomorrow the way TestOpenAPIPaginationParamsAreSharedAndCorrect's
//     ListLimit mutation demonstrated for pagination.
//  5. remove POST /v1/admin/artifacts/{id}/approve from guardedOps while
//     ALSO stripping its If-Match parameter and 412/428 responses from the
//     YAML → THIS test and the mutations above stay green (they never look
//     at a route that isn't in guardedOps); only TestPreconditionOpsListIsExhaustive,
//     which derives its expectation from source instead of trusting the
//     literal, fails.
func TestOpenAPIDocumentsPreconditions(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	for _, want := range guardedOps {
		parts := strings.SplitN(want, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", want)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", want)
			continue
		}

		found := false
		for _, p := range op.Parameters {
			if p.Value == nil || p.Value.In != "header" || p.Value.Name != "If-Match" {
				continue
			}
			found = true
			if p.Ref != "#/components/parameters/IfMatch" {
				t.Errorf("%s: If-Match parameter must $ref components.parameters.IfMatch, got ref %q", want, p.Ref)
			}
		}
		if !found {
			t.Errorf("%s: no If-Match header parameter documented", want)
		}

		if op.Responses == nil || op.Responses.Value("412") == nil {
			t.Errorf("%s: no 412 response documented", want)
		}
		if op.Responses == nil || op.Responses.Value("428") == nil {
			t.Errorf("%s: no 428 response documented", want)
		}
	}
}

// TestOpenAPIDocumentsRateLimit exists for the same reason
// TestOpenAPIDocumentsPaginationParams and TestOpenAPIDocumentsPreconditions
// do: TestOpenAPICoversAllRoutes compares only the {method, path} set, and
// wiring the limiter into an existing route adds no new route — so that test
// stays green whether or not the 429 is documented at all. This is the real
// gate, for spec §8's "OpenAPI documents 429 on every limited route"
// acceptance criterion.
//
// The limited-route set is derived from source (derivedLimitedRoutes,
// ratelimit_test.go), mirroring guardedOps/paginatedOps' derive-from-source
// discipline instead of a second hand-maintained literal that could drift
// from the first.
func TestOpenAPIDocumentsRateLimit(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi spec: %v", err)
	}

	for route := range derivedLimitedRoutes(t) {
		parts := strings.SplitN(route, " ", 2)
		method, path := parts[0], parts[1]
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("%s: path not in spec", route)
			continue
		}
		op := item.Operations()[method]
		if op == nil {
			t.Errorf("%s: operation not in spec", route)
			continue
		}
		if op.Responses == nil {
			t.Errorf("%s: no responses documented", route)
			continue
		}
		resp := op.Responses.Value("429")
		if resp == nil || resp.Value == nil {
			t.Errorf("%s: no 429 response documented", route)
			continue
		}
		if _, ok := resp.Value.Headers["Retry-After"]; !ok {
			t.Errorf("%s: 429 response does not document a Retry-After header", route)
		}
	}
}

// ---- DTO-to-schema parity ----

// scalarSchemaTypes maps the Go kinds this gate can check to the OpenAPI type
// they must be documented as. Kinds that are NOT here (struct, slice, map,
// interface) are checked for presence only: time.Time is a formatted string,
// json.RawMessage is an array of another schema, and a nested DTO is a $ref,
// so a kind-to-type rule for them would be three special cases pretending to
// be one rule.
var scalarSchemaTypes = map[reflect.Kind]string{
	reflect.String:  "string",
	reflect.Bool:    "boolean",
	reflect.Int:     "integer",
	reflect.Int64:   "integer",
	reflect.Float64: "number",
}

// dtoJSONFields returns the json field name of every field a DTO struct
// marshals, mapped to that field's kind with pointers dereferenced.
//
// Derived by reflection, never from a list written here: a hand-maintained
// copy of a struct's fields is the same defect one level up, and it would go
// stale on the very edit this gate exists to catch.
func dtoJSONFields(t *testing.T, dto any) map[string]reflect.Kind {
	t.Helper()
	rt := reflect.TypeOf(dto)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("dtoJSONFields wants a struct, got %s", rt.Kind())
	}
	out := map[string]reflect.Kind{}
	for i := range rt.NumField() {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		if tag == "" {
			t.Fatalf("%s.%s has no json tag, so this gate cannot know what the wire name is",
				rt.Name(), f.Name)
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		out[name] = ft.Kind()
	}
	return out
}

// assertSchemaMatchesDTO is the parity check: the named component schema's
// property set must equal the DTO's json field set, in BOTH directions, and
// every scalar property must be documented as the type the Go field marshals
// to.
//
// Both directions matter for different failures. Missing means a field ships
// undocumented, which is what an added field does by default. Extra means the
// schema still advertises a field the code stopped sending, which is the
// quieter one: a client written against the document gets `undefined` at
// runtime and the document never complains.
//
// What this does NOT check: `required`, enums, descriptions, and the types of
// non-scalar fields (see scalarSchemaTypes). It is parity of the field set and
// of the scalar types, not a full contract test, and a schema can still be
// wrong in ways it cannot see.
func assertSchemaMatchesDTO(t *testing.T, schemaName string, dto any) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("load openapi.yaml: %v", err)
	}
	ref, ok := doc.Components.Schemas[schemaName]
	if !ok {
		t.Fatalf("openapi.yaml has no components.schemas.%s", schemaName)
	}
	schema := resolveSchema(t, doc, ref)
	fields := dtoJSONFields(t, dto)

	var missing, extra []string
	for name := range fields {
		if _, documented := schema.Properties[name]; !documented {
			missing = append(missing, name)
		}
	}
	for name := range schema.Properties {
		if _, sent := fields[name]; !sent {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%T fields absent from the %s schema in openapi.yaml (%d): %s",
			dto, schemaName, len(missing), strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("%s schema properties no %T field sends (%d): %s",
			schemaName, dto, len(extra), strings.Join(extra, ", "))
	}

	for name, kind := range fields {
		want, checkable := scalarSchemaTypes[kind]
		prop := schema.Properties[name]
		if !checkable || prop == nil || prop.Ref != "" || prop.Value == nil || prop.Value.Type == nil {
			continue
		}
		if !prop.Value.Type.Is(want) {
			t.Errorf("%s.%s is documented as %v but the Go field is a %s, which marshals to %s",
				schemaName, name, prop.Value.Type.Slice(), kind, want)
		}
	}
}

// TestOpenAPIArtifactSchemaMatchesDTO closes the gap this file's other gates
// leave open. Everything above derives ROUTES, parameters, headers and
// response codes from source; nothing has ever compared a response BODY to the
// schema documenting it, so adding a field to artifactDTO has always been a
// silent documentation drift with no CI signal at all.
//
// It is scoped to the artifact schemas rather than every DTO in the package
// because that is the surface this change touches. The helper is general; the
// remaining schemas are not enrolled here.
//
// syncArtifactDTO is enrolled because it is the one artifact body a machine
// parses rather than a person reads. It gained id and revision so a developer's
// install can name the version it holds, and both fields are unconditional in
// both editions, which makes the schema a contract a client generator will
// build against rather than prose.
func TestOpenAPIArtifactSchemaMatchesDTO(t *testing.T) {
	assertSchemaMatchesDTO(t, "Artifact", artifactDTO{})
	assertSchemaMatchesDTO(t, "ArtifactRoleGrants", artifactRoleGrantsDTO{})
	assertSchemaMatchesDTO(t, "SyncArtifact", syncArtifactDTO{})
}

// TestOpenAPIMeSchemaMatchesDTO closes the same gap for GET /v1/me that
// TestOpenAPIArtifactSchemaMatchesDTO closes for the artifact endpoints:
// TestOpenAPICoversAllRoutes only ever compared the {method, path} set, and
// /v1/me already existed there, so adding the "features" field to
// meResponseDTO (me.go) would otherwise be a silent documentation drift with
// no CI signal at all — exactly the risk this slice's own design note names
// ("a wire change with a stale spec is the drift this gate exists to stop").
//
// MeFeatures is enrolled separately from MeResponse for the same reason
// ArtifactRoleGrants is enrolled separately from Artifact above: "features"
// is a nested object (dtoJSONFields records its Kind as reflect.Struct, which
// scalarSchemaTypes deliberately does not type-check — see its own comment),
// so only a second, direct assertSchemaMatchesDTO call on meFeaturesDTO
// itself checks that "pinning" is documented as a boolean.
func TestOpenAPIMeSchemaMatchesDTO(t *testing.T) {
	assertSchemaMatchesDTO(t, "MeResponse", meResponseDTO{})
	assertSchemaMatchesDTO(t, "MeFeatures", meFeaturesDTO{})
}
