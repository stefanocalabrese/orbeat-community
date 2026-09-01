package brewformula

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// wantRequirePlatformCalls and wantVerifyCoverageCalls are the exact call
// counts render.go's own doc comments already claim: requirePlatform has
// "three call sites in Render" (render.go:150-153, one per hardcoded
// template slot), and verifyCoverage is called once, after the template
// executes (render.go:344-346). Both guards were added to close real
// completeness gaps and neither is exercised by calling Render itself -
// TestRequirePlatformRejectsUnknownCombo and
// TestVerifyCoverageCatchesMissingPlatform in render_test.go call these two
// functions DIRECTLY, which proves the functions work but not that Render
// still calls them. Deleting the verifyCoverage call, or replacing all three
// requirePlatform calls with bare Platform{} literals, leaves every test in
// this package green without this gate.
const (
	wantRequirePlatformCalls = 3
	wantVerifyCoverageCalls  = 1
)

// TestRenderCallsRequirePlatformAndVerifyCoverage parses render.go's own
// source and counts calls to requirePlatform and verifyCoverage made
// directly inside func Render, rather than trusting that a function existing
// in the package means Render actually uses it - scripts/publish-community-
// release.sh depends on Render aborting BEFORE a release goes out, so an
// unwired guard would fail silently, not loudly, exactly where this package
// cannot see it fail.
func TestRenderCallsRequirePlatformAndVerifyCoverage(t *testing.T) {
	renderFn := parseFuncDecl(t, "render.go", "Render")

	counts := map[string]int{}
	ast.Inspect(renderFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		counts[ident.Name]++
		return true
	})

	if got := counts["requirePlatform"]; got < wantRequirePlatformCalls {
		t.Errorf("Render calls requirePlatform %d times, want at least %d: fewer means one of the "+
			"template's three hardcoded platform slots (render.go:150-153) renders with no check "+
			"that Platforms() still names it", got, wantRequirePlatformCalls)
	}
	if got := counts["verifyCoverage"]; got < wantVerifyCoverageCalls {
		t.Errorf("Render calls verifyCoverage %d times, want at least %d: without it, Platforms() "+
			"growing past the three hardcoded template slots renders a formula that silently omits "+
			"the new platform and nothing in this package catches it", got, wantVerifyCoverageCalls)
	}
}

// parseFuncDecl parses relPath (relative to this package's directory) and
// returns the top-level, non-method func named fnName, or fails the test.
func parseFuncDecl(t *testing.T, relPath, fnName string) *ast.FuncDecl {
	t.Helper()
	src, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == fnName {
			return fn
		}
	}
	t.Fatalf("%s has no top-level func %s", relPath, fnName)
	return nil
}
