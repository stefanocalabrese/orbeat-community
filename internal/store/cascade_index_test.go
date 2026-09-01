package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRoleDeleteCascadeUsesAnIndexOnArtifactEntitlement pins that deleting a
// role does not sequentially scan artifact_entitlement, and it exists because
// the answer changed underneath the repo.
//
// `artifact_entitlement.role_id` is a bare `REFERENCES role(id) ON DELETE
// CASCADE` (migration 00004), so when a role row goes away Postgres' own
// referential-integrity trigger deletes the children keyed on `role_id = $1`,
// with no tenant_id in the predicate. Migration 00012 kept entitlement's bare
// (role_id) index alive for exactly that reason, recorded that
// artifact_entitlement had no equivalent, and measured the consequence: `Seq
// Scan on artifact_entitlement, Rows Removed by Filter: 79996`. It deferred
// the fix as needing its own decision. v1.24.0 then shipped
// `DELETE /v1/admin/roles/{id}`, making that cascade an admin-facing path.
//
// **The decision was taken on 2026-08-26 and it was: add nothing.** Measured
// on postgres:18-alpine at the shape this repo actually deploys (one tenant,
// 200 roles, 10,000 grants, the deleted role owning 50 of them), the cascade
// plans as `Index Scan using artifact_entitlement_tenant_role_id_idx` with
// `Index Cond: (role_id = ...)`, reading 100 index tuples to fetch 50 rows.
// Postgres 18 seeks a multicolumn index whose LEADING column is unconstrained;
// 00012's measurement was taken on Postgres 16, and the 16-to-18 move in this
// same unreleased batch is what retired it. A bare (role_id) index would add a
// second physical copy of an access path that already exists.
//
// **The finding is scoped to that shape, not universal**, measured the same
// day: with 1,000 distinct tenant_id values in the table the planner reverts
// to a Seq Scan, and when the deleted role owns every row a Seq Scan is simply
// the correct plan. Single-tenant-per-instance is a locked design decision, so
// the first shape is the one that ships; a real Phase 5 would have to re-run
// this measurement before trusting it.
//
// EXPLAIN cannot see any of this from the production statement. A cascade runs
// inside the RI trigger, so `EXPLAIN DELETE FROM role ...` reports the parent
// delete and says nothing about the child scan, and EXPLAIN ANALYZE adds only
// a per-trigger TIME, which would make this a test that measures the machine.
// So this reads pg_stat_user_tables instead: seq_scan and idx_scan are exact
// integers moved only by what Postgres actually did.
//
// Two things make those counters trustworthy here:
//
//  1. **One backend.** Pending statistics live in the backend that did the
//     work, and `pg_stat_force_next_flush()` flushes only the backend that
//     calls it. A pooled Store would delete on one connection and flush on
//     whichever it handed out next, so the counter could read as unchanged
//     while a seq scan really happened: a test that passes on the bug. Every
//     statement here runs on ONE acquired connection, through a Store bound to
//     it (`&Store{db: conn}`, the same dbConn seam InTx uses).
//  2. **No parallel siblings.** internal/store shares one container and no
//     test in the package calls t.Parallel, so nothing else touches
//     artifact_entitlement between the two reads.
//
// **What this test cannot catch, stated because it is the whole reason no
// index was added:** it does not discriminate a present bare (role_id) index
// from an absent one. Dropping `artifact_entitlement_tenant_role_id_idx` alone
// leaves it green, because the `UNIQUE (tenant_id, role_id, artifact_id)`
// constraint index serves the same seek. Red-proven only by removing BOTH (the
// index and the constraint), which makes the delete seq-scan three times and
// fires both assertions. So the gate is against losing every role_id-capable
// access path, or against a planner that stops seeking a non-leading column;
// it is not evidence for or against the deferred index.
func TestRoleDeleteCascadeUsesAnIndexOnArtifactEntitlement(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "role", "artifact", "artifact_entitlement")

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire pinned connection: %v", err)
	}
	t.Cleanup(conn.Release)
	pinned := &Store{db: conn}

	const roles, artifacts = 200, 50
	if _, err := conn.Exec(ctx, `
WITH r AS (
  INSERT INTO role (tenant_id, name)
  SELECT $1, 'cascade-role-' || g FROM generate_series(1, $2::int) g
  RETURNING id
), a AS (
  INSERT INTO artifact (tenant_id, type, name, content)
  SELECT $1, 'skill', 'cascade-art-' || g, 'body' FROM generate_series(1, $3::int) g
  RETURNING id
)
INSERT INTO artifact_entitlement (tenant_id, role_id, artifact_id)
SELECT $1, r.id, a.id FROM r CROSS JOIN a`, tn.ID, roles, artifacts); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, table := range []string{"role", "artifact", "artifact_entitlement"} {
		if _, err := conn.Exec(ctx, `ANALYZE `+table); err != nil {
			t.Fatalf("analyze %s: %v", table, err)
		}
	}

	var target string
	if err := conn.QueryRow(ctx,
		`SELECT id::text FROM role WHERE tenant_id = $1 ORDER BY name LIMIT 1`,
		tn.ID).Scan(&target); err != nil {
		t.Fatalf("pick target role: %v", err)
	}

	beforeSeq, beforeIdx := scanCounters(t, conn, "artifact_entitlement")

	// The real production path, not a reconstruction of it: DeleteRole reads
	// the grant names for the audit metadata and then deletes the role row,
	// and the cascade this test exists for fires inside that second statement.
	revoked, err := pinned.DeleteRole(ctx, tn.ID, target)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if revoked.ArtifactEntitlements != artifacts {
		t.Fatalf("revoked %d artifact grants, want %d — the seed is not the shape this test reasons about",
			revoked.ArtifactEntitlements, artifacts)
	}

	afterSeq, afterIdx := scanCounters(t, conn, "artifact_entitlement")

	if afterSeq != beforeSeq {
		t.Errorf("deleting a role sequentially scanned artifact_entitlement %d time(s) (seq_scan %d -> %d).\n"+
			"The RI cascade's `role_id = $1` has no index left that can serve it. Check that "+
			"artifact_entitlement still carries UNIQUE (tenant_id, role_id, artifact_id) and "+
			"artifact_entitlement_tenant_role_id_idx, and that this Postgres still seeks a "+
			"multicolumn index on a non-leading column (measured on 18; not true on 16).",
			afterSeq-beforeSeq, beforeSeq, afterSeq)
	}
	if afterIdx == beforeIdx {
		t.Errorf("deleting a role drove no index scan on artifact_entitlement at all (idx_scan stayed %d). "+
			"Either the cascade found no usable index, or these counters are not reaching the plan at all, "+
			"which would make the seq_scan assertion above vacuous",
			beforeIdx)
	}
}

// scanCounters flushes this backend's pending statistics and returns
// (seq_scan, idx_scan) for table. The flush is what makes the read exact:
// without it Postgres reports whatever it last happened to write out, which is
// a stale number that would let this test pass over a real seq scan.
func scanCounters(t *testing.T, conn *pgxpool.Conn, table string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("force stats flush: %v", err)
	}
	var seq, idx int64
	if err := conn.QueryRow(ctx,
		`SELECT seq_scan, COALESCE(idx_scan, 0) FROM pg_stat_user_tables WHERE relname = $1`,
		table).Scan(&seq, &idx); err != nil {
		t.Fatalf("read scan counters for %s: %v", table, err)
	}
	return seq, idx
}

// inboundFK is one foreign key pointing AT a parent table, as the live
// catalogue reports it, plus this repo's claim about the rows it destroys.
// OnDelete is spelled out rather than left as pg_constraint.confdeltype's
// single character, because "the letter a" is not something a failure message
// can be read against at 2am.
//
// THE CLAIM USED TO BE A SENTENCE INSIDE why, AND THAT WAS A10 ONE LEVEL UP.
// TestInboundForeignKeysOnParentTables pins the SET of foreign keys and
// nothing resolved the reasons beside them, so a migration could add a
// cascading child of role, write "00030: reported as RevokedGrants.Whatever"
// into this map, and go green with the report still incomplete. A free-text
// sentence that a gate has to pattern-match is the weaker design, so the claim
// is now a bool and a field list, resolved against the RevokedGrants struct,
// DeleteRole's body and handleDeleteRole's audit metadata by
// TestReportedCascadeClaimsResolveToCode (cascade_claims_test.go). why is what
// is left over: context for a reader, checked only for being non-empty, which
// is honest about what a prose reason can be worth to a machine.
type inboundFK struct {
	child    string
	columns  string
	onDelete string
	why      string
	// reported says the destroyed rows reach an operator, through
	// store.RevokedGrants and the role.delete audit record. There is no third
	// state and no zero value meaning "undecided", because every entry is
	// built by reportedAs or by unreported and neither lets the question go
	// unanswered. A bare composite literal would have to be keyed (the struct
	// has five fields and no entry writes five values), so the two
	// constructors are the only readable way to add a row here.
	reported bool
	// fields names the RevokedGrants fields that carry those rows. Non-empty
	// exactly when reported is true, which the gate checks rather than
	// assumes: reportedAs(...) with no field names would otherwise be a claim
	// with nothing to resolve.
	fields []string
}

// reportedAs declares a foreign key whose destroyed rows an operator can see,
// naming the store.RevokedGrants fields that carry them.
//
// Each named field must exist on that struct, be assigned by DeleteRole's
// returned literal, AND appear in handleDeleteRole's role.delete audit
// metadata; and DeleteRole must actually read child. All four are checked by
// TestReportedCascadeClaimsResolveToCode against the source of rbac.go and
// internal/api/admin_roles.go, never against this file, so the claim cannot be
// satisfied by its own words.
func reportedAs(child, columns, onDelete, why string, fields ...string) inboundFK {
	return inboundFK{
		child: child, columns: columns, onDelete: onDelete, why: why,
		reported: true, fields: fields,
	}
}

// unreported declares a foreign key whose destroyed rows reach no operator by
// name or by count. That is the ordinary case for every parent here except
// role: a tenant, a user, an artifact and an mcp_server have no equivalent of
// RevokedGrants, and no audit finding says they need one.
//
// It is REFUSED for a cascading child of role, because that combination is
// exactly the state A10 found and the state this map exists to make
// impossible. Removing that refusal is a deliberate edit to the gate with a
// written reason, not a value someone can pick in this map.
func unreported(child, columns, onDelete, why string) inboundFK {
	return inboundFK{child: child, columns: columns, onDelete: onDelete, why: why}
}

// inboundForeignKeys is THE ROOT-CAUSE FIX FOR A10, and the finding it closes
// is not "virtual keys were unreported" but "nothing in this repo knew how
// many children a parent table had".
//
// Two migrations added a cascading child of role (00020's virtual_key, 00022's
// usage_daily and role_quota) with the entire suite green, because every
// schema assertion in this repo looks at the CHILD side. internal/migrate's
// TestArtifactDeploymentSchemaAndDownUpRoundTrip reads the FKs OF
// artifact_deployment; nothing anywhere read the FKs pointing AT anything. So
// store.RevokedGrants went on describing two of role's five children for two
// releases, and the doc comment asserting it described all of them stayed
// where it was.
//
// This map is the written-down answer, one entry per foreign key, keyed on the
// constraint name because that is the only identifier Postgres guarantees is
// unique and stable. Adding a child of any parent listed here fails
// TestInboundForeignKeysOnParentTables until someone writes the new
// constraint into this map, and for a cascading child of role the only entry
// that gate accepts is a reportedAs(...) naming RevokedGrants fields that
// TestReportedCascadeClaimsResolveToCode can find in DeleteRole and in the
// role.delete audit record.
//
// It lives in internal/store rather than internal/migrate, where the other
// schema gates are, for two reasons. internal/store's TestMain already applies
// the full migration chain to a shared container, so this costs no new
// container; and every action a failure here demands (report the new child in
// RevokedGrants, or record why it needs no report) is taken in THIS package,
// where a reader who trips the gate is already standing.
var inboundForeignKeys = map[string]map[string]inboundFK{
	// role is the parent this gate was written for. Deleting one cascades
	// through all seven of these, and DeleteRole must report every child.
	"role": {
		"entitlement_role_id_fkey": reportedAs(
			"entitlement", "role_id", "cascade",
			"00001: the original server-grant FK",
			"Entitlements", "ServerNames"),
		"entitlement_role_tenant_fk": reportedAs(
			"entitlement", "tenant_id,role_id", "cascade",
			"00010: the composite-tenant backstop, added not replaced, so it "+
				"destroys the rows the FK above already accounts for",
			"Entitlements", "ServerNames"),
		"artifact_entitlement_role_id_fkey": reportedAs(
			"artifact_entitlement", "role_id", "cascade",
			"00004: the original artifact-grant FK",
			"ArtifactEntitlements", "ArtifactNames"),
		"artifact_entitlement_role_tenant_fk": reportedAs(
			"artifact_entitlement", "tenant_id,role_id", "cascade",
			"00010: the composite-tenant backstop for the same rows",
			"ArtifactEntitlements", "ArtifactNames"),
		"virtual_key_role_tenant_fk": reportedAs(
			"virtual_key", "tenant_id,role_id", "cascade",
			"00020. Unreported until A10, which is what made a role deletion "+
				"silently kill every robot credential capped by it and orphan "+
				"its Keycloak client",
			"VirtualKeys", "VirtualKeyClientIDs"),
		"usage_daily_role_tenant_fk": reportedAs(
			"usage_daily", "tenant_id,role_id", "cascade",
			"00022. Unreported until A10",
			"UsageRows", "UsageCalls"),
		"role_quota_role_tenant_fk": reportedAs(
			"role_quota", "tenant_id,role_id", "cascade",
			"00022. Unreported until A10",
			"QuotaMonthlyCalls"),
	},
	// tenant is the widest parent in the schema and the one where an
	// unnoticed new child matters least per row and most in aggregate: it is
	// the single-tenant-to-SaaS seam, so this list is also the answer to
	// "what does a tenant own". No handler deletes a tenant today; the test
	// fixtures do (cleanupTenant), which is why the cascades exist.
	"tenant": {
		"role_tenant_id_fkey":                 unreported("role", "tenant_id", "cascade", "00001"),
		"users_tenant_id_fkey":                unreported("users", "tenant_id", "cascade", "00001"),
		"mcp_server_tenant_id_fkey":           unreported("mcp_server", "tenant_id", "cascade", "00001"),
		"entitlement_tenant_id_fkey":          unreported("entitlement", "tenant_id", "cascade", "00001"),
		"audit_event_tenant_id_fkey":          unreported("audit_event", "tenant_id", "cascade", "00001"),
		"artifact_tenant_id_fkey":             unreported("artifact", "tenant_id", "cascade", "00003"),
		"artifact_entitlement_tenant_id_fkey": unreported("artifact_entitlement", "tenant_id", "cascade", "00004"),
		"artifact_revision_tenant_id_fkey":    unreported("artifact_revision", "tenant_id", "cascade", "00007"),
		"publish_state_tenant_id_fkey":        unreported("publish_state", "tenant_id", "cascade", "00008"),
		"artifact_deployment_tenant_id_fkey":  unreported("artifact_deployment", "tenant_id", "cascade", "00017"),
		"virtual_key_tenant_id_fkey":          unreported("virtual_key", "tenant_id", "cascade", "00020"),
		"usage_daily_tenant_id_fkey":          unreported("usage_daily", "tenant_id", "cascade", "00022"),
		"role_quota_tenant_id_fkey":           unreported("role_quota", "tenant_id", "cascade", "00022"),
	},
	// users is the one parent here whose children disagree about ON DELETE,
	// and the disagreement is deliberate on both sides. Nothing in the
	// product deletes a user row today (SCIM deprovisioning sets
	// users.deactivated_at, 00021), so these fire only via a tenant cascade.
	"users": {
		"virtual_key_created_by_fkey": unreported(
			"virtual_key", "created_by", "set null",
			"00020: SET NULL, not cascade. A key is owned by a ROLE, which outlives "+
				"people; deleting the admin who minted it must not delete the "+
				"credential a CI job authenticates with"),
		"artifact_deployment_user_id_fkey": unreported(
			"artifact_deployment", "user_id", "cascade",
			"00017: cascade. A deployment row is a fact about one person's install; "+
				"with the person gone it identifies nobody and the registry's own "+
				"privacy posture says it holds five values and no sixth"),
	},
	// artifact: deleting one takes its grants, its revision chain and its
	// deployment rows with it. DeleteArtifact reports none of these, and no
	// audit finding says it must; the point of the entry is that a SIXTH
	// child would have to be argued for rather than merged.
	"artifact": {
		"artifact_entitlement_artifact_id_fkey":   unreported("artifact_entitlement", "artifact_id", "cascade", "00004"),
		"artifact_entitlement_artifact_tenant_fk": unreported("artifact_entitlement", "tenant_id,artifact_id", "cascade", "00010: composite-tenant backstop"),
		"artifact_revision_artifact_id_fkey":      unreported("artifact_revision", "artifact_id", "cascade", "00007"),
		"artifact_revision_artifact_tenant_fk":    unreported("artifact_revision", "tenant_id,artifact_id", "cascade", "00032: composite-tenant backstop (audit B37), added not replaced, so it destroys the rows the FK above already accounts for"),
		"artifact_deployment_artifact_id_fkey":    unreported("artifact_deployment", "artifact_id", "cascade", "00017"),
		"artifact_deployment_artifact_tenant_fk":  unreported("artifact_deployment", "tenant_id,artifact_id", "cascade", "00034: composite-tenant backstop, added not replaced, so it destroys the rows the FK above already accounts for"),
	},
	// mcp_server: deleting one revokes every entitlement naming it and
	// erases the metering attributed to it.
	"mcp_server": {
		"entitlement_mcp_server_id_fkey":   unreported("entitlement", "mcp_server_id", "cascade", "00001"),
		"entitlement_mcp_server_tenant_fk": unreported("entitlement", "tenant_id,mcp_server_id", "cascade", "00010: composite-tenant backstop"),
		"usage_daily_server_tenant_fk":     unreported("usage_daily", "tenant_id,server_id", "cascade", "00022"),
	},
}

// readInboundFKs asks the live catalogue which foreign keys point at parent.
// It takes a dbConn rather than a *Store so the positive control below can run
// the identical derivation inside an open transaction that has created a
// throwaway child.
//
// confdeltype is translated here and not compared as a raw character: 'a' and
// 'c' differ by one byte in a failure message and by everything in effect.
func readInboundFKs(t *testing.T, db dbConn, parent string) map[string]inboundFK {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `
		SELECT c.conname,
		       c.conrelid::regclass::text,
		       string_agg(a.attname, ',' ORDER BY k.ord),
		       CASE c.confdeltype
		           WHEN 'c' THEN 'cascade'
		           WHEN 'n' THEN 'set null'
		           WHEN 'd' THEN 'set default'
		           WHEN 'r' THEN 'restrict'
		           WHEN 'a' THEN 'no action'
		           ELSE 'UNKNOWN:' || c.confdeltype::text
		       END
		FROM pg_constraint c
		JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.contype = 'f' AND c.confrelid = to_regclass($1)
		GROUP BY c.oid, c.conname, c.conrelid, c.confdeltype`, parent)
	if err != nil {
		t.Fatalf("read inbound FKs on %s: %v", parent, err)
	}
	defer rows.Close()
	got := map[string]inboundFK{}
	for rows.Next() {
		var name string
		var fk inboundFK
		if err := rows.Scan(&name, &fk.child, &fk.columns, &fk.onDelete); err != nil {
			t.Fatalf("scan inbound FK on %s: %v", parent, err)
		}
		got[name] = fk
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("inbound FK rows on %s: %v", parent, err)
	}
	return got
}

// TestInboundForeignKeysOnParentTables pins, per parent table, the exact set
// of foreign keys pointing at it, derived from the live schema after all
// migrations rather than from a hand-read of the migration files. See
// inboundForeignKeys above for what this closes and why it lives here.
//
// THE VACUITY GUARDS ARE NOT DECORATION. A gate of this shape has two ways to
// pass while measuring nothing, and this repo has shipped both before: the
// parent table name could be misspelled, in which case to_regclass returns
// NULL and the query returns zero rows that compare equal to a zero-entry
// expectation; or the expectation map could be emptied and the comparison
// would agree with an unqueried database. So the parent must resolve, its
// expected set must be non-empty, and its derived set must be non-empty, each
// checked separately and each with its own message. TestNewCascadingChild-
// IsDetected below is the third guard and the strongest: it proves the
// derivation actually notices a new child.
func TestInboundForeignKeysOnParentTables(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for parent, want := range inboundForeignKeys {
		t.Run(parent, func(t *testing.T) {
			var exists bool
			if err := s.db.QueryRow(ctx,
				`SELECT to_regclass($1) IS NOT NULL`, parent).Scan(&exists); err != nil {
				t.Fatalf("resolve %s: %v", parent, err)
			}
			if !exists {
				t.Fatalf("no table named %q exists after migrations: either it was renamed "+
					"(update inboundForeignKeys) or this key is a typo, in which case every "+
					"assertion below this line was measuring an empty result", parent)
			}
			if len(want) == 0 {
				t.Fatalf("inboundForeignKeys[%q] is empty; an empty expectation matches an "+
					"unqueried database and proves nothing", parent)
			}

			got := readInboundFKs(t, s.db, parent)
			if len(got) == 0 {
				t.Fatalf("the live schema reports ZERO foreign keys pointing at %s, "+
					"but %d are expected: the query is not reaching the catalogue",
					parent, len(want))
			}

			for name, w := range want {
				g, ok := got[name]
				if !ok {
					t.Errorf("constraint %q on %s is expected but GONE from the schema. "+
						"If a migration dropped it, drop this entry too; if it was renamed, "+
						"rename it here. Expected: %s(%s) ON DELETE %s -- %s",
						name, parent, w.child, w.columns, w.onDelete, w.why)
					continue
				}
				if g.child != w.child || g.columns != w.columns || g.onDelete != w.onDelete {
					t.Errorf("constraint %q on %s changed shape:\n  live:     %s(%s) ON DELETE %s\n"+
						"  expected: %s(%s) ON DELETE %s\n  reason on record: %s",
						name, parent, g.child, g.columns, g.onDelete,
						w.child, w.columns, w.onDelete, w.why)
				}
			}
			for name, g := range got {
				if _, ok := want[name]; ok {
					continue
				}
				t.Errorf("UNDECLARED foreign key %q: %s(%s) ON DELETE %s now points at %s.\n"+
					"%s\nEither way, add it to inboundForeignKeys[%q] with a reason. "+
					"This gate exists because migrations 00020 and 00022 each did exactly "+
					"this to role and nothing failed for two releases.",
					name, g.child, g.columns, g.onDelete, parent,
					newChildInstruction(parent, g.onDelete), parent)
			}
		})
	}
}

// newChildInstruction is the actionable half of the failure above: what a
// reader is supposed to DO about a new child, which differs by parent because
// only role has a report that must stay complete.
func newChildInstruction(parent, onDelete string) string {
	if parent == "role" && onDelete == "cascade" {
		return "Deleting a role now destroys rows in this table too. DECIDE FIRST whether " +
			"store.RevokedGrants must report them (rbac.go), and if so add the read to " +
			"DeleteRole and the key to handleDeleteRole's role.delete audit metadata " +
			"(internal/api/admin_roles.go) -- an operator cannot answer \"why did this " +
			"break?\" from a record that does not mention it."
	}
	if onDelete == "cascade" {
		return "Deleting a " + parent + " row now destroys rows in this table too. " +
			"Check that whatever deletes a " + parent + " says so."
	}
	return "Its ON DELETE is not cascade, so confirm that is deliberate."
}

// TestNewCascadingChildIsDetected is the positive control for the gate above,
// and it is the assertion that makes the rest non-vacuous: it proves the
// derivation SEES a newly added cascading child of role, rather than trusting
// that it would.
//
// The throwaway table is created inside a transaction that is always rolled
// back, so it never reaches the shared container's committed state. That is
// safe here specifically because internal/store runs no test in parallel (see
// TestRoleDeleteCascadeUsesAnIndexOnArtifactEntitlement's note on the same
// container), and the ACCESS EXCLUSIVE lock the FK takes on role would
// otherwise stall a concurrent sibling.
func TestNewCascadingChildIsDetected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	before := readInboundFKs(t, tx, "role")
	if _, ok := before["redproof_child_role_fk"]; ok {
		t.Fatal("redproof_child_role_fk already exists; a previous run leaked its fixture")
	}

	if _, err := tx.Exec(ctx, `
		CREATE TABLE redproof_child (
			id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			role_id uuid NOT NULL,
			CONSTRAINT redproof_child_role_fk
				FOREIGN KEY (role_id) REFERENCES role (id) ON DELETE CASCADE
		)`); err != nil {
		t.Fatalf("create throwaway child: %v", err)
	}

	after := readInboundFKs(t, tx, "role")
	fk, ok := after["redproof_child_role_fk"]
	if !ok {
		t.Fatalf("the derivation did not see a brand new cascading child of role. "+
			"Every assertion in TestInboundForeignKeysOnParentTables is therefore "+
			"incapable of catching the thing it exists for. Saw: %v", after)
	}
	if fk.child != "redproof_child" || fk.columns != "role_id" || fk.onDelete != "cascade" {
		t.Errorf("derived %s(%s) ON DELETE %s, want redproof_child(role_id) ON DELETE cascade",
			fk.child, fk.columns, fk.onDelete)
	}
	if _, declared := inboundForeignKeys["role"]["redproof_child_role_fk"]; declared {
		t.Fatal("redproof_child_role_fk is declared in inboundForeignKeys; " +
			"the fixture is supposed to be UNdeclared, otherwise this proves nothing " +
			"about the undeclared-child branch")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback the fixture: %v", err)
	}
	var leaked bool
	if err := s.db.QueryRow(ctx,
		`SELECT to_regclass('redproof_child') IS NOT NULL`).Scan(&leaked); err != nil {
		t.Fatalf("check for a leaked fixture: %v", err)
	}
	if leaked {
		t.Error("redproof_child survived the rollback and is now in the shared container")
	}
}
