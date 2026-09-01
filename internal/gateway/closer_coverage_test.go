package gateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"reflect"
	"sort"
	"testing"
)

// closerTypedServerFields derives, from Server's own field declarations
// rather than from a hand-maintained list, every field whose type implements
// io.Closer (Close() error) -- the exact shape ratelimit.Limiter and
// ratelimit.ConcurrencyLimiter both have (internal/ratelimit/ratelimit.go,
// concurrency.go). This IS the "limiter-typed" set: whatever field
// reflection turns up is one whose background work Server.Close() must stop,
// whatever that field ends up being named or however many there are.
//
// store.Store.Close() has no return value ("func (s *Store) Close()" in
// internal/store/store.go), so s.store does NOT implement io.Closer and is
// correctly excluded -- checked deliberately, because a looser derivation
// (any method literally named Close, any signature) would have swept it in
// and demanded Server.Close() call s.store.Close(), which would close the
// shared pool every Server in this package's tests borrows but none of them
// own.
func closerTypedServerFields(t *testing.T) []string {
	t.Helper()
	closer := reflect.TypeOf((*io.Closer)(nil)).Elem()
	st := reflect.TypeOf(Server{})
	var names []string
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		ft := f.Type
		implements := ft.Implements(closer)
		if !implements && ft.Kind() != reflect.Ptr {
			implements = reflect.PointerTo(ft).Implements(closer)
		}
		if implements {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("reflection found ZERO io.Closer-typed fields on Server{}; either Server lost " +
			"every limiter it used to hold (delete this test with a reason if so) or " +
			"reflect.TypeOf(Server{}) stopped reaching its fields, which would make every " +
			"assertion in TestServerCloseCoversEveryCloserTypedField pass over an empty set")
	}
	return names
}

// closedFieldsInServerClose parses server.go's own source (not a copy, not a
// snippet quoted in this file) and returns the set of Server field names that
// Server.Close()'s body actually calls .Close() on.
//
// This is a STATIC derivation, not a run of the method, and that is the
// point: it also catches a field Close() would only reach through a setter
// nobody calls, which is exactly the class TestServerCloseStopsEveryLimiter-
// SweeperGoroutine's own doc comment names as its blind spot -- a new field
// left at nil (New's default, since setters run after New) leaks nothing for
// goleak to observe, so a purely runtime test never fires for it.
func closedFieldsInServerClose(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	const path = "server.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var closeFn *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Close" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		id, ok := star.X.(*ast.Ident)
		if !ok || id.Name != "Server" {
			continue
		}
		closeFn = fn
		break
	}
	if closeFn == nil {
		t.Fatalf("no method `func (*Server) Close()` found in %s: it was renamed or moved, "+
			"so every assertion derived from its body would be checked against nothing", path)
	}
	if len(closeFn.Recv.List[0].Names) == 0 {
		t.Fatalf("Close()'s receiver is unnamed in %s, so this scan cannot tell "+
			"s.<field>.Close() apart from any other .Close() call in its body", path)
	}
	recv := closeFn.Recv.List[0].Names[0].Name

	found := map[string]bool{}
	ast.Inspect(closeFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Close" {
			return true
		}
		fieldSel, ok := sel.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := fieldSel.X.(*ast.Ident); ok && id.Name == recv {
			found[fieldSel.Sel.Name] = true
		}
		return true
	})
	if len(found) == 0 {
		t.Fatalf("Close()'s body in %s calls .Close() on nothing shaped like %s.<field>.Close(): "+
			"either Close was gutted or this scan's AST shape assumption no longer matches how "+
			"it closes the limiters, and every assertion below would pass over an empty set",
			path, recv)
	}
	return found
}

// TestServerCloseCoversEveryCloserTypedField is the derived complement to
// TestServerCloseStopsEveryLimiterSweeperGoroutine above: that test proves
// Close() actually stops the sweepers of the three limiter fields it already
// knows to construct and check with goleak; this one proves there is no
// FOURTH (or fifth) io.Closer-typed field on Server that Close() forgot,
// without needing to know its name in advance or build a working instance
// of it.
//
// The two are not redundant, and neither subsumes the other. The goleak test
// can only discover a leak in a field it explicitly sets to a real value and
// then observes NOT being closed -- exactly the OBLIGATION its own doc
// comment names: a Server fresh from New() holds nil in every limiter field
// (they are injected afterwards via the SetXxx methods, mirroring
// cmd/gateway/main.go), and nil never leaks, so a field nobody's test bothers
// to construct passes the goleak gate vacuously. This test needs no instance
// at all: it compares two static derivations of Server's own source --
// reflect.Type.Implements against server.go's AST -- so a field that is
// merely DECLARED as Closer-typed and never referenced inside Close() fails
// here even though it never held a real value that could leak anything.
//
// What it does NOT prove: that a field Close() does call .Close() on
// actually stops that field's background work, or that the call happens
// before Close() returns rather than, say, inside a dead branch. That is
// exactly what TestServerCloseStopsEveryLimiterSweeperGoroutine's goleak
// diff proves for the three fields it constructs. A field this test flags
// still needs a line added to that test's fixture (real value, goleak
// diff) to get the SAME dynamic proof the first three fields already have --
// this gate only refuses to let it ship with zero coverage of either kind.
func TestServerCloseCoversEveryCloserTypedField(t *testing.T) {
	want := closerTypedServerFields(t)
	got := closedFieldsInServerClose(t)

	for _, name := range want {
		if !got[name] {
			t.Errorf("Server.%s has a Close() error method (found by reflecting on Server{}) "+
				"but Server.Close() (server.go) never calls %s.Close(). Wire it in, nil-checked "+
				"like the three fields already there, and add a real instance of it to "+
				"TestServerCloseStopsEveryLimiterSweeperGoroutine's fixture -- otherwise this "+
				"field's background goroutine, if it starts one, leaks for the life of the "+
				"process, the same way two of the three limiter fields did before that test "+
				"existed to catch it (see its own doc comment)", name, name)
		}
	}
}
