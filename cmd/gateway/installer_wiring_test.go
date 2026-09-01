package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// exemptServerInstallers lists Set* methods, keyed "TypeName.MethodName", on
// a type run() constructs and installs into that are declared but
// deliberately not called, each entry naming the reason.
// TestExemptInstallersNameARealInstallerWithAReason refuses an empty reason
// or a stale name, so an exemption cannot be smuggled in as a silent list
// edit -- writing the reason is the review surface, not list membership.
//
// Keyed by type, not by bare method name, mirroring cmd/api/
// installer_wiring_test.go's sibling map: internal/api.Server and
// internal/authz.Resolver both declare SetContactEmail, so a flat
// map[string]string would let one satisfy the other. Nothing collides in
// cmd/gateway today (this package only ever installs into
// internal/gateway.Server -- see TestAllServerInstallersAreWiredOrExempt's
// own doc comment for why internal/authz.Resolver, also constructed here,
// stays out of scope), but the key shape is kept identical between the two
// packages rather than simplified back to a bare method name, so a future
// second installed-into type here does not need this map's shape to change
// along with it.
//
// Empty today: every Set* method declared on internal/gateway.Server is
// wired in cmd/gateway's run().
var exemptServerInstallers = map[string]string{}

// TestAllServerInstallersAreWiredOrExempt derives, from run()'s own source,
// EVERY type run() constructs and then installs into via a Set* call, not a
// hand-picked (package, type) pair. See deriveInstallTargets below for how
// the type set is found; it resolves to internal/gateway.Server (via gw)
// today. internal/authz.Resolver is ALSO constructed in this run() (the
// "resolver" local, passed straight into gateway.New), but run() never
// calls a Set* method on it here, so it correctly stays out of this gate's
// scope -- see deriveInstallTargets' own doc comment for exactly what that
// means and its one stated limitation, verified by running it against
// cmd/api's sibling gate rather than assumed.
//
// It also fails on a call whose EVERY occurrence passes a literal nil
// argument (installedLocals below): the prior version of this gate only
// asked whether the CALL happened, so `gw.SetInterceptor(nil)` satisfied it
// exactly as well as a working install. See hasLiteralNilArg's own doc
// comment for what a literal nil catches and what it structurally cannot.
//
// This is the gate the virtual-keys slice needed: it shipped nine fully
// tested tasks that were inert because cmd/api never called SetDCRClient,
// and nothing caught it until a documentation task noticed by hand, nine
// commits later. cmd/gateway already carried two hand-written instances of
// this exact shape (TestRunWiresInterceptor, TestRunWiresUsageMetering);
// this gate generalizes past both so a future Set* method does not need a
// third, and it now also generalizes past which TYPE gets that treatment.
func TestAllServerInstallersAreWiredOrExempt(t *testing.T) {
	file, runFn := parseMainFile(t)

	wired, nilOnly := installedLocals(file, runFn)
	if len(wired) == 0 && len(nilOnly) == 0 {
		t.Fatal("installedLocals found zero Set* calls anywhere in run() (there are 6+ today) -- the " +
			"scan itself is broken, not that run() stopped installing anything")
	}

	targets := deriveInstallTargets(t, file, runFn, wired, nilOnly)
	if len(targets) == 0 {
		t.Fatal("deriveInstallTargets resolved zero installed types from run() -- the scan itself is " +
			"broken, since installedLocals above found at least one Set* call")
	}

	for _, tgt := range targets {
		declared := declaredSetters(t, tgt.pkgDir, tgt.typeName)
		if len(declared) == 0 {
			t.Fatalf("declaredSetters found zero Set* methods on %s in %s, but run() calls a Set* "+
				"method on %s (a %s) -- the scan itself is broken", tgt.typeName, tgt.pkgDir, tgt.recv,
				tgt.typeName)
		}
		for name := range declared {
			key := tgt.typeName + "." + name
			if wired[tgt.recv][name] {
				continue
			}
			if reason, exempt := exemptServerInstallers[key]; exempt {
				if strings.TrimSpace(reason) == "" {
					t.Errorf("%s is exempted with an empty reason -- write why, per this file's "+
						"exemptServerInstallers doc comment", key)
				}
				continue
			}
			if nilOnly[tgt.recv][name] {
				t.Errorf("%s is declared, and run() calls %s.%s, but EVERY call passes a literal nil "+
					"argument -- indistinguishable from never calling it at all. Either pass a real "+
					"value or add a named, reasoned exemption to exemptServerInstallers[%q]",
					key, tgt.recv, name, key)
				continue
			}
			t.Errorf("%s is declared but run() never calls %s.%s, and it carries no exemption in "+
				"exemptServerInstallers[%q] -- either wire it in cmd/gateway/main.go's run() or add a "+
				"named, reasoned exemption", key, tgt.recv, name, key)
		}
	}
}

// TestExemptInstallersNameARealInstallerWithAReason guards
// exemptServerInstallers itself: every key must be TypeName.MethodName for a
// Set* method genuinely declared on a type run() installs into (a stale
// exemption for a renamed or removed installer, or for a type run() no
// longer constructs, does nothing, silently, which is worse than doing
// nothing loudly), and every value must be a non-empty reason. Vacuously
// true today since the map is empty.
func TestExemptInstallersNameARealInstallerWithAReason(t *testing.T) {
	file, runFn := parseMainFile(t)
	wired, nilOnly := installedLocals(file, runFn)
	targets := deriveInstallTargets(t, file, runFn, wired, nilOnly)

	declaredByType := map[string]map[string]bool{}
	for _, tgt := range targets {
		if _, ok := declaredByType[tgt.typeName]; ok {
			continue
		}
		declaredByType[tgt.typeName] = declaredSetters(t, tgt.pkgDir, tgt.typeName)
	}

	for key, reason := range exemptServerInstallers {
		typeName, method, ok := strings.Cut(key, ".")
		if !ok || !declaredByType[typeName][method] {
			t.Errorf("exemptServerInstallers names %q, which is not TypeName.MethodName for a "+
				"declared Set* method on a type run() installs into -- stale exemption", key)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemptServerInstallers[%q] has an empty reason", key)
		}
	}
}

// parseRunFunc parses relPath (relative to this package's directory) and
// returns its top-level func run(), or fails the test. Shared by several of
// this package's wiring gates, kept as its own helper so each one does not
// have to repeat the read-parse-find dance.
func parseRunFunc(t *testing.T, relPath string) *ast.FuncDecl {
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
	runFn := findFuncDecl(file, "run")
	if runFn == nil {
		t.Fatalf("%s has no func run()", relPath)
	}
	return runFn
}

// parseMainFile is parseRunFunc's sibling for the tests in this file: it
// also returns the parsed *ast.File, needed to resolve which import path a
// package alias used inside run() (e.g. "authz") actually names, which
// deriveInstallTargets below needs and parseRunFunc's callers do not.
func parseMainFile(t *testing.T) (*ast.File, *ast.FuncDecl) {
	t.Helper()
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
	return file, runFn
}

// declaredSetters walks every non-test .go file directly inside dir and
// returns the set of exported method names declared as
// func (<recv> *typeName) SetXxx(...). The receiver's TYPE is checked
// (*typeName), not the receiver variable's name, so a rename of the
// receiver identifier inside dir does not blind this scan.
func declaredSetters(t *testing.T, dir, typeName string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	setters := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if !isSetterName(fn.Name.Name) {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			setters[fn.Name.Name] = true
		}
	}
	return setters
}

// installerReceiverVar finds the identifier on the left-hand side of an
// assignment inside fn whose right-hand side is a call pkgAlias.ctorName(...)
// -- e.g. gateway.New(...) in cmd/gateway's run() -- and returns that
// identifier's name and true. Returns "", false if no such assignment
// exists.
//
// Kept for TestJWKSRefreshContextSurvivesShutdownSignal
// (jwks_lifetime_test.go), which needs one specific, named receiver
// (signal.NotifyContext's) rather than the full derived set
// TestAllServerInstallersAreWiredOrExempt now builds.
func installerReceiverVar(fn *ast.FuncDecl, pkgAlias, ctorName string) (string, bool) {
	var name string
	var found bool
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != pkgAlias || sel.Sel.Name != ctorName {
				continue
			}
			if i >= len(assign.Lhs) {
				continue
			}
			lhsIdent, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			name, found = lhsIdent.Name, true
		}
		return true
	})
	return name, found
}

// isSetterName reports whether name follows the SetXxx convention every
// installer in this tree uses: the literal prefix "Set" followed by an
// uppercase letter. A plain strings.HasPrefix(name, "Set") is not enough:
// cmd/gateway's real run() has no "Setup"-shaped false positive today (that
// one is cmd/api's telemetry.Setup), but installedLocals below scans every
// receiver in run() indiscriminately, and this boundary check is what keeps
// a future same-shaped stdlib or internal call from being mistaken for an
// installer here too. See cmd/api/installer_wiring_helpers_test.go's
// TestIsSetterNameRequiresCamelCaseBoundary for the reproduction that found
// this needed a boundary check in the first place.
func isSetterName(name string) bool {
	const prefix = "Set"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := name[len(prefix):]
	if rest == "" {
		return false
	}
	r := rest[0]
	return r >= 'A' && r <= 'Z'
}

// hasLiteralNilArg reports whether any argument of call is the literal
// identifier `nil`.
//
// What this catches: `gw.SetInterceptor(nil)` -- a call written in source
// with a bare `nil` in any argument position, which is provably, statically,
// exactly as inert as never calling the setter at all, for any setter whose
// parameter accepts nil (pointer, interface, func, slice, map, chan --
// every parameter type every Set* method in this tree takes).
//
// What this CANNOT catch, and no source-level check can: `x :=
// interceptorFor(cfg, scanner); gw.SetInterceptor(x)` where interceptorFor
// always returns nil. The argument is an identifier, not the literal `nil`,
// and telling "a variable that happens to always be nil" from "a variable
// that is sometimes a real value" requires running the program, not reading
// its syntax. That is exactly why interceptorFor has its own direct unit
// tests (TestInterceptorForOffNeverInvokesScanner,
// TestInterceptorForOnReturnsTheScanner in intercept_wiring_test.go) rather
// than relying on this gate to see through it.
func hasLiteralNilArg(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == "nil" {
			return true
		}
	}
	return false
}

// installedLocals scans fn for every call of the shape `recv.SetXxx(...)`
// and buckets each by recv, splitting each receiver's Set* calls into wired
// (at least one call to that method had no literal nil argument) and
// nilOnly (every call to that method found so far had one). A later
// non-nil call redeems an earlier nil-only sighting -- deleted from nilOnly
// the moment a real call is seen -- so call ORDER inside run() never
// produces a false nilOnly verdict for a method that is, on some path,
// called for real.
//
// file is needed to exclude PACKAGE-qualified calls: `pkgAlias.Fn(...)` and
// `localVar.Method(...)` are the identical AST shape (*ast.SelectorExpr with
// an *ast.Ident base), so without cross-checking against main.go's own
// import block, a package-qualified Set*-named call would look exactly like
// a method call on a same-named local, with no assignment for
// deriveInstallTargets to trace -- a loud, correct-shaped failure for the
// wrong reason. Any receiver name that resolves to an import alias is
// therefore skipped here, not passed through to fail later.
func installedLocals(file *ast.File, fn *ast.FuncDecl) (wired, nilOnly map[string]map[string]bool) {
	wired = map[string]map[string]bool{}
	nilOnly = map[string]map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isSetterName(sel.Sel.Name) {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isPkg := importPathForAlias(file, recv.Name); isPkg {
			return true
		}
		if hasLiteralNilArg(call) {
			if wired[recv.Name][sel.Sel.Name] {
				return true // already redeemed by a real call elsewhere
			}
			if nilOnly[recv.Name] == nil {
				nilOnly[recv.Name] = map[string]bool{}
			}
			nilOnly[recv.Name][sel.Sel.Name] = true
			return true
		}
		if wired[recv.Name] == nil {
			wired[recv.Name] = map[string]bool{}
		}
		wired[recv.Name][sel.Sel.Name] = true
		delete(nilOnly[recv.Name], sel.Sel.Name)
		return true
	})
	return wired, nilOnly
}

// constructorPkgAndCtor finds the assignment `recv := pkgAlias.Ctor(...)`
// (recv may be any one of several names on the left-hand side of a
// multi-value assignment) inside fn and returns pkgAlias and Ctor.
//
// Known limitation, accepted: this recognizes only a plain package-qualified
// call as the right-hand side. A local later reassigned, or built through an
// intermediate wrapper function (e.g. `gw := buildGateway(...)`), is not
// traced -- deriveInstallTargets below turns that into a loud t.Fatal
// ("cannot trace its type") rather than a silent narrowing of what this
// gate checks.
//
// selectorCallName, used below, is secrets_resolver_arg_test.go's helper of
// the same name -- reused here rather than redeclared, since both files
// live in this same `main` package.
func constructorPkgAndCtor(fn *ast.FuncDecl, recv string) (pkgAlias, ctorName string, ok bool) {
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, isAssign := n.(*ast.AssignStmt)
		if !isAssign {
			return true
		}
		for i, lhs := range assign.Lhs {
			ident, isIdent := lhs.(*ast.Ident)
			if !isIdent || ident.Name != recv || i >= len(assign.Rhs) {
				continue
			}
			call, isCall := assign.Rhs[i].(*ast.CallExpr)
			if !isCall {
				continue
			}
			if p, c, okSel := selectorCallName(call); okSel {
				pkgAlias, ctorName, ok = p, c, true
			}
		}
		return true
	})
	return
}

// importPathForAlias returns the full import path main.go's import block
// binds to local package identifier alias -- either an explicit `import
// alias "path"` name, or (the common case here) the path's last segment --
// and whether one was found.
func importPathForAlias(file *ast.File, alias string) (string, bool) {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		local := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			local = path[idx+1:]
		}
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == alias {
			return path, true
		}
	}
	return "", false
}

// moduleImportPrefix reads the module path out of go.mod, so the boundary
// deriveInstallTargets enforces ("this gate only polices our own internal/
// packages") is read from the repository's own declaration rather than
// typed as a second copy of the module path that could drift from it.
func moduleImportPrefix(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatal(`go.mod has no "module" line`)
	return ""
}

// exportedLocalTypeName reports the base type name of expr (a pointer is
// stripped) if expr is an exported identifier other than "error", and false
// otherwise -- e.g. *Server -> "Server", true; error -> "", false;
// []string -> "", false.
func exportedLocalTypeName(expr ast.Expr) (string, bool) {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok || ident.Name == "error" || !ast.IsExported(ident.Name) {
		return "", false
	}
	return ident.Name, true
}

// ctorReturnTypeName parses the non-test .go files directly inside dir,
// finds the package-level function named ctorName (no receiver), and
// returns the base name of the first exported, non-error type among its
// results -- e.g. gateway.New's `func New(...) *Server` yields "Server".
// This is how deriveInstallTargets learns which TYPE a local holds without
// hand-naming it: the type name comes from the constructor's own declared
// signature, in the package run() imports it from, read fresh every run.
func ctorReturnTypeName(t *testing.T, dir, ctorName string) (string, bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != ctorName || fn.Type.Results == nil {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if typeName, ok := exportedLocalTypeName(res.Type); ok {
					return typeName, true
				}
			}
		}
	}
	return "", false
}

// installTarget names one type run() constructs (via recv := pkgAlias.
// Ctor(...)) and installs into via at least one Set* call: recv is the local
// identifier run() uses, pkgDir is that type's package directory relative to
// this package, and typeName is the type's own name as declared there.
type installTarget struct {
	recv     string
	pkgDir   string
	typeName string
}

// deriveInstallTargets is the type-set derivation TestAllServerInstallersAreWiredOrExempt's
// own doc comment describes: rather than a hand-picked (package, type) pair
// -- the earlier version of this gate hard-coded internal/gateway.Server and
// nothing else -- it reads, from run()'s own source and the constructors
// run() calls, every type run() both constructs AND calls a Set* method on
// (wired/nilOnly, from installedLocals), which today resolves to
// internal/gateway.Server (via gw) alone.
//
// A type run() constructs but never calls a Set* method on (internal/authz.
// Resolver in this package's own run(), which builds one and passes it
// straight into gateway.New without calling anything on it) is correctly
// NOT a target: there is nothing to check completeness of when the called
// set is empty, and requiring every constructed type's every setter to be
// called regardless of whether run() uses that type's Set* surface at all
// would be a different, wrong property (it would also make
// internal/store.Store -- which declares five Set* CRUD methods called only
// from request handlers, never from run() -- a bogus mandatory target here
// too). The moment a future change calls even one Set* method on such a
// local, this derivation picks it up automatically, with no second edit to
// this file required.
//
// STATED LIMIT, VERIFIED IN cmd/api'S SIBLING GATE, NOT ASSUMED: this
// criterion has a blind spot when EVERY Set* call on a type is removed at
// once, and it is exactly as sharp as the type has few setters -- see
// cmd/api/installer_wiring_test.go's own copy of this comment for the
// reproduction (deleting cmd/api's one resolver.SetContactEmail call makes
// "resolver" disappear from installedLocals entirely, indistinguishable
// from never having been installed into). internal/gateway.Server has six
// setters today, so this package's own target is not exposed to that
// failure mode; if it ever narrowed to one, the same limitation would apply
// here too.
//
// Every failure to resolve a target -- a receiver whose constructor call
// cannot be traced, an import alias with no matching entry in main.go's
// import block, an import path outside this module's internal/ tree, a
// constructor whose return type this can't name -- is a t.Fatal, never a
// skip. A silent skip here would be exactly the hand-written-list defect
// this function exists to remove, just moved into the derivation step
// instead of a hard-coded list.
func deriveInstallTargets(t *testing.T, file *ast.File, runFn *ast.FuncDecl, wired, nilOnly map[string]map[string]bool) []installTarget {
	t.Helper()
	modPrefix := moduleImportPrefix(t)

	recvs := map[string]bool{}
	for r := range wired {
		recvs[r] = true
	}
	for r := range nilOnly {
		recvs[r] = true
	}

	var targets []installTarget
	for recv := range recvs {
		pkgAlias, ctorName, ok := constructorPkgAndCtor(runFn, recv)
		if !ok {
			t.Fatalf("run() calls a Set* method on %q, but no assignment %q := <pkg>.<Ctor>(...) was "+
				"found to trace its type from -- this gate's derivation is broken, not the code", recv, recv)
		}
		importPath, ok := importPathForAlias(file, pkgAlias)
		if !ok {
			t.Fatalf("run() assigns %q from %s.%s(...), but main.go's import block has no entry "+
				"resolving to package alias %q", recv, pkgAlias, ctorName, pkgAlias)
		}
		if !strings.HasPrefix(importPath, modPrefix+"/internal/") {
			t.Fatalf("run() assigns %q from %s.%s(...) (import %q), which is outside this module's "+
				"internal/ tree -- this gate only knows how to police our own packages' Set* surface, "+
				"and a Set* call on a local of an external type needs a different gate, not a silent "+
				"skip here", recv, pkgAlias, ctorName, importPath)
		}
		pkgDir := filepath.Join("..", "..", filepath.FromSlash(strings.TrimPrefix(importPath, modPrefix+"/")))
		typeName, ok := ctorReturnTypeName(t, pkgDir, ctorName)
		if !ok {
			t.Fatalf("could not resolve the return type of %s.%s in %s -- this gate's derivation is "+
				"broken, not the code", pkgAlias, ctorName, pkgDir)
		}
		targets = append(targets, installTarget{recv: recv, pkgDir: pkgDir, typeName: typeName})
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].recv < targets[j].recv })
	return targets
}
