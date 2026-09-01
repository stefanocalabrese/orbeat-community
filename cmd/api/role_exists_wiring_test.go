package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunWiresRoleExistsChecker statically asserts that run() calls
// srv.SetRoleExistsChecker, mirroring TestRunWiresDCRClient
// (dcr_client_wiring_test.go) for the exact same defect class: a control
// declared in internal/api (Server.SetRoleExistsChecker, Task 4 of
// docs/plans/orbeat-role-rename-2026-08-27.md) and unit-tested end to end
// (admin_roles_update_test.go) but reachable from no real binary's startup
// path, because cmd/api never called it. Left unwired, handleUpdateRole's
// nil-roleExists fallback (the bare operator-assertion path) would be the
// ONLY path any deployment ever exercises, even one that configured
// ORBEAT_DCR_CLIENT_ID specifically to get the identity-provider lookup --
// silently downgrading a verified check to an honor-system checkbox on
// every real binary, exactly the shape TestAllServerInstallersAreWiredOrExempt's
// own doc comment names ("shipped nine fully tested tasks that were inert").
//
// Scoped to run()'s *ast.FuncDecl, not the whole file, for the same reason
// TestRunWiresDCRClient's doc comment gives: a mutant that moves the call
// into a function run() never invokes must still be caught.
//
// This checks only that the call exists, not the identity of its argument:
// buildDCRClient returns a SINGLE checkRoleExists value (nil when
// ORBEAT_DCR_CLIENT_ID is unset, the real dcrClient.RealmRoleExists bound
// method value otherwise), and SetRoleExistsChecker's own nil-ignore
// contract (internal/api/api.go) makes passing it through, nil or not,
// safe by construction -- there is no second value here that could be
// half-installed or wiped, the identical reasoning TestRunWiresDCRClient's
// own doc comment gives for skipping an argument-identity check.
//
// It parses the checked-in main.go from disk rather than through
// `go test -overlay` (which only substitutes what the compiler sees), so
// the red-proof is an edit to the real source and a revert. Reproduced
// live before this file was committed: replacing the
// `srv.SetRoleExistsChecker(checkRoleExists)` line in main.go with
// `_ = checkRoleExists` and running
// `go test ./cmd/api/ -run TestRunWiresRoleExistsChecker -v` produced
//
//	role_exists_wiring_test.go:66: run() never calls srv.SetRoleExistsChecker, so a
//	    deployment with ORBEAT_DCR_CLIENT_ID configured never gets the identity-provider
//	    rename check and silently falls back to the operator's bare assertion on every
//	    real binary, exactly the gap this test exists to close
//	--- FAIL: TestRunWiresRoleExistsChecker (0.00s)
//
// then main.go was restored from a pre-edit copy; `diff` against that copy
// was empty and the suite was green again afterward.
//
// Known limitation, the same one TestRunWiresDCRClient accepts: this
// matches the literal identifier SetRoleExistsChecker called on the
// literal receiver srv, as currently spelled in run(). A rename makes this
// test fail on behaviourally-correct code, an acceptable cost for a check
// that a plain "the function exists" assertion cannot make.
//
// A live boot would be the more direct proof and is not available here:
// run() dials the database (migrate) before it reaches this line, as
// TestRunProceedsPastWellFormedRefToMigrate already establishes.
func TestRunWiresRoleExistsChecker(t *testing.T) {
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
		if !ok || sel.Sel.Name != "SetRoleExistsChecker" {
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
		t.Error("run() never calls srv.SetRoleExistsChecker, so a deployment with " +
			"ORBEAT_DCR_CLIENT_ID configured never gets the identity-provider rename check " +
			"and silently falls back to the operator's bare assertion on every real binary, " +
			"exactly the gap this test exists to close")
	}
}
