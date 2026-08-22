package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestInTxCommits(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn, err := s.GetOrCreateTenantByName(ctx, t.Name())
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	var roleID string
	err = s.InTx(ctx, func(tx *Store) error {
		r, err := tx.CreateRole(ctx, tn.ID, "tx-role")
		if err != nil {
			return err
		}
		roleID = r.ID
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	roles, _ := s.ListRolesPage(ctx, tn.ID, nil, 0)
	if len(roles) != 1 || roles[0].ID != roleID {
		t.Fatalf("committed role not visible: %+v", roles)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn, err := s.GetOrCreateTenantByName(ctx, t.Name())
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	sentinel := errors.New("boom")
	err = s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.CreateRole(ctx, tn.ID, "doomed-role"); err != nil {
			return err
		}
		return sentinel // force rollback
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
	roles, _ := s.ListRolesPage(ctx, tn.ID, nil, 0)
	if len(roles) != 0 {
		t.Fatalf("rollback failed, roles persisted: %+v", roles)
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn, err := s.GetOrCreateTenantByName(ctx, t.Name())
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = s.InTx(ctx, func(tx *Store) error {
			if _, err := tx.CreateRole(ctx, tn.ID, "panic-role"); err != nil {
				t.Errorf("CreateRole: %v", err)
			}
			panic("boom")
		})
	}()
	roles, _ := s.ListRolesPage(ctx, tn.ID, nil, 0)
	if len(roles) != 0 {
		t.Fatalf("panic rollback failed, roles persisted: %+v", roles)
	}
}

// TestInTxPropagatesEveryStoreField guards the trap documented in
// docs/specs/2026-08-19-orbeat-intx-field-propagation-design.md: InTx builds the
// Store handed to fn as a fresh struct literal (&Store{db: pgtx}), not by copying
// the receiver, so any field added to Store in the future is silently the zero
// value inside every transaction unless InTx's literal is updated to carry it.
//
// want is built from the parent Store with exactly the two fields InTx is allowed
// to change: pool is nil'd (a tx-bound Store must fail InTx's own nesting guard —
// see TestInTxRejectsNesting) and db is taken from the child (the transaction
// itself is the entire point of InTx). Every other field must come through
// untouched. Do NOT "fix" this by deriving want (or the child, inside InTx itself)
// by copying the receiver — see the design doc §2 for why that would make this
// gate unfalsifiable.
func TestInTxPropagatesEveryStoreField(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var child *Store
	err := s.InTx(ctx, func(tx *Store) error {
		child = tx
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	want := *s
	want.pool = nil    // tx-bound Stores must fail InTx's nesting guard
	want.db = child.db // the transaction is the whole point of InTx

	if !reflect.DeepEqual(*child, want) {
		t.Fatalf(`InTx (internal/store/store.go) builds the transaction-bound Store passed to
fn as a fresh struct literal, &Store{db: pgtx} — it does NOT copy the receiver. So a
field on Store that this literal does not mention is silently the zero value inside
every transaction, even though the same field is set on the top-level Store, and every
audited mutation in this codebase runs inside a transaction.

This test failed because a field on Store now differs between the parent and the
Store InTx handed to fn (%%+v below prints every field, including unexported ones —
compare the two lines to find which one changed). That means you added or changed a
field on Store and need to make a DELIBERATE choice about it, not leave it to this
test to decide by default:

  1. it must cross into transactions: add it to the &Store{...} literal in InTx, or
  2. it must NOT cross into transactions (e.g. it is connection-scoped, a "closed"
     flag, a cached stat): add it to 'want' in this test, right next to pool/db, with
     a comment stating why — same as pool and db already do above.

See docs/specs/2026-08-19-orbeat-intx-field-propagation-design.md §2 for why InTx is
a struct literal on purpose and must stay one: deriving the child (or 'want' here) by
copying the receiver would make this failure impossible to ever see again.

     child Store: %+v
parent-derived want: %+v`, *child, want)
	}
}

// TestInTxRejectsNesting covers the "if s.pool == nil" guard at the top of InTx,
// which store_tx_test.go otherwise never exercises (the commit/rollback tests above
// only ever call InTx on a top-level Store). This matters beyond nesting itself:
// TestInTxPropagatesEveryStoreField's want.pool = nil encodes this guard's
// precondition, so the guard needs its own coverage rather than riding on that
// test's construction alone.
//
// This deliberately does NOT recover from a panic. Without the guard, a tx-bound
// Store's nil pool would make s.pool.Begin(ctx) panic rather than return an error
// (verified empirically against this repo's pinned pgx v5.10.0: Pool.Begin
// dereferences fields on its receiver before it would ever get a chance to check it
// for nil) — recovering that panic here and reporting it as "returned an error"
// would make this test pass whether or not the guard exists, which defeats the
// point of it. If the guard regresses, this test is meant to fail loudly (a panic
// crashing the test binary), not quietly.
func TestInTxRejectsNesting(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// t.Error (not t.Fatal) inside the InTx closure: t.Fatal calls runtime.Goexit,
	// which would unwind the goroutine mid-transaction — before the outer InTx gets
	// to commit — leaking the checked-out connection instead of releasing it
	// cleanly. t.Error records the failure and lets execution continue to the
	// closure's return, so the outer InTx still commits normally.
	fnCalled := false
	err := s.InTx(ctx, func(tx *Store) error {
		nestErr := tx.InTx(ctx, func(*Store) error {
			fnCalled = true
			return nil
		})
		if nestErr == nil {
			t.Error(`InTx on a tx-bound Store returned no error; the nesting guard
("if s.pool == nil" at the top of InTx, store.go) did not fire`)
		}
		if fnCalled {
			t.Error("the nested InTx's fn ran; the nesting guard should have rejected before ever calling fn")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer InTx: %v", err)
	}
}
