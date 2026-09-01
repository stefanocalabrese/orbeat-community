package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// parseSnippet parses src as a standalone Go file and returns its parsed
// *ast.File and the *ast.FuncDecl named "run" inside it. Used by this file's
// tests to pin installedLocals/hasLiteralNilArg/isSetterName's exact
// semantics against small, hand-written fragments, independent of whatever
// shape the real cmd/api/main.go happens to be in on a given day -- the
// integration-level proof that the real main.go is covered correctly lives
// in installer_wiring_test.go's own tests plus this package's other wiring
// gates, which parse main.go itself.
func parseSnippet(t *testing.T, src string) (*ast.File, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse snippet: %v\n%s", err, src)
	}
	runFn := findFuncDecl(file, "run")
	if runFn == nil {
		t.Fatal("snippet has no func run()")
	}
	return file, runFn
}

// TestIsSetterNameRequiresCamelCaseBoundary pins the exact bug this file's
// own history caught while building installedLocals: a plain
// strings.HasPrefix(name, "Set") also matches "Setup", and cmd/api's real
// run() calls telemetry.Setup(...). Reproduced live before this fix: running
// TestAllServerInstallersAreWiredOrExempt with isSetterName inlined as
// strings.HasPrefix(name, "Set") failed with `run() calls a Set* method on
// "telemetry", but no assignment "telemetry" := <pkg>.<Ctor>(...) was found
// to trace its type from` -- the right failure SHAPE for the wrong reason,
// since telemetry was never meant to be a target of this gate.
func TestIsSetterNameRequiresCamelCaseBoundary(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"SetScanner", true},
		{"SetDCRClient", true},
		{"Setup", false},    // the real regression: telemetry.Setup
		{"Settings", false}, // same shape, a plausible future false positive
		{"Set", false},      // no camelCase suffix at all
		{"set", false},      // lowercase, not exported
		{"GetScanner", false},
	}
	for _, c := range cases {
		if got := isSetterName(c.name); got != c.want {
			t.Errorf("isSetterName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHasLiteralNilArgDetectsAnyArgumentPosition pins that a literal nil is
// caught wherever it appears in the argument list, not just as a lone
// argument -- SetDCRClient takes two, and a mutant nulling out only the
// second one (`srv.SetDCRClient(dcrRegister, nil)`) is exactly as inert as
// nulling both, per that method's own nil-ignore contract documented in
// dcr_client_wiring_test.go.
func TestHasLiteralNilArgDetectsAnyArgumentPosition(t *testing.T) {
	_, runFn := parseSnippet(t, `package main
func run() {
	x.SetA(nil)
	x.SetB(1, nil)
	x.SetC(nil, 2)
	x.SetD(y)
	x.SetE(y, z)
}
`)
	var calls []*ast.CallExpr
	ast.Inspect(runFn, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, c)
		}
		return true
	})
	want := []bool{true, true, true, false, false}
	if len(calls) != len(want) {
		t.Fatalf("found %d calls in snippet, want %d", len(calls), len(want))
	}
	for i, c := range calls {
		if got := hasLiteralNilArg(c); got != want[i] {
			t.Errorf("call %d: hasLiteralNilArg = %v, want %v", i, got, want[i])
		}
	}
}

// TestInstalledLocalsRedeemsALaterRealCall pins the redemption rule
// installedLocals' own doc comment states: a method that is EVER called for
// real anywhere in run() is wired, regardless of an earlier or later
// nil-literal call to the same method -- call ORDER must not flip the
// verdict. SetOnlyNil never gets a real call and must land in nilOnly, never
// in wired.
func TestInstalledLocalsRedeemsALaterRealCall(t *testing.T) {
	file, runFn := parseSnippet(t, `package main
func run() {
	x.SetRedeemedAfter(nil)
	x.SetRedeemedAfter(real1)
	x.SetRedeemedBefore(real2)
	x.SetRedeemedBefore(nil)
	x.SetOnlyNil(nil)
}
`)
	wired, nilOnly := installedLocals(file, runFn)

	for _, name := range []string{"SetRedeemedAfter", "SetRedeemedBefore"} {
		if !wired["x"][name] {
			t.Errorf("wired[x][%s] = false, want true (a real call exists)", name)
		}
		if nilOnly["x"][name] {
			t.Errorf("nilOnly[x][%s] = true, want false (a real call redeems it)", name)
		}
	}
	if wired["x"]["SetOnlyNil"] {
		t.Error("wired[x][SetOnlyNil] = true, want false (never called for real)")
	}
	if !nilOnly["x"]["SetOnlyNil"] {
		t.Error("nilOnly[x][SetOnlyNil] = false, want true (only ever called with a literal nil)")
	}
}

// TestInstalledLocalsExcludesPackageQualifiedCalls pins the fix for the same
// telemetry.Setup-shaped problem TestIsSetterNameRequiresCamelCaseBoundary
// covers, from the other side: even a genuinely Set*-named, camelCase-correct
// call (slog.SetDefault, in the real main.go) must not be mistaken for a
// method call on a local named "slog", because *ast.SelectorExpr cannot
// otherwise tell "package.Function(...)" from "localVar.Method(...)" -- both
// are an Ident base with a Sel name. The only way to tell them apart from
// source alone is to check whether the base identifier is a name main.go's
// own import block binds to a package, which is exactly what this test
// exercises via a real import.
func TestInstalledLocalsExcludesPackageQualifiedCalls(t *testing.T) {
	file, runFn := parseSnippet(t, `package main

import "log/slog"

func run() {
	slog.SetDefault(nil)
	resolver.SetContactEmail(cfg.ContactEmail)
}
`)
	wired, nilOnly := installedLocals(file, runFn)

	if wired["slog"] != nil || nilOnly["slog"] != nil {
		t.Errorf("installedLocals treated the package identifier %q as an installed-into receiver: "+
			"wired=%v nilOnly=%v", "slog", wired["slog"], nilOnly["slog"])
	}
	if !wired["resolver"]["SetContactEmail"] {
		t.Error("installedLocals did not find the real resolver.SetContactEmail call")
	}
}

// TestCtorReturnTypeNameSkipsErrorAndFindsExportedType pins
// ctorReturnTypeName's contract against a real file on disk (it reads via
// os.ReadDir/os.ReadFile, so an in-memory *ast.File is not enough): given a
// constructor whose signature is (*Widget, error) -- the two-return shape
// none of api.New/gateway.New/authz.NewResolver happen to use today, but
// which this function must still handle correctly since nothing prevents a
// future installer-bearing constructor from returning an error too -- it
// must return "Widget", skipping "error" entirely.
func TestCtorReturnTypeNameSkipsErrorAndFindsExportedType(t *testing.T) {
	dir := t.TempDir()
	src := `package widget

type Widget struct{}

func (w *Widget) SetFoo(v int) {}

func New(x int) (*Widget, error) {
	return &Widget{}, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	typeName, ok := ctorReturnTypeName(t, dir, "New")
	if !ok {
		t.Fatal("ctorReturnTypeName did not find New's return type")
	}
	if typeName != "Widget" {
		t.Errorf("typeName = %q, want %q", typeName, "Widget")
	}

	declared := declaredSetters(t, dir, typeName)
	if !declared["SetFoo"] {
		t.Errorf("declaredSetters(%q, %q) = %v, want it to contain SetFoo", dir, typeName, declared)
	}
}

// TestCtorReturnTypeNameAbsentCtorReturnsFalse pins the negative case: a
// ctorName that does not exist in dir must report ok=false, not panic or
// return a zero-value type name that a caller could mistake for a match.
func TestCtorReturnTypeNameAbsentCtorReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	src := `package widget

type Widget struct{}

func New() *Widget { return &Widget{} }
`
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, ok := ctorReturnTypeName(t, dir, "NewOther"); ok {
		t.Error("ctorReturnTypeName(dir, \"NewOther\") = true, want false: no such constructor exists")
	}
}
