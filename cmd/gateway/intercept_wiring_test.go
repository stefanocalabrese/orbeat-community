package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
)

// TestRunWiresInterceptor statically asserts that run() calls
// gw.SetInterceptor, mirroring cmd/api/dcr_client_wiring_test.go's
// TestRunWiresDCRClient -- the precedent this gate follows because the
// virtual-keys slice shipped nine fully tested tasks that were inert for
// exactly this reason: cmd/api never called SetDCRClient, and nothing caught
// it until the documentation task noticed by hand, not any gate. Task 3's
// own tests (internal/gateway/intercept_test.go) prove
// interceptResult/interceptArguments behave correctly ONCE INSTALLED; this
// proves run() actually installs them.
//
// Scoped to run()'s *ast.FuncDecl, not the whole file, for the same reason
// TestRunWiresDCRClient gives: a mutant that moves the call into a function
// run() never invokes must still be caught.
//
// It parses the checked-in main.go from disk rather than through
// `go test -overlay` (which only substitutes what the compiler sees), so the
// red-proof below is a real edit to the source and a revert, not a build
// substitution.
//
// Reproduced live before this file was committed: replacing
// `gw.SetInterceptor(interceptorFor(cfg, govern.NewDefaultScanner()))` in
// main.go with `_ = interceptorFor(cfg, govern.NewDefaultScanner())` and
// running `go test ./cmd/gateway/ -run TestRunWiresInterceptor -v` failed
// naming the consequence, then main.go was restored and diffed clean against
// the pre-edit copy.
func TestRunWiresInterceptor(t *testing.T) {
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
		if !ok || sel.Sel.Name != "SetInterceptor" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "gw" {
			return true
		}
		wired = true
		return true
	})

	if !wired {
		t.Error("run() never calls gw.SetInterceptor, so ORBEAT_INTERCEPT is inert and no " +
			"tool call argument or result is ever scanned on any deployment built from this " +
			"tree, exactly the gap this test exists to close")
	}
}

// findFuncDecl returns the top-level function declaration named name, or nil
// if main.go has none by that name. Mirrors
// cmd/api/contact_email_wiring_test.go's helper of the same name --
// duplicated rather than shared because cmd/api and cmd/gateway are separate
// `main` packages, and Go does not let one import the other.
func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// poisonScanner is a govern.Scanner whose Scan fails the test the instant it
// is called. It exists to make TestInterceptorForOffNeverInvokesScanner an
// actual proof that the scanner is never REACHED when ORBEAT_INTERCEPT is
// unset, rather than a weaker assertion that would stay green even if the
// wiring installed a real, working scanner that simply had nothing to find
// in this particular test's input -- "called-and-ignored" would pass that
// weaker version; it cannot pass this one.
type poisonScanner struct{ t *testing.T }

func (p poisonScanner) Scan(context.Context, govern.ArtifactPayload) ([]govern.Finding, error) {
	p.t.Fatal("interceptor scanner was invoked despite ORBEAT_INTERCEPT being unset -- " +
		"'off' must mean the scanner is never reached, not called-and-ignored")
	return nil, nil
}

// TestInterceptorForOffNeverInvokesScanner is Task 4's off-path gate
// (design spec §4: "ORBEAT_INTERCEPT unset means the code path does not run
// at all"). interceptorFor is the exact decision main.go's run() feeds
// straight into gw.SetInterceptor (TestRunWiresInterceptor above pins that
// wiring); this drives that same function with a poison scanner standing in
// for govern.NewDefaultScanner(), and, if the decision leaked it through
// anyway, invokes it exactly as internal/gateway would -- so a wiring
// regression fails naming the actual scanner call, not just an == nil check
// that a reader could mistake for pedantry.
func TestInterceptorForOffNeverInvokesScanner(t *testing.T) {
	poison := poisonScanner{t: t}
	got := interceptorFor(config.Config{Intercept: ""}, poison)
	if got == nil {
		return // correct: nothing was installed, so poison.Scan can never run
	}
	if _, err := got.Scan(context.Background(), govern.ArtifactPayload{Content: "irrelevant"}); err != nil {
		t.Fatalf("scan error: %v", err)
	}
}

// TestInterceptorForOnReturnsTheScanner is the converse of the off-path
// gate: with ORBEAT_INTERCEPT set, interceptorFor must return the scanner it
// was given, not silently drop it -- otherwise "on" would be exactly as
// inert as "off" is meant to be, and no test above would catch it (a
// function that always returns nil passes TestInterceptorForOffNeverInvokesScanner
// trivially).
func TestInterceptorForOnReturnsTheScanner(t *testing.T) {
	real := govern.NewDefaultScanner()
	got := interceptorFor(config.Config{Intercept: "1"}, real)
	if got == nil {
		t.Fatal("ORBEAT_INTERCEPT set: interceptorFor returned nil, want the scanner installed")
	}
}
