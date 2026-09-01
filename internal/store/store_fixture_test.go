package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// noopQueryTracer is a pgx.QueryTracer that does nothing. Its only job is to be
// NON-NIL, so newProductionShapedStore exercises the branch of NewWithTracer
// that a nil tracer skips.
type noopQueryTracer struct{}

func (noopQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (noopQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// newProductionShapedStore builds a Store the way cmd/api and cmd/gateway do:
// NewWithTracer with every parameter set to a non-nil value.
//
// newTestStore calls New, which is NewWithTracer(ctx, dbURL, nil), and that
// single nil is what made TestInTxPropagatesEveryStoreField vacuous (audit C1).
// A Store field populated from a NewWithTracer PARAMETER is zero in a parent
// built that way, and InTx's `&Store{db: pgtx}` literal leaves it zero in the
// child too, so both sides of the comparison agree and the gate cannot fire for
// exactly the field it exists to catch. Red-proven in the audit by adding a
// `tracer` field: the gate passed while the field was silently nil inside every
// transaction, which is where every audited mutation in this codebase runs.
//
// This is the gateway limiter lesson one package over. internal/gateway's
// ratelimit_test.go explicitly refuses a bare-New() fixture for the identical
// reason, and this gate was building the object the test way rather than the
// cmd/api way.
func newProductionShapedStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewWithTracer(context.Background(), testDSN, noopQueryTracer{})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestInTxFixtureLeavesNoFieldZero is the meta-gate that keeps
// TestInTxPropagatesEveryStoreField able to fail, and it is derived rather than
// hand-maintained on purpose.
//
// That gate compares the parent Store against the child InTx built. A field
// that is the ZERO VALUE in the parent is zero on both sides whatever InTx
// does, so the comparison passes for it no matter what. The propagation gate
// can therefore only fail for fields the FIXTURE actually populates, which
// makes "is every field populated?" the real precondition — and nothing
// asserted it.
//
// Reflection is what makes this survive a change nobody remembers to mirror
// here: add a field to Store and forget to populate it in
// newProductionShapedStore, and this fails, naming the field, instead of
// silently exempting it from the gate next door. A hand-listed set of field
// names would be the same defect one level up, which is the mistake this repo
// has now made in a volume gate and an admin-route gate.
func TestInTxFixtureLeavesNoFieldZero(t *testing.T) {
	s := newProductionShapedStore(t)

	v := reflect.ValueOf(*s)
	typ := v.Type()
	if typ.NumField() == 0 {
		t.Fatal("Store has no fields, so this gate and the one it protects are both vacuous")
	}
	for i := range typ.NumField() {
		f := v.Field(i)
		if f.IsZero() {
			t.Errorf(`Store field %q is the zero value on a Store built the way cmd/api builds one.

TestInTxPropagatesEveryStoreField compares the parent Store against the one InTx
hands to fn. A field that is zero in the PARENT is zero on both sides regardless of
what InTx does, so that gate silently cannot fail for this field — which is audit
finding C1, and it was red-proven: a field added this way sat nil inside every
transaction with the whole package green.

Fix the FIXTURE, not this test: populate %q in newProductionShapedStore
(store_fixture_test.go) the way production populates it. If the field genuinely has
no production value and is legitimately zero at construction (a lazily-filled cache,
a flag that starts false), say so here with a named exemption and the reason, the way
'want' in store_tx_test.go documents pool and db.`, typ.Field(i).Name, typ.Field(i).Name)
		}
	}
}
