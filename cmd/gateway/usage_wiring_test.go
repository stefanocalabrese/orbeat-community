package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestRunWiresUsageMetering statically asserts that run() installs BOTH the
// usage counter and the quota enforcer, and flushes the counter on shutdown
// -- three assertions in one test rather than three, because they all guard
// the same failure mode and Task 5's own brief calls this "the wiring gate
// that proves the wiring": the virtual-keys slice shipped nine tasks of
// tested code connected to nothing, found by a documentation task rather
// than any gate, because cmd/api never called SetDCRClient. Every piece
// this test covers (usage.ee.go's UsageCounter, quota.ee.go's
// QuotaEnforcer, rbac_middleware.go's count/check hooks) already has its
// own passing unit tests; none of them prove run() actually reaches
// gw.SetUsageCounter/SetQuotaEnforcer or that a stopped gateway persists
// what it counted.
//
// Mirrors cmd/gateway/intercept_wiring_test.go's TestRunWiresInterceptor and
// cmd/api/deployment_retention_wiring_test.go's
// TestRunStartsAndStopsDeploymentRetention in shape and in the reasons
// stated on both: scoped to run()'s *ast.FuncDecl, not the whole file, so a
// mutant that moves any of the three calls into a helper run() never
// invokes is still caught; parses the checked-in main.go from disk rather
// than through `go test -overlay` (which only substitutes what the
// compiler sees), so the red-proof below is a real edit to the source and a
// revert, not a build substitution.
//
// THE SHUTDOWN-FLUSH ASSERTION RELIES ON A NAMING CONVENTION THAT MAKES IT
// UNAMBIGUOUS BY CONSTRUCTION, not a heuristic: the only uc.Flush(...) call
// visible to an AST walk of run() IS the deliberate shutdown call, because
// the periodic ticker's own Flush call lives inside
// internal/gateway.RunUsageTicker -- a different function, in a different
// file and package, invisible to this scan. See RunUsageTicker's own doc
// comment (internal/gateway/usage_ticker.go) for why that split was chosen
// deliberately, not incidentally.
//
// Known limitation, the same one TestRunWiresInterceptor and
// TestRunStartsAndStopsDeploymentRetention both accept: this matches the
// literal receiver names gw/uc/qe and the literal method names
// SetUsageCounter/SetQuotaEnforcer/Flush as currently spelled in run(). A
// rename of any of them makes this test fail on behaviourally-correct code,
// which is an acceptable cost for a check that a plain "the function
// exists" assertion cannot make.
func TestRunWiresUsageMetering(t *testing.T) {
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

	var wiresCounter, wiresQuota, flushesOnShutdown bool
	ast.Inspect(runFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case recv.Name == "gw" && sel.Sel.Name == "SetUsageCounter":
			wiresCounter = true
		case recv.Name == "gw" && sel.Sel.Name == "SetQuotaEnforcer":
			wiresQuota = true
		case recv.Name == "uc" && sel.Sel.Name == "Flush":
			flushesOnShutdown = true
		}
		return true
	})

	if !wiresCounter {
		t.Error("run() never calls gw.SetUsageCounter, so no usage_daily row is ever written on any " +
			"deployment built from this tree, exactly the gap this test exists to close")
	}
	if !wiresQuota {
		t.Error("run() never calls gw.SetQuotaEnforcer, so ORBEAT-configured role quotas are never " +
			"enforced on any deployment built from this tree")
	}
	if !flushesOnShutdown {
		t.Error("run() never calls uc.Flush, so a stopped gateway silently loses up to one flush " +
			"interval's worth of usage -- the difference between a quota that is approximately right " +
			"and one that drifts down forever")
	}
}
