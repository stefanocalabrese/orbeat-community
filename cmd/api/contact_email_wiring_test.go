package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunWiresContactEmail statically asserts that run() passes
// cfg.ContactEmail, specifically, to BOTH resolver.SetContactEmail and
// srv.SetContactEmail (Task 6, docs/plans/orbeat-community-caps-2026-08-19.md):
// the seat cap's 402 is written by internal/authz, the server/role caps'
// 402 by internal/api, so missing either leaves one of the three caps
// pointing at authz.DefaultContactEmail while the other two honour the
// operator's override.
//
// The walk is scoped to run()'s *ast.FuncDecl, not the whole file, so a
// mutant that relocates the two calls into a function run() never invokes
// is caught: an earlier version of this test scanned the whole file and
// stayed green under that mutant despite its own name and doc comment
// claiming run() makes the calls.
//
// The argument check requires the literal selector cfg.ContactEmail, not
// merely that SetContactEmail was called with one argument of any kind.
// An earlier version of this test only checked argument count, so
// resolver.SetContactEmail("") and srv.SetContactEmail("") both passed it
// while producing exactly the silently-wrong-default outcome the test's own
// failure messages warn about: a call that satisfies the assertion's letter
// while keeping the bug.
//
// Known limitation, accepted rather than engineered around: this matches
// only the literal identifiers cfg, resolver and srv as currently spelled
// in run(). It does not resolve aliases (e.g. c := cfg; c.ContactEmail, or
// r := resolver; r.SetContactEmail(...) would not be recognised), so a
// rename or alias of any of the three inside run() makes this test fail
// even on behaviourally-correct code. main.go's local-variable names are
// hand-written and stable, not generated, so a rename is a deliberate edit
// a person will make consciously, and having it also require touching this
// test is an acceptable cost for a check that a plain argument-count
// assertion cannot make.
//
// A live boot would be the more direct proof, but run() dials the DB
// (migrate) and then Keycloak's OIDC discovery (auth.NewValidator) before
// either call is reached, and neither is available to this package's tests,
// as TestRunProceedsPastWellFormedRefToMigrate already establishes: run()
// cannot get past "migrate" without a real ORBEAT_DB_URL.
//
// NOTHING observes the configured address in a real 402 body today, and
// make smoke is NOT that gate. This repo is the Enterprise tree, where
// communityLimits() returns editionLimits{} (limits.ee.go) and
// communitySeatLimit() returns 0 (seatlimit.ee.go), so every cap check
// short-circuits on max <= 0 before it reads the store. No 402 can be
// produced at all in the stack make smoke boots, and scripts/smoke.sh has
// zero references to 402 or ORBEAT_CONTACT_EMAIL. An earlier version of
// this comment claimed smoke "exercises the real path end to end", which
// is the v1.14.1 pattern: a compensating control named as coverage that
// stops before the seam. What this test proves is the two call sites; the
// first observable proof is Task 7's generated-Community gate, which
// portal/src/api/client.limit.test.tsx already points at.
//
// This test parses the actual
// checked-in main.go from disk, not through go test -overlay (which only
// substitutes what the compiler sees), so an edit to the real source trips
// it: red-proven by editing main.go and reverting.
func TestRunWiresContactEmail(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	runFn := findFuncDecl(file, "run")
	if runFn == nil {
		t.Fatal("main.go has no func run()")
	}

	var sawResolverSet, sawServerSet bool
	ast.Inspect(runFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetContactEmail" {
			return true
		}
		if !isCfgContactEmail(call.Args[0]) {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch recv.Name {
		case "resolver":
			sawResolverSet = true
		case "srv":
			sawServerSet = true
		}
		return true
	})

	if !sawResolverSet {
		t.Error("run() does not call resolver.SetContactEmail(cfg.ContactEmail), so the seat cap's 402 would stay pointed at authz.DefaultContactEmail")
	}
	if !sawServerSet {
		t.Error("run() does not call srv.SetContactEmail(cfg.ContactEmail), so the server/role caps' 402 would stay pointed at authz.DefaultContactEmail")
	}
}

// isCfgContactEmail reports whether expr is exactly the selector expression
// cfg.ContactEmail (a field access, not a call: ContactEmail is a plain
// string field on config.Config, not a method).
func isCfgContactEmail(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ContactEmail" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "cfg"
}

// findFuncDecl returns the top-level function declaration named name, or
// nil if main.go has none by that name.
func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
