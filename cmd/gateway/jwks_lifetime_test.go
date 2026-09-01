package main

import (
	"go/ast"
	"testing"
)

// TestJWKSRefreshContextSurvivesShutdownSignal pins the one thing that keeps
// token validation working while the gateway drains.
//
// auth.NewValidator hands its context to jwk.NewCache, which hands it to
// httprc's controller goroutine; that goroutine returns on ctx.Done(). Once it
// is gone, jwk.Cache.Lookup sends its request on a channel nobody receives
// from, so every Validate blocks until the CALLER's context is done and then
// fails with that context's error. It does not fall back to cached keys.
// internal/auth's TestValidateStallsOnceRefreshContextIsCancelled pins that
// behaviour directly, against a real jwk.Cache.
//
// run() builds its shutdown context with signal.NotifyContext(SIGINT,
// SIGTERM) and then drains for 10s via srv.Shutdown. Wiring that same context
// into auth.NewValidator therefore killed the JWKS controller at the instant
// the drain began, so every request still in flight stalled for its whole
// deadline instead of validating from the cache that was sitting right there.
//
// The gate is derived from source on both sides rather than matching a
// hard-coded identifier: it finds the shutdown context by tracing the
// assignment whose right-hand side is signal.NotifyContext(...), and it finds
// the validator's context by taking argument 0 of the auth.NewValidator(...)
// call. Renaming either local cannot blind it. The dependency walk is
// transitive, so smuggling the shutdown context back in as
// context.WithCancel(ctx) also fails.
func TestJWKSRefreshContextSurvivesShutdownSignal(t *testing.T) {
	runFn := parseRunFunc(t, "main.go")

	shutdownVar, ok := installerReceiverVar(runFn, "signal", "NotifyContext")
	if !ok {
		t.Fatal("run() has no local assigned from signal.NotifyContext(...); this gate's whole " +
			"premise is that such a context exists and must NOT reach auth.NewValidator, so a " +
			"missing anchor is a broken gate, not a passing one")
	}

	arg, ok := firstArgOfCall(runFn, "auth", "NewValidator")
	if !ok {
		t.Fatal("run() never calls auth.NewValidator(...); this gate cannot check an argument that " +
			"is not there, and a gateway with no token validator is a bigger problem than the one " +
			"this test guards")
	}

	deps := identDependencies(runFn, arg)
	if deps[shutdownVar] {
		t.Fatalf("auth.NewValidator's context depends on %q, the shutdown context from "+
			"signal.NotifyContext: cancelling it stops httprc's controller goroutine, after which "+
			"every Validate blocks until the caller's deadline and then fails instead of serving "+
			"cached keys, so every request arriving during srv.Shutdown's drain stalls. Pass a "+
			"context that outlives the shutdown signal, as cmd/api/main.go does", shutdownVar)
	}
}

// firstArgOfCall returns the first argument expression of the first
// pkgAlias.fnName(...) call inside fn.
func firstArgOfCall(fn *ast.FuncDecl, pkgAlias, fnName string) (ast.Expr, bool) {
	var arg ast.Expr
	var found bool
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != pkgAlias || sel.Sel.Name != fnName {
			return true
		}
		arg, found = call.Args[0], true
		return false
	})
	return arg, found
}

// identDependencies returns every identifier name that expr depends on inside
// fn, following assignments transitively: an identifier in expr pulls in every
// identifier on the right-hand side of any assignment in fn that defines it,
// and so on to a fixpoint.
//
// Transitivity is the point. A gate that only compared expr's own identifiers
// would go green on jwksCtx, _ := context.WithCancel(ctx), which propagates
// the very cancellation this package must not propagate.
func identDependencies(fn *ast.FuncDecl, expr ast.Expr) map[string]bool {
	deps := identsIn(expr)
	for {
		grown := false
		ast.Inspect(fn, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			defines := false
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && deps[ident.Name] {
					defines = true
				}
			}
			if !defines {
				return true
			}
			for _, rhs := range assign.Rhs {
				for name := range identsIn(rhs) {
					if !deps[name] {
						deps[name] = true
						grown = true
					}
				}
			}
			return true
		})
		if !grown {
			return deps
		}
	}
}

// identsIn returns the set of identifier names appearing anywhere in expr.
func identsIn(expr ast.Expr) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			names[ident.Name] = true
		}
		return true
	})
	return names
}
