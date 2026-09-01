package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunWiresDCRClient statically asserts that run() calls
// srv.SetDCRClient, closing the exact gap this repo has now shipped twice:
// a control that exists, is unit tested end to end (rbac.KeyToolAllowed,
// the virtual_key store, the gateway session fork, the admin routes, the
// portal console), and is wired to nothing. internal/api.Server.SetDCRClient
// existed with dcrRegister/dcrDelete permanently nil because cmd/api never
// called it -- POST /v1/admin/virtual-keys refused 100% of requests on every
// deployment built from this tree, and every test in internal/api and
// internal/keycloak stayed green throughout, because each of them proves
// its own piece and none of them proves the piece is reached from a real
// binary's startup path. See TestRunStartsAndStopsDeploymentRetention's own
// doc comment (deployment_retention_wiring_test.go) for the prior instance
// of this exact defect class in this same file.
//
// Scoped to run()'s *ast.FuncDecl, not the whole file, for the identical
// reason TestRunWiresContactEmail's doc comment gives: a mutant that moves
// the call into a function run() never invokes must still be caught.
//
// This checks only that the call exists, not the identity of its arguments
// (unlike TestRunWiresContactEmail's isCfgContactEmail check). That
// asymmetry is deliberate, not a shortcut: SetDCRClient's own nil-ignore
// contract (internal/api/api.go) makes any single argument choice here
// safe by construction -- passing buildDCRClient's own two return values,
// nil or not, can never wipe a previously-installed pair or half-install
// one -- so there is no "satisfies the assertion's letter while keeping the
// bug" shape an argument-identity check would need to rule out here the
// way ContactEmail's silent-empty-string footgun required one.
//
// It parses the checked-in main.go from disk rather than through
// `go test -overlay` (which only substitutes what the compiler sees), so
// the red-proof is an edit to the real source and a revert. Reproduced
// live before this file was committed: replacing the
// `srv.SetDCRClient(dcrRegister, dcrDelete)` line in main.go with
// `_ = dcrRegister; _ = dcrDelete` and running
// `go test ./cmd/api/ -run TestRunWiresDCRClient -v` produced
//
//	dcr_client_wiring_test.go:99: run() never calls srv.SetDCRClient, so ORBEAT_DCR_CLIENT_ID
//	    is inert and POST /v1/admin/virtual-keys refuses every request via its own nil-registrar
//	    branch on every deployment, exactly the gap this test exists to close
//	--- FAIL: TestRunWiresDCRClient (0.00s)
//
// then main.go was restored from a pre-edit copy; `diff` against that copy
// was empty and the suite was green again afterward.
//
// Known limitation, the same one TestRunWiresContactEmail and
// TestRunStartsAndStopsDeploymentRetention both accept: this matches the
// literal identifier SetDCRClient called on the literal receiver srv, as
// currently spelled in run(). A rename makes this test fail on
// behaviourally-correct code, an acceptable cost for a check that a plain
// "the function exists" assertion cannot make.
//
// A live boot would be the more direct proof and is not available here:
// run() dials the database (migrate) before it reaches this line, as
// TestRunProceedsPastWellFormedRefToMigrate already establishes.
func TestRunWiresDCRClient(t *testing.T) {
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

	var wired bool
	ast.Inspect(runFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetDCRClient" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "srv" {
			return true
		}
		wired = true
		return true
	})

	if !wired {
		t.Error("run() never calls srv.SetDCRClient, so ORBEAT_DCR_CLIENT_ID is inert and " +
			"POST /v1/admin/virtual-keys refuses every request via its own nil-registrar branch " +
			"on every deployment, exactly the gap this test exists to close")
	}
}
