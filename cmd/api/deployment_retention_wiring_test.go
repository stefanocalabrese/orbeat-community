package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunStartsAndStopsDeploymentRetention statically asserts that run()
// both STARTS the deployment retention loop and STOPS it on the way out.
//
// The failure it exists to catch is the one this repo has shipped before: a
// control that exists, is unit tested, and is wired to nothing. A
// startDeploymentRetention with no call site leaves ORBEAT_DEPLOYMENT_RETENTION_DAYS
// inert, so rows about people's machines accumulate forever while every test
// in internal/api and internal/store stays green, because each of them proves
// its own piece and none of them proves the piece is reached.
//
// The stop half is asserted separately and is not decoration: without it the
// loop's goroutine outlives an otherwise-graceful shutdown, which is exactly
// the ordering audit retention's own stopRetention exists to get right (an
// in-flight batched DELETE is cancelled cleanly rather than killed mid
// teardown).
//
// Both halves are asserted inside run()'s *ast.FuncDecl, not over the whole
// file, so a mutant that moves either call into a helper run() never invokes
// is caught. That scoping is the correction TestRunWiresContactEmail's doc
// comment records making after its own first version scanned the whole file.
//
// It parses the checked-in main.go from disk rather than through
// `go test -overlay`, which substitutes only what the compiler sees, so the
// red-proof is an edit to the real source and a revert.
//
// Known limitation, the same one TestRunWiresContactEmail accepts: this
// matches the literal identifier startDeploymentRetention and the literal
// local name stopDeploymentRetention as currently spelled in run(). A rename
// makes this test fail on behaviourally-correct code, which is an acceptable
// cost for a check that a plain "the function exists" assertion cannot make.
//
// A live boot would be the more direct proof and is not available here:
// run() dials the database (migrate) before it reaches either line, as
// TestRunProceedsPastWellFormedRefToMigrate already establishes.
func TestRunStartsAndStopsDeploymentRetention(t *testing.T) {
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

	var started, stopped bool
	ast.Inspect(runFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "startDeploymentRetention":
			started = true
		case "stopDeploymentRetention":
			stopped = true
		}
		return true
	})

	if !started {
		t.Error("run() never calls startDeploymentRetention, so ORBEAT_DEPLOYMENT_RETENTION_DAYS " +
			"is inert and artifact_deployment rows about people's machines are never pruned")
	}
	if !stopped {
		t.Error("run() never calls stopDeploymentRetention, so the prune goroutine outlives shutdown " +
			"and an in-flight batched DELETE is killed rather than cancelled cleanly")
	}
}
