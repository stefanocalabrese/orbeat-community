package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBuildsTheGatewayWithTheAllowlistedSecretsResolver is cmd/gateway's
// half of the pair whose cmd/api half is TestRunDoesNotWireSecretsIntoTheServer,
// and it exists because this binary reaches the same value by a route every
// other gate in the repo is structurally blind to.
//
// internal/gateway.Server holds its catalog secrets resolver in an unexported
// field set ONLY by gateway.New's fourth positional argument. There is no
// SetSecrets installer on this Server at all, so
// TestAllServerInstallersAreWiredOrExempt cannot reach the value: it diffs
// declared Set* methods against called Set* methods, and a constructor argument
// is neither. cmd/api's inverse gate does not transfer either, since it asserts
// the ABSENCE of a call that on this side does not exist.
//
// What the argument decides: secrets.NewResolver() enforces the
// ORBEAT_SECRET_ENV_ALLOW prefix allowlist on env: refs (default
// ORBEAT_UPSTREAM_), while secrets.NewProcessConfigResolver() switches it off,
// and both return the same *secrets.Resolver type, so swapping one for the
// other is a one-token edit that compiles. The gateway is the consumer that
// matters: dialUpstream resolves an mcp_server row's secret_ref and sends the
// resolved value as an Authorization: Bearer header to that row's
// endpointOrCommand, which an admin also chose. Audit A4 is exactly that path.
//
// Measured before this file existed: swapping the argument to
// NewProcessConfigResolver() left cmd/gateway, internal/gateway and
// internal/secrets all reporting ok, TestDialUpstreamRefusesADisallowedEnvSecretRef
// included. That test builds its own &Server{secrets: secrets.NewResolver()}
// fixture on purpose (it needs the field set without a database), which makes it
// a statement about the RESOLVER's behaviour and not about which resolver the
// binary hands the gateway. Nothing else looked at the argument.
//
// Derivation, not a literal, on both sides. The parameter INDEX comes from
// gateway.New's own signature (secretsResolverParam finds the parameter whose
// type is *secrets.Resolver), so reordering New's parameters moves this check
// with them instead of silently pointing it at the wrong argument. The value
// comes from run()'s own source, either as a direct secrets.X() call or by
// tracing a local back to its assignment, so hoisting the constructor into a
// variable keeps the gate working rather than blinding it.
//
// Non-vacuity is checked at four points, because a negative-shaped assertion
// over a scan is worth nothing when the scan can return empty for an unrelated
// reason: the parameter must be found in the signature, the call must be found
// in run(), its argument count must match New's arity, and the argument must
// resolve to some package-qualified constructor. Any of those failing is a
// t.Fatal saying the scan is broken, never a pass.
func TestRunBuildsTheGatewayWithTheAllowlistedSecretsResolver(t *testing.T) {
	gatewayDir := filepath.Join("..", "..", "internal", "gateway")
	idx, arity, ok := secretsResolverParam(t, gatewayDir)
	if !ok {
		t.Fatalf("no parameter of type *secrets.Resolver on gateway.New in %s: the constructor's shape "+
			"changed, so this gate cannot tell which argument carries the catalog secrets resolver and "+
			"would otherwise pass without checking anything", gatewayDir)
	}

	runFn := parseRunFunc(t, "main.go")
	args, found := callArgsInFunc(runFn, "gateway", "New")
	if !found {
		t.Fatal("run() contains no gateway.New(...) call; the gateway's secrets resolver is not " +
			"where this gate looks, so its silence would prove nothing")
	}
	if len(args) != arity {
		t.Fatalf("run()'s gateway.New call has an argument count of %d against gateway.New's %d "+
			"parameters (a spread call, or a signature change): position %d is not reliably the "+
			"*secrets.Resolver, so this gate is reading the wrong argument", len(args), arity, idx)
	}

	pkg, ctor, resolved := constructorForArg(runFn, args[idx])
	if !resolved {
		t.Fatalf("argument %d of run()'s gateway.New call does not resolve to a package-qualified "+
			"constructor call (got %T): it is neither pkg.Ctor(...) inline nor a local assigned from "+
			"one, so the scan cannot see which resolver the binary builds", idx, args[idx])
	}
	if pkg != "secrets" {
		t.Fatalf("argument %d of run()'s gateway.New call is %s.%s, not from package secrets: the "+
			"catalog secrets resolver is no longer built where this gate reads it", idx, pkg, ctor)
	}

	if ctor != "NewResolver" {
		t.Errorf("run() builds the gateway with secrets.%s. Only secrets.NewResolver() enforces the "+
			"ORBEAT_SECRET_ENV_ALLOW prefix allowlist on catalog env: refs; "+
			"secrets.NewProcessConfigResolver() has it switched OFF and exists solely for the three "+
			"refs cmd/api reads out of the process environment. With the unrestricted resolver here, "+
			"any admin can POST /v1/admin/servers with secretRef \"env:ORBEAT_DB_URL\" (or any other "+
			"process environment variable) and endpointOrCommand pointing at a host they chose, and "+
			"the next MCP session sends that variable's value to that host as an Authorization: "+
			"Bearer header. That is audit A4 reopened on the dial path, which is the path that "+
			"actually mails the secret out. See NewProcessConfigResolver's doc comment for why the "+
			"two populations need two policies", ctor)
	}
}

// secretsResolverParam parses the non-test .go files directly inside dir,
// finds the package-level func New (no receiver), and returns the zero-based
// index of its *secrets.Resolver parameter plus New's total parameter count.
//
// The count is returned so the caller can reject a call whose argument count
// disagrees with the signature, which is the shape a spread call
// (gateway.New(f())) or an un-updated call site takes. Parameter names are
// counted, not fields, because Go groups same-typed parameters into one field
// (resource, authServer string is one field and two parameters), and an index
// derived from fields would be off by one against the argument list.
func secretsResolverParam(t *testing.T, dir string) (idx, arity int, ok bool) {
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
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Recv != nil || fn.Name.Name != "New" || fn.Type.Params == nil {
				continue
			}
			pos := 0
			for _, field := range fn.Type.Params.List {
				n := len(field.Names)
				if n == 0 { // an unnamed parameter still occupies one position
					n = 1
				}
				if isPointerToSelector(field.Type, "secrets", "Resolver") {
					idx, ok = pos, true
				}
				pos += n
			}
			arity = pos
			return idx, arity, ok
		}
	}
	return 0, 0, false
}

// isPointerToSelector reports whether expr is *pkg.Name.
func isPointerToSelector(expr ast.Expr, pkg, name string) bool {
	star, isStar := expr.(*ast.StarExpr)
	if !isStar {
		return false
	}
	sel, isSel := star.X.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != name {
		return false
	}
	ident, isIdent := sel.X.(*ast.Ident)
	return isIdent && ident.Name == pkg
}

// callArgsInFunc returns the argument list of the call pkgAlias.fnName(...)
// found inside fn, and whether one was found at all.
func callArgsInFunc(fn *ast.FuncDecl, pkgAlias, fnName string) ([]ast.Expr, bool) {
	var args []ast.Expr
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if pkg, name, ok := selectorCallName(call); ok && pkg == pkgAlias && name == fnName {
			args, found = call.Args, true
		}
		return true
	})
	return args, found
}

// constructorForArg resolves arg to the package-qualified constructor that
// produced it: either the call written in place (secrets.NewResolver()), or,
// when arg is a plain identifier, the constructor on the right-hand side of
// that identifier's assignment inside fn. Both shapes are accepted so that
// hoisting the constructor into a local (which cmd/api's run() already does for
// its own resolver) does not blind this gate.
func constructorForArg(fn *ast.FuncDecl, arg ast.Expr) (pkg, name string, ok bool) {
	switch e := arg.(type) {
	case *ast.CallExpr:
		return selectorCallName(e)
	case *ast.Ident:
		var foundPkg, foundName string
		found := false
		ast.Inspect(fn, func(n ast.Node) bool {
			assign, isAssign := n.(*ast.AssignStmt)
			if !isAssign {
				return true
			}
			for i, lhs := range assign.Lhs {
				ident, isIdent := lhs.(*ast.Ident)
				if !isIdent || ident.Name != e.Name || i >= len(assign.Rhs) {
					continue
				}
				call, isCall := assign.Rhs[i].(*ast.CallExpr)
				if !isCall {
					continue
				}
				if p, n2, okCall := selectorCallName(call); okCall {
					foundPkg, foundName, found = p, n2, true
				}
			}
			return true
		})
		return foundPkg, foundName, found
	}
	return "", "", false
}

// selectorCallName splits a call of the form ident.Sel(...) into its two
// identifiers.
func selectorCallName(call *ast.CallExpr) (pkg, name string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return "", "", false
	}
	return ident.Name, sel.Sel.Name, true
}
