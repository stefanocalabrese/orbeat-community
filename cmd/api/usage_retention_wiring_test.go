package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunStartsAndStopsUsageRetention statically asserts that run() both
// STARTS the usage retention loop and STOPS it on the way out. Mirrors
// TestRunStartsAndStopsDeploymentRetention (deployment_retention_wiring_test.go)
// exactly, for the identical reason: a startUsageRetention with no call
// site leaves ORBEAT_USAGE_RETENTION_DAYS inert, so usage_daily rows
// accumulate forever while every test in internal/api and internal/store
// stays green, because each of them proves its own piece and none of them
// proves the piece is reached.
//
// The stop half is asserted separately and is not decoration: without it
// the loop's goroutine outlives an otherwise-graceful shutdown, the same
// ordering concern stopDeploymentRetention and stopRetention both exist to
// get right.
//
// Both halves are asserted inside run()'s *ast.FuncDecl, not over the whole
// file, so a mutant that moves either call into a helper run() never
// invokes is caught.
//
// It parses the checked-in main.go from disk rather than through
// `go test -overlay`, which substitutes only what the compiler sees, so the
// red-proof is an edit to the real source and a revert.
//
// Known limitation, the same one TestRunStartsAndStopsDeploymentRetention
// accepts: this matches the literal identifier startUsageRetention and the
// literal local name stopUsageRetention as currently spelled in run(). A
// rename makes this test fail on behaviourally-correct code, which is an
// acceptable cost for a check that a plain "the function exists" assertion
// cannot make.
func TestRunStartsAndStopsUsageRetention(t *testing.T) {
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
		case "startUsageRetention":
			started = true
		case "stopUsageRetention":
			stopped = true
		}
		return true
	})

	if !started {
		t.Error("run() never calls startUsageRetention, so ORBEAT_USAGE_RETENTION_DAYS is inert and " +
			"usage_daily rows are never pruned")
	}
	if !stopped {
		t.Error("run() never calls stopUsageRetention, so the prune goroutine outlives shutdown and an " +
			"in-flight batched DELETE is killed rather than cancelled cleanly")
	}
}
