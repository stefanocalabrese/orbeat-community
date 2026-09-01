package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestUpCreatesTables(t *testing.T) {
	ctx := context.Background()

	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				// Postgres opens the port, then restarts during init; wait for the
				// readiness log to appear twice so we don't connect mid-restart.
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	wantTables := []string{"tenant", "users", "role", "mcp_server", "entitlement", "audit_event"}
	for _, name := range wantTables {
		var exists bool
		err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			name,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %q: %v", name, err)
		}
		if !exists {
			t.Errorf("table %q was not created", name)
		}
	}
}

func TestArtifactRevisionBackfill(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-backfill-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before the revision migration (00006).
	if err := goose.UpTo(db, "migrations", 6); err != nil {
		t.Fatalf("up to 6: %v", err)
	}

	// Seed a tenant + an APPROVED artifact (as it would exist pre-00007).
	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('bf') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var artifactID string
	if err := db.QueryRow(`
		INSERT INTO artifact (tenant_id, type, name, content, approval_state, approved_content, approved_by, approved_at)
		VALUES ($1,'skill','bf-skill','WORKING','approved','FROZEN','carol', now())
		RETURNING id::text`, tenantID).Scan(&artifactID); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	// Apply 00007 (table + backfill).
	if err := goose.UpTo(db, "migrations", 7); err != nil {
		t.Fatalf("up to 7: %v", err)
	}

	var num int
	var content, source, approvedBy string
	err = db.QueryRow(`SELECT revision_num, content, source, approved_by
		FROM artifact_revision WHERE artifact_id=$1`, artifactID).
		Scan(&num, &content, &source, &approvedBy)
	if err != nil {
		t.Fatalf("query backfilled revision: %v", err)
	}
	if num != 1 || content != "FROZEN" || source != "approval" || approvedBy != "carol" {
		t.Fatalf("backfill wrong: num=%d content=%q source=%q by=%q", num, content, source, approvedBy)
	}
}

// TestIndexSetsAfterFullMigration pins the exact index set on the artifact,
// audit_event, entitlement, and artifact_entitlement tables after every
// migration runs. It is the regression net for 00010, which (a) restores the
// distribution index 00006 silently destroyed and (b) replaces the audit
// keyset index with one carrying the id tiebreak; and for 00012, which adds
// the admin-list pagination keyset indexes on artifact and entitlement, and
// — its only destructive act — replaces artifact_entitlement's index rather
// than just adding to it. Exact-set equality is deliberately used everywhere,
// including on entitlement/artifact_entitlement: it subsumes both "the new
// index exists" and "the old one is gone" in a single assertion, and it is
// pinned exactly because 00012 destroys one of these (a weaker
// present/absent check on just the changed names would miss a stray index
// neither assertion is looking for). Pinning the FULL set — mirroring how
// TestMCPServerStatusNormalization pins 00009 by querying live state — means
// a future migration that drops or renames one of these without updating the
// test fails loudly.
func TestIndexSetsAfterFullMigration(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-index-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	indexNames := func(table string) []string {
		rows, err := db.Query(`SELECT indexname FROM pg_indexes WHERE tablename=$1 ORDER BY indexname`, table)
		if err != nil {
			t.Fatalf("query indexes for %q: %v", table, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan index name: %v", err)
			}
			got = append(got, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows err: %v", err)
		}
		sort.Strings(got)
		return got
	}

	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// artifact: PK + the (tenant_id,type,name) uniqueness + the new composite-FK
	// target UNIQUE(tenant_id,id) + the restored distributable partial index +
	// 00012's two pagination keyset indexes (unfiltered and state-filtered
	// admin-list order) + 00016's uniqueness on the DISTRIBUTED identity.
	// Critically: 00003's `artifact_tenant_active_idx` must be ABSENT (00006's
	// DROP COLUMN status took it), superseded by
	// artifact_tenant_distributable_idx.
	wantArtifact := []string{
		"artifact_pkey",
		"artifact_tenant_approved_identity_uniq",
		"artifact_tenant_distributable_idx",
		"artifact_tenant_id_type_name_key",
		"artifact_tenant_id_uniq",
		"artifact_tenant_state_type_name_id_idx",
		"artifact_tenant_type_name_id_idx",
	}
	if got := indexNames("artifact"); !equal(got, wantArtifact) {
		t.Fatalf("artifact indexes = %v, want %v", got, wantArtifact)
	}

	// Names alone cannot see 00016 step 6, which REPLACES
	// artifact_tenant_distributable_idx under the same name: the set assertion
	// above is identical before and after that change. Pin the definitions of
	// the two indexes 00016 owns, keyed on the substrings that differ between
	// the old shape and the new one.
	indexDef := func(name string) string {
		var def string
		if err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname=$1`, name).Scan(&def); err != nil {
			t.Fatalf("indexdef for %q: %v", name, err)
		}
		return def
	}
	for _, tc := range []struct {
		index string
		want  []string
		why   string
	}{
		{
			index: "artifact_tenant_distributable_idx",
			want:  []string{"(tenant_id, approved_visibility)", "WHERE (approved_content IS NOT NULL)"},
			why: "00010 keyed it on the LIVE visibility; the distributable set is selected by " +
				"approved_visibility now, so the old key would be maintained on every write and serve nothing",
		},
		{
			index: "artifact_tenant_approved_identity_uniq",
			want: []string{"CREATE UNIQUE INDEX", "(tenant_id, approved_type, approved_name)",
				"WHERE (approved_content IS NOT NULL)"},
			why: "it must be UNIQUE, keyed on the distributed identity, and PARTIAL: without the " +
				"predicate a withdrawn artifact would keep holding its name against every other row",
		},
	} {
		def := indexDef(tc.index)
		for _, want := range tc.want {
			if !strings.Contains(def, want) {
				t.Errorf("%s is defined as %q, want it to contain %q: %s", tc.index, def, want, tc.why)
			}
		}
	}

	// audit_event: PK + the keyset index WITH the id tiebreak, + the ts index
	// for the retention prune's `ts < cutoff` filter (00011), + one index per
	// SELECTIVE admin filter (00023). The old (tenant_id, ts DESC)-only
	// `audit_event_tenant_ts_idx` must be gone, and there is deliberately no
	// index on `decision`: three CHECK-constrained values can never be
	// selective enough to seq-scan, so 00023 measured that one and left it out.
	wantAudit := []string{
		"audit_event_pkey",
		"audit_event_tenant_action_ts_id_idx",
		"audit_event_tenant_actor_ts_id_idx",
		"audit_event_tenant_ts_id_idx",
		"audit_event_ts_idx",
	}
	if got := indexNames("audit_event"); !equal(got, wantAudit) {
		t.Fatalf("audit_event indexes = %v, want %v", got, wantAudit)
	}

	// entitlement: PK + 00001's UNIQUE(tenant_id,role_id,mcp_server_id) +
	// 00001's entitlement_role_idx (bare role_id — deliberately KEPT by
	// 00012, since it alone serves the role_id ON DELETE CASCADE, which
	// deletes by bare `role_id = X` and cannot use a tenant_id-leading index)
	// + 00012's new tenant-scoped pagination keyset index.
	wantEntitlement := []string{
		"entitlement_pkey",
		"entitlement_role_idx",
		"entitlement_tenant_id_role_id_mcp_server_id_key",
		"entitlement_tenant_role_id_idx",
	}
	if got := indexNames("entitlement"); !equal(got, wantEntitlement) {
		t.Fatalf("entitlement indexes = %v, want %v", got, wantEntitlement)
	}

	// artifact_entitlement: PK + 00004's UNIQUE(tenant_id,role_id,artifact_id)
	// + 00012's new tenant-scoped pagination keyset index. Critically:
	// 00004's `artifact_entitlement_role_idx` (tenant_id, role_id) must be
	// ABSENT — 00012 DROPs and replaces it, since unlike entitlement_role_idx
	// above it was already tenant-leading and so never served the FK cascade
	// (a bare role_id lookup); (tenant_id, role_id) is a strict prefix of the
	// replacement, making the old index pure redundant write cost.
	wantArtifactEntitlement := []string{
		"artifact_entitlement_pkey",
		"artifact_entitlement_tenant_id_role_id_artifact_id_key",
		"artifact_entitlement_tenant_role_id_idx",
	}
	if got := indexNames("artifact_entitlement"); !equal(got, wantArtifactEntitlement) {
		t.Fatalf("artifact_entitlement indexes = %v, want %v", got, wantArtifactEntitlement)
	}

	// virtual_key: PK + 00020's virtual_key_client_id_uniq (tenant_id,
	// client_id) and virtual_key_tenant_role_id_idx (tenant_id, role_id) +
	// 00029's new (tenant_id, name, id) pagination keyset index, added by
	// that slice (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 2)
	// because, unlike role/mcp_server, virtual_key's default (name, id)
	// sort had NO index at all before it, name is not part of any UNIQUE
	// constraint here (only (tenant_id, client_id) is).
	//
	// virtual_key_lookup (00020, also (tenant_id, client_id)) is DELIBERATELY
	// absent from this set: 00033 dropped it as a genuine duplicate of
	// virtual_key_client_id_uniq's own auto-generated unique index — same
	// columns, same order — costing a second B-tree insert per write for
	// zero read benefit (audit B37). This is the one item of that finding
	// with a measurable cost, and this assertion is exactly what proves the
	// drop landed: it failed with virtual_key_lookup still present the
	// moment 00033 was written but before it ran, and now fails the other
	// way if a future migration accidentally reintroduces it.
	wantVirtualKey := []string{
		"virtual_key_client_id_uniq",
		"virtual_key_pkey",
		"virtual_key_tenant_name_id_idx",
		"virtual_key_tenant_role_id_idx",
	}
	if got := indexNames("virtual_key"); !equal(got, wantVirtualKey) {
		t.Fatalf("virtual_key indexes = %v, want %v", got, wantVirtualKey)
	}
}

// TestMCPServerStatusNormalization pins 00009's own claim: normalizing
// legacy status values is LOSSLESS FOR VISIBILITY (every non-'active' row was
// already hidden from the catalog+gateway, and 'disabled' now means exactly
// that), even though the distinct original values themselves are not
// recoverable. Seeds rows with a representative set of legacy values BEFORE
// 00009 runs, then asserts 'active' is preserved and everything else became
// 'disabled'.
func TestMCPServerStatusNormalization(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-status-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before the status-check migration (00009).
	if err := goose.UpTo(db, "migrations", 8); err != nil {
		t.Fatalf("up to 8: %v", err)
	}

	// Seed a tenant + mcp_server rows with a representative set of legacy
	// status values (as they could exist pre-00009: no CHECK constrains them
	// yet). 'active' must survive untouched; everything else must normalize
	// to 'disabled'.
	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('status-norm') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	legacy := map[string]string{
		"srv-active":      "active",
		"srv-inactive":    "inactive",
		"srv-Active":      "Active",
		"srv-maintenance": "maintenance",
		"srv-empty":       "",
	}
	for name, status := range legacy {
		if _, err := db.Exec(`
			INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
			VALUES ($1,$2,'http','https://x',$3)`, tenantID, name, status); err != nil {
			t.Fatalf("seed mcp_server %q status=%q: %v", name, status, err)
		}
	}

	// Apply 00009 (normalize + CHECK).
	if err := goose.UpTo(db, "migrations", 9); err != nil {
		t.Fatalf("up to 9: %v", err)
	}

	want := map[string]string{
		"srv-active":      "active",
		"srv-inactive":    "disabled",
		"srv-Active":      "disabled",
		"srv-maintenance": "disabled",
		"srv-empty":       "disabled",
	}
	for name, wantStatus := range want {
		var gotStatus string
		if err := db.QueryRow(`SELECT status FROM mcp_server WHERE tenant_id=$1 AND name=$2`, tenantID, name).Scan(&gotStatus); err != nil {
			t.Fatalf("query %q: %v", name, err)
		}
		if gotStatus != wantStatus {
			t.Errorf("%s: status = %q, want %q", name, gotStatus, wantStatus)
		}
	}
}

// TestRowVersionDownUpRoundTrip exercises 00013's `down` block, which
// otherwise had ZERO coverage when this was written. That sentence used to
// read "goose.Down/DownTo appears nowhere else in this repo", which is false
// (audit C9): there are six call sites, all in THIS file, exercising the down
// blocks of 00012, 00015, 00016, 00017, 00018 and 00027. Every other
// migration's down block is still unexecuted by any test, which is the point
// that actually matters and the one the false claim was standing in for. That matters
// most for 00013 specifically — it is the only migration that drops a
// function shared by two triggers, and its own comment asserts a correctness
// property ("both triggers must go BEFORE the shared function, or the DROP
// fails on the dependency") that nothing previously verified.
//
// Asserts object counts (row_version columns / triggers / bump_row_version
// function) across the full cycle: 2/2/1 -> 0/0/0 -> 2/2/1 -> 0/0/0 -> 2/2/1
// — i.e. the down/up cycle run TWICE. A leftover object from an incomplete
// down would only surface on the *second* up, as a goose "already exists"
// failure, not the first.
func TestRowVersionDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-rowversion-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// counts returns (row_version columns on artifact+mcp_server, bump
	// triggers on artifact+mcp_server, bump_row_version functions).
	counts := func(step string) (cols, triggers, fns int) {
		if err := db.QueryRow(`
			SELECT count(*) FROM information_schema.columns
			WHERE table_name IN ('artifact','mcp_server') AND column_name = 'row_version'
		`).Scan(&cols); err != nil {
			t.Fatalf("%s: count columns: %v", step, err)
		}
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_trigger
			WHERE tgname IN ('artifact_bump_row_version','mcp_server_bump_row_version')
		`).Scan(&triggers); err != nil {
			t.Fatalf("%s: count triggers: %v", step, err)
		}
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_proc WHERE proname = 'bump_row_version'
		`).Scan(&fns); err != nil {
			t.Fatalf("%s: count functions: %v", step, err)
		}
		return cols, triggers, fns
	}

	assertCounts := func(step string, wantCols, wantTriggers, wantFns int) {
		cols, triggers, fns := counts(step)
		if cols != wantCols || triggers != wantTriggers || fns != wantFns {
			t.Fatalf("%s: columns/triggers/functions = %d/%d/%d, want %d/%d/%d",
				step, cols, triggers, fns, wantCols, wantTriggers, wantFns)
		}
	}

	// Migrate up to (and including) 12 first, so the baseline has none of
	// 00013's objects — isolates what 00013 itself adds/removes.
	if err := goose.UpTo(db, "migrations", 12); err != nil {
		t.Fatalf("up to 12: %v", err)
	}
	assertCounts("baseline (v12)", 0, 0, 0)

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 13); err != nil {
			t.Fatalf("cycle %d: up to 13: %v", cycle, err)
		}
		assertCounts(fmt.Sprintf("cycle %d: after up", cycle), 2, 2, 1)

		if err := goose.DownTo(db, "migrations", 12); err != nil {
			t.Fatalf("cycle %d: down to 12: %v", cycle, err)
		}
		assertCounts(fmt.Sprintf("cycle %d: after down", cycle), 0, 0, 0)
	}

	// Leave the database at 13 so this test's shape matches every other test
	// in this file (all end migrated to latest).
	if err := goose.UpTo(db, "migrations", 13); err != nil {
		t.Fatalf("final up to 13: %v", err)
	}
	assertCounts("final (v13)", 2, 2, 1)
}

// TestUsersLastSeenBackfill pins 00015's own claim: an EXISTING user row (as
// it could exist pre-00015, with no last_seen_at column at all) is backfilled
// to approximately now() when the column is added, not left null. This
// matters because the Community seat cap (docs/specs/2026-08-19-orbeat-
// community-caps-design.md sec 3.2) treats a null/never-set last_seen_at as
// "not an active seat": a null backfill would silently lock every existing
// user out of the seat count on the very upgrade meant to introduce it.
func TestUsersLastSeenBackfill(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-lastseen-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before the last_seen_at migration (00015).
	if err := goose.UpTo(db, "migrations", 14); err != nil {
		t.Fatalf("up to 14: %v", err)
	}

	// Seed a tenant + a user row as it would exist pre-00015 (no
	// last_seen_at column exists yet at this schema version).
	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('lastseen-bf') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var userID string
	if err := db.QueryRow(`
		INSERT INTO users (tenant_id, subject, email, display_name)
		VALUES ($1, 'pre-existing-subject', 'a@x.io', 'Alice')
		RETURNING id::text`, tenantID).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	before := time.Now()
	// Apply 00015 (adds the column + backfill).
	if err := goose.UpTo(db, "migrations", 15); err != nil {
		t.Fatalf("up to 15: %v", err)
	}
	after := time.Now()

	var lastSeenAt sql.NullTime
	if err := db.QueryRow(`SELECT last_seen_at FROM users WHERE id = $1`, userID).Scan(&lastSeenAt); err != nil {
		t.Fatalf("query backfilled last_seen_at: %v", err)
	}
	if !lastSeenAt.Valid {
		t.Fatal("backfilled last_seen_at is NULL, want a timestamp: a null value locks a pre-existing user out of the seat count")
	}
	if lastSeenAt.Time.Before(before.Add(-time.Second)) || lastSeenAt.Time.After(after.Add(time.Second)) {
		t.Fatalf("backfilled last_seen_at = %s, want within [%s, %s] (the migration's own now())", lastSeenAt.Time, before, after)
	}
}

// TestUsersDeactivatedAtUnaffectedOnUpgrade is the deliberate OPPOSITE of
// TestUsersLastSeenBackfill above: 00021 adds users.deactivated_at with no
// DEFAULT and no backfill, on purpose (docs/specs/2026-08-25-orbeat-scim-
// design.md sec 2, migration 00021's own comment) -- nobody has ever been
// SCIM-deactivated before this column existed, so NULL ("active") is the
// correct value for every row that already exists, unlike 00015's
// last_seen_at where NULL would have meant "never active" and locked
// everyone out. An EXISTING row (seeded pre-00021, as it could exist in any
// upgraded deployment) must resolve exactly as it did before the upgrade.
//
// This is the red-proof for gate 3 of docs/plans/orbeat-scim-2026-08-25.md
// Task 1: a mutated migration reading `deactivated_at timestamptz NOT NULL
// DEFAULT now()` would deactivate every pre-existing user on upgrade, and
// this test's Valid assertion catches exactly that (verified by hand: with
// the column defaulted to now(), deactivatedAt.Valid flips true and the
// t.Fatal fires).
func TestUsersDeactivatedAtUnaffectedOnUpgrade(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-deactivated-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before 00021.
	if err := goose.UpTo(db, "migrations", 20); err != nil {
		t.Fatalf("up to 20: %v", err)
	}

	// Seed a tenant + a user row as it would exist pre-00021 (no
	// deactivated_at column exists yet at this schema version).
	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('deactivated-upgrade') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var userID string
	if err := db.QueryRow(`
		INSERT INTO users (tenant_id, subject, email, display_name)
		VALUES ($1, 'pre-existing-subject', 'a@x.io', 'Alice')
		RETURNING id::text`, tenantID).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Apply 00021 (adds the column, no default, no backfill).
	if err := goose.UpTo(db, "migrations", 21); err != nil {
		t.Fatalf("up to 21: %v", err)
	}

	var deactivatedAt sql.NullTime
	if err := db.QueryRow(`SELECT deactivated_at FROM users WHERE id = $1`, userID).Scan(&deactivatedAt); err != nil {
		t.Fatalf("query deactivated_at: %v", err)
	}
	if deactivatedAt.Valid {
		t.Fatalf("deactivated_at = %s on a pre-existing row after the 00021 upgrade, want NULL: "+
			"a non-null default would deactivate every existing user on upgrade", deactivatedAt.Time)
	}
}

// TestArtifactApprovedIdentityBackfill pins 00016 step 3's claim: an artifact
// that was already distributing when the migration ran keeps distributing
// under the same identity, so the upgrade moves no file on any developer
// machine.
//
// The rows are seeded BEFORE 00016 exists, and that is the only arrangement
// that can observe a backfill at all. A row approved after the migration gets
// its approved identity from SetArtifactApproved's UPDATE, so a test built on
// one would pass with the backfill statement deleted.
//
// It also pins the deliberate NON-backfill of artifact_revision (spec §3.2).
// artifact_revision has never recorded what an artifact was called at an
// earlier revision, so writing today's name into 00007's grandfathered
// revision 1 would put an unverified claim into an append-only governance
// record. NULL there means "approved before 00016, identity not recorded".
func TestArtifactApprovedIdentityBackfill(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-approved-identity-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before the approved-identity migration (00016).
	if err := goose.UpTo(db, "migrations", 15); err != nil {
		t.Fatalf("up to 15: %v", err)
	}

	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('ident-bf') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// An artifact already in distribution, plus a draft that is not. The draft
	// is what proves the backfill is scoped by approved_content IS NOT NULL
	// rather than applied to every row.
	var approvedID string
	if err := db.QueryRow(`
		INSERT INTO artifact (tenant_id, type, name, content, visibility, approval_state, approved_content, approved_by, approved_at)
		VALUES ($1,'subagent','bf-live','WORKING','role','approved','FROZEN','carol', now())
		RETURNING id::text`, tenantID).Scan(&approvedID); err != nil {
		t.Fatalf("seed approved artifact: %v", err)
	}
	var draftID string
	if err := db.QueryRow(`
		INSERT INTO artifact (tenant_id, type, name, content, visibility)
		VALUES ($1,'skill','bf-draft','WORKING','org')
		RETURNING id::text`, tenantID).Scan(&draftID); err != nil {
		t.Fatalf("seed draft artifact: %v", err)
	}
	// 00007 grandfathered every approved artifact as revision 1, but it did so
	// before this migration existed, so that row carries no identity.
	var revisionID string
	if err := db.QueryRow(`
		INSERT INTO artifact_revision (tenant_id, artifact_id, revision_num, content, source, approved_by)
		VALUES ($1,$2,1,'FROZEN','approval','carol') RETURNING id::text`, tenantID, approvedID).Scan(&revisionID); err != nil {
		t.Fatalf("seed revision: %v", err)
	}

	// Apply 00016 (columns + backfill + CHECK + indexes).
	if err := goose.UpTo(db, "migrations", 16); err != nil {
		t.Fatalf("up to 16: %v", err)
	}

	var gotType, gotName, gotVis sql.NullString
	if err := db.QueryRow(
		`SELECT approved_type, approved_name, approved_visibility FROM artifact WHERE id=$1`, approvedID).
		Scan(&gotType, &gotName, &gotVis); err != nil {
		t.Fatalf("query backfilled identity: %v", err)
	}
	if gotType.String != "subagent" || gotName.String != "bf-live" || gotVis.String != "role" {
		t.Fatalf("backfilled identity = %q/%q/%q, want subagent/bf-live/role; an already-distributing "+
			"artifact must keep its path and its channel across the upgrade",
			gotType.String, gotName.String, gotVis.String)
	}

	if err := db.QueryRow(
		`SELECT approved_type, approved_name, approved_visibility FROM artifact WHERE id=$1`, draftID).
		Scan(&gotType, &gotName, &gotVis); err != nil {
		t.Fatalf("query draft identity: %v", err)
	}
	if gotType.Valid || gotName.Valid || gotVis.Valid {
		t.Fatalf("the draft artifact got an approved identity (%v/%v/%v); the backfill must be "+
			"scoped to rows with a snapshot", gotType, gotName, gotVis)
	}

	if err := db.QueryRow(
		`SELECT type, name, visibility FROM artifact_revision WHERE id=$1`, revisionID).
		Scan(&gotType, &gotName, &gotVis); err != nil {
		t.Fatalf("query revision identity: %v", err)
	}
	if gotType.Valid || gotName.Valid || gotVis.Valid {
		t.Fatalf("a pre-00016 revision was backfilled with %v/%v/%v; artifact_revision has never "+
			"recorded a historical name, so filling it in writes an unverified claim into an "+
			"append-only record", gotType, gotName, gotVis)
	}
}

// TestArtifactApprovedIdentityDownUpRoundTrip exercises 00016's Down block,
// which nothing else in this repo runs: goose.Down/DownTo appears only in
// TestRowVersionDownUpRoundTrip, and that one stops at 13. 00016's Down is not
// a column drop and nothing else. It also has to put 00010's definition of
// artifact_tenant_distributable_idx back, because 00016 replaced that index
// under the same name rather than adding one, and 00010's own Down then drops
// that name unconditionally.
//
// The cycle runs TWICE for the reason the 00013 test gives: an object the Down
// failed to remove surfaces as an "already exists" failure on the SECOND up,
// never the first.
func TestArtifactApprovedIdentityDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-identity-downup-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// identityCols counts the six columns 00016 adds (three on artifact, three
	// on artifact_revision); constraints counts the CHECK; distributableKey is
	// the column artifact_tenant_distributable_idx is keyed on.
	state := func(step string) (identityCols, constraints, uniqIdx int, distributableDef string) {
		if err := db.QueryRow(`
			SELECT count(*) FROM information_schema.columns
			WHERE (table_name = 'artifact' AND column_name IN ('approved_type','approved_name','approved_visibility'))
			   OR (table_name = 'artifact_revision' AND column_name IN ('type','name','visibility'))
		`).Scan(&identityCols); err != nil {
			t.Fatalf("%s: count columns: %v", step, err)
		}
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_constraint WHERE conname = 'artifact_approved_identity_complete'
		`).Scan(&constraints); err != nil {
			t.Fatalf("%s: count constraints: %v", step, err)
		}
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_indexes WHERE indexname = 'artifact_tenant_approved_identity_uniq'
		`).Scan(&uniqIdx); err != nil {
			t.Fatalf("%s: count unique index: %v", step, err)
		}
		if err := db.QueryRow(`
			SELECT indexdef FROM pg_indexes WHERE indexname = 'artifact_tenant_distributable_idx'
		`).Scan(&distributableDef); err != nil {
			t.Fatalf("%s: distributable indexdef: %v", step, err)
		}
		return identityCols, constraints, uniqIdx, distributableDef
	}

	assertState := func(step string, wantCols, wantConstraints, wantUniq int, wantKey string) {
		cols, constraints, uniq, def := state(step)
		if cols != wantCols || constraints != wantConstraints || uniq != wantUniq {
			t.Fatalf("%s: identity columns/constraints/unique indexes = %d/%d/%d, want %d/%d/%d",
				step, cols, constraints, uniq, wantCols, wantConstraints, wantUniq)
		}
		if !strings.Contains(def, wantKey) {
			t.Fatalf("%s: artifact_tenant_distributable_idx is %q, want it keyed on %q", step, def, wantKey)
		}
	}

	if err := goose.UpTo(db, "migrations", 15); err != nil {
		t.Fatalf("up to 15: %v", err)
	}
	assertState("baseline (v15)", 0, 0, 0, "(tenant_id, visibility)")

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 16); err != nil {
			t.Fatalf("cycle %d: up to 16: %v", cycle, err)
		}
		assertState(fmt.Sprintf("cycle %d: after up", cycle), 6, 1, 1, "(tenant_id, approved_visibility)")

		if err := goose.DownTo(db, "migrations", 15); err != nil {
			t.Fatalf("cycle %d: down to 15: %v", cycle, err)
		}
		assertState(fmt.Sprintf("cycle %d: after down", cycle), 0, 0, 0, "(tenant_id, visibility)")
	}

	// Leave the database at 16, matching every other test in this file.
	if err := goose.UpTo(db, "migrations", 16); err != nil {
		t.Fatalf("final up to 16: %v", err)
	}
	assertState("final (v16)", 6, 1, 1, "(tenant_id, approved_visibility)")
}

// TestArtifactDeploymentSchemaAndDownUpRoundTrip pins 00017's shape and
// exercises its Down.
//
// "The table exists" is the assertion this test refuses to stop at. A
// deployment registry whose primary key is not (user_id, install_id,
// artifact_id) accumulates a second row per report instead of replacing the
// first, and every install count it ever reports is wrong upward, with the
// table present and every insert succeeding. A users foreign key without ON
// DELETE CASCADE turns DELETE /v1/admin/users/{id} into a 23503, which is the
// 500 an admin meets on the route that is supposed to be the erasure path. So
// the key columns, all three cascade actions, the named CHECK and both index
// definitions are asserted individually.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already exists"
// failure on the SECOND up, never the first.
func TestArtifactDeploymentSchemaAndDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-deployment-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	tablePresent := func(step string) bool {
		var present bool
		if err := db.QueryRow(`SELECT to_regclass('artifact_deployment') IS NOT NULL`).Scan(&present); err != nil {
			t.Fatalf("%s: to_regclass: %v", step, err)
		}
		return present
	}

	// primaryKey returns the PK columns in key order, comma-joined. Order
	// matters to the reader but the correctness claim is the SET: a PK missing
	// install_id would collapse two machines into one row.
	primaryKey := func(step string) string {
		var cols string
		if err := db.QueryRow(`
			SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			FROM pg_constraint c
			JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
			JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
			WHERE c.conrelid = to_regclass('artifact_deployment') AND c.contype = 'p'
		`).Scan(&cols); err != nil {
			t.Fatalf("%s: primary key columns: %v", step, err)
		}
		return cols
	}

	// foreignKeys maps referenced table -> ON DELETE action ('c' = cascade,
	// 'a' = no action, the default a forgotten ON DELETE CASCADE leaves).
	foreignKeys := func(step string) map[string]string {
		rows, err := db.Query(`
			SELECT c.confrelid::regclass::text, c.confdeltype
			FROM pg_constraint c
			WHERE c.conrelid = to_regclass('artifact_deployment') AND c.contype = 'f'
		`)
		if err != nil {
			t.Fatalf("%s: foreign keys: %v", step, err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var table, action string
			if err := rows.Scan(&table, &action); err != nil {
				t.Fatalf("%s: scan foreign key: %v", step, err)
			}
			got[table] = action
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: foreign key rows: %v", step, err)
		}
		return got
	}

	indexDefs := func(step string) map[string]string {
		rows, err := db.Query(`SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'artifact_deployment'`)
		if err != nil {
			t.Fatalf("%s: index defs: %v", step, err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatalf("%s: scan index: %v", step, err)
			}
			got[name] = def
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: index rows: %v", step, err)
		}
		return got
	}

	constraintDef := func(step, name string) string {
		var def string
		err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name).Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("%s: constraintdef %q: %v", step, name, err)
		}
		return def
	}

	assertPresent := func(step string) {
		if !tablePresent(step) {
			t.Fatalf("%s: artifact_deployment does not exist", step)
		}
		if got, want := primaryKey(step), "user_id,install_id,artifact_id"; got != want {
			t.Fatalf("%s: primary key = %q, want %q -- a key missing any of the three lets one report "+
				"accumulate rows instead of replacing them, and every install count reads high", step, got, want)
		}
		wantFKs := map[string]string{"tenant": "c", "users": "c", "artifact": "c"}
		gotFKs := foreignKeys(step)
		if len(gotFKs) != len(wantFKs) {
			t.Fatalf("%s: foreign keys = %v, want exactly %v", step, gotFKs, wantFKs)
		}
		for table, want := range wantFKs {
			got, ok := gotFKs[table]
			if !ok {
				t.Fatalf("%s: no foreign key to %q; got %v", step, table, gotFKs)
			}
			if got != want {
				t.Fatalf("%s: foreign key to %q has ON DELETE %q, want %q ('c' = cascade). "+
					"Without it, deleting the parent fails with 23503 instead of removing the "+
					"deployment rows, and for users that is the erasure path returning a 500", step, table, got, want)
			}
		}
		if def := constraintDef(step, "artifact_deployment_revision_positive"); !strings.Contains(def, "revision >= 1") {
			t.Fatalf("%s: artifact_deployment_revision_positive is %q, want it to CHECK revision >= 1. "+
				"insertRevision numbers from 1, so a 0 arriving is a bug and the constraint is where it stops", step, def)
		}
		idx := indexDefs(step)
		for name, want := range map[string]string{
			"artifact_deployment_artifact_idx": "(tenant_id, artifact_id)",
			"artifact_deployment_reported_idx": "(reported_at)",
		} {
			def, ok := idx[name]
			if !ok {
				t.Fatalf("%s: index %q is missing; got %v", step, name, idx)
			}
			if !strings.Contains(def, want) {
				t.Fatalf("%s: index %q is defined as %q, want it keyed on %q", step, name, def, want)
			}
		}
	}

	if err := goose.UpTo(db, "migrations", 16); err != nil {
		t.Fatalf("up to 16: %v", err)
	}
	if tablePresent("baseline (v16)") {
		t.Fatal("baseline (v16): artifact_deployment exists before 00017 ran")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 17); err != nil {
			t.Fatalf("cycle %d: up to 17: %v", cycle, err)
		}
		assertPresent(fmt.Sprintf("cycle %d: after up", cycle))

		if err := goose.DownTo(db, "migrations", 16); err != nil {
			t.Fatalf("cycle %d: down to 16: %v", cycle, err)
		}
		if tablePresent(fmt.Sprintf("cycle %d: after down", cycle)) {
			t.Fatalf("cycle %d: artifact_deployment survived the Down", cycle)
		}
	}

	// Leave the database at 17, matching every other test in this file.
	if err := goose.UpTo(db, "migrations", 17); err != nil {
		t.Fatalf("final up to 17: %v", err)
	}
	assertPresent("final (v17)")
}

// TestArtifactMinRevisionSchemaAndDownUpRoundTrip pins 00018's shape and
// exercises its Down.
//
// "The column exists" is not the assertion. Three properties of it are
// load-bearing and each fails on its own mutant, with the column present
// either way:
//
//   - NOT NULL DEFAULT 0. A nullable column makes every pre-upgrade row read
//     as "unknown floor", and every reader of Artifact.MinRevisionNum would
//     need a null branch that nothing in the tree has.
//   - The default is 0, not 1. 0 is the off sentinel; a default of 1 would
//     silently give every artifact in an upgraded install a floor at the
//     oldest possible revision, which is a policy nobody set.
//   - CHECK (min_revision_num >= 0) under its own name. A floor below zero is
//     not a smaller floor, it is a value no clamp has a rule for.
//
// The negative case (an actual -1 write being refused) is asserted in
// internal/store, where a real artifact row exists to write it to and
// isConstraintViolation can match the constraint by NAME; here the definition
// itself is what survives, or does not survive, the Down.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already exists"
// failure on the SECOND up, never the first.
func TestArtifactMinRevisionSchemaAndDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-min-revision-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// column returns the column's is_nullable, data_type and column_default, or
	// ok=false when artifact carries no such column at all.
	column := func(step string) (nullable, dataType, def string, ok bool) {
		err := db.QueryRow(`
			SELECT is_nullable, data_type, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_name = 'artifact' AND column_name = 'min_revision_num'
		`).Scan(&nullable, &dataType, &def)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", false
		}
		if err != nil {
			t.Fatalf("%s: column lookup: %v", step, err)
		}
		return nullable, dataType, def, true
	}

	constraintDef := func(step, name string) string {
		var def string
		err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name).Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("%s: constraintdef %q: %v", step, name, err)
		}
		return def
	}

	assertPresent := func(step string) {
		nullable, dataType, def, ok := column(step)
		if !ok {
			t.Fatalf("%s: artifact.min_revision_num does not exist", step)
		}
		if nullable != "NO" {
			t.Fatalf("%s: min_revision_num is_nullable = %q, want %q. A nullable floor makes every "+
				"pre-upgrade row read as an unknown floor and forces a null branch no reader has",
				step, nullable, "NO")
		}
		if dataType != "integer" {
			t.Fatalf("%s: min_revision_num data_type = %q, want %q", step, dataType, "integer")
		}
		if def != "0" {
			t.Fatalf("%s: min_revision_num column_default = %q, want %q. 0 is the off sentinel; any "+
				"other default hands every artifact in an upgraded install a floor nobody set",
				step, def, "0")
		}
		if got := constraintDef(step, "artifact_min_revision_num_non_negative"); !strings.Contains(got, "min_revision_num >= 0") {
			t.Fatalf("%s: artifact_min_revision_num_non_negative is %q, want it to CHECK "+
				"min_revision_num >= 0. insertRevision numbers from 1 and 0 means no floor, so a "+
				"negative floor is a value the clamp has no rule for", step, got)
		}
	}

	if err := goose.UpTo(db, "migrations", 17); err != nil {
		t.Fatalf("up to 17: %v", err)
	}
	if _, _, _, ok := column("baseline (v17)"); ok {
		t.Fatal("baseline (v17): artifact.min_revision_num exists before 00018 ran")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 18); err != nil {
			t.Fatalf("cycle %d: up to 18: %v", cycle, err)
		}
		assertPresent(fmt.Sprintf("cycle %d: after up", cycle))

		if err := goose.DownTo(db, "migrations", 17); err != nil {
			t.Fatalf("cycle %d: down to 17: %v", cycle, err)
		}
		if _, _, _, ok := column(fmt.Sprintf("cycle %d: after down", cycle)); ok {
			t.Fatalf("cycle %d: artifact.min_revision_num survived the Down", cycle)
		}
		if got := constraintDef(fmt.Sprintf("cycle %d: after down", cycle), "artifact_min_revision_num_non_negative"); got != "" {
			t.Fatalf("cycle %d: artifact_min_revision_num_non_negative survived the Down as %q; the "+
				"second up would then fail on an already-existing constraint", cycle, got)
		}
	}

	// Leave the database at 18, matching every other test in this file.
	if err := goose.UpTo(db, "migrations", 18); err != nil {
		t.Fatalf("final up to 18: %v", err)
	}
	assertPresent("final (v18)")
}

// TestArtifactDeploymentPinnedSchemaAndDownUpRoundTrip pins 00019's shape and
// exercises its Down.
//
// Two properties are load-bearing, and each fails on its own mutant with the
// column present either way:
//
//   - NOT NULL DEFAULT false. A nullable pinned makes every row written before
//     this migration ran read as "unknown" rather than "not a pin", and every
//     reader would need a null branch nothing in the tree has.
//   - boolean, not an int or a nullable revision number. Spec sec 9.4 argues
//     this explicitly: carrying the requested revision here as well would let
//     an admin see which version a named developer wanted, which is a
//     per-person drill-down this table exists to refuse.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already exists"
// failure on the SECOND up, never the first.
func TestArtifactDeploymentPinnedSchemaAndDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-deployment-pinned-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// column returns pinned's is_nullable, data_type and column_default, or
	// ok=false when artifact_deployment carries no such column at all.
	column := func(step string) (nullable, dataType, def string, ok bool) {
		err := db.QueryRow(`
			SELECT is_nullable, data_type, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_name = 'artifact_deployment' AND column_name = 'pinned'
		`).Scan(&nullable, &dataType, &def)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", false
		}
		if err != nil {
			t.Fatalf("%s: column lookup: %v", step, err)
		}
		return nullable, dataType, def, true
	}

	assertPresent := func(step string) {
		nullable, dataType, def, ok := column(step)
		if !ok {
			t.Fatalf("%s: artifact_deployment.pinned does not exist", step)
		}
		if nullable != "NO" {
			t.Fatalf("%s: pinned is_nullable = %q, want %q. A nullable pinned makes every "+
				"pre-migration row read as unknown rather than not-a-pin", step, nullable, "NO")
		}
		if dataType != "boolean" {
			t.Fatalf("%s: pinned data_type = %q, want %q", step, dataType, "boolean")
		}
		if def != "false" {
			t.Fatalf("%s: pinned column_default = %q, want %q", step, def, "false")
		}
	}

	if err := goose.UpTo(db, "migrations", 18); err != nil {
		t.Fatalf("up to 18: %v", err)
	}
	if _, _, _, ok := column("baseline (v18)"); ok {
		t.Fatal("baseline (v18): artifact_deployment.pinned exists before 00019 ran")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 19); err != nil {
			t.Fatalf("cycle %d: up to 19: %v", cycle, err)
		}
		assertPresent(fmt.Sprintf("cycle %d: after up", cycle))

		if err := goose.DownTo(db, "migrations", 18); err != nil {
			t.Fatalf("cycle %d: down to 18: %v", cycle, err)
		}
		if _, _, _, ok := column(fmt.Sprintf("cycle %d: after down", cycle)); ok {
			t.Fatalf("cycle %d: artifact_deployment.pinned survived the Down", cycle)
		}
	}

	// Leave the database at 19, matching every other test in this file.
	if err := goose.UpTo(db, "migrations", 19); err != nil {
		t.Fatalf("final up to 19: %v", err)
	}
	assertPresent("final (v19)")
}

// TestArtifactFindingsAckSchemaAndDownUpRoundTrip pins 00028's shape and
// exercises its Down. Two properties are load-bearing:
//
//   - all four columns are NULLABLE with no default, which is what makes an
//     upgrade a no-op for every existing row: nothing is backfilled, and a
//     nullable column with no default reads NULL on every row that predates
//     the migration, exactly "no digest recorded, not acknowledged".
//   - artifact_findings_ack_complete CHECKs the three acknowledgment columns
//     together (mirroring 00016's artifact_approved_identity_complete) and
//     does NOT mention scan_findings_digest, which the test asserts by
//     substring rather than by "some CHECK exists": an acknowledgment can
//     legitimately exist for a digest that no longer matches the current
//     scan_findings_digest (a stale acknowledgment after a re-scan), so
//     tying the two together would reject a state this feature exists to
//     represent, not to forbid.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already exists"
// failure on the SECOND up, never the first.
func TestArtifactFindingsAckSchemaAndDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-findings-ack-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// column returns is_nullable/data_type for one of the four new columns,
	// or ok=false when artifact carries no such column at all.
	column := func(step, name string) (nullable, dataType string, ok bool) {
		err := db.QueryRow(`
			SELECT is_nullable, data_type
			FROM information_schema.columns
			WHERE table_name = 'artifact' AND column_name = $1
		`, name).Scan(&nullable, &dataType)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false
		}
		if err != nil {
			t.Fatalf("%s: column lookup %q: %v", step, name, err)
		}
		return nullable, dataType, true
	}

	constraintDef := func(step, name string) string {
		var def string
		err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name).Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("%s: constraintdef %q: %v", step, name, err)
		}
		return def
	}

	assertPresent := func(step string) {
		for name, wantType := range map[string]string{
			"scan_findings_digest": "text",
			"findings_ack_digest":  "text",
			"findings_ack_by":      "text",
			"findings_ack_at":      "timestamp with time zone",
		} {
			nullable, dataType, ok := column(step, name)
			if !ok {
				t.Fatalf("%s: artifact.%s does not exist", step, name)
			}
			if nullable != "YES" {
				t.Fatalf("%s: %s is_nullable = %q, want %q. A NOT NULL column here would fail on "+
					"upgrade for every existing row (no backfill is performed)", step, name, nullable, "YES")
			}
			if dataType != wantType {
				t.Fatalf("%s: %s data_type = %q, want %q", step, name, dataType, wantType)
			}
		}
		def := constraintDef(step, "artifact_findings_ack_complete")
		if !strings.Contains(def, "findings_ack_digest") || !strings.Contains(def, "findings_ack_by") || !strings.Contains(def, "findings_ack_at") {
			t.Fatalf("%s: artifact_findings_ack_complete is %q, want it to tie all three "+
				"acknowledgment columns together", step, def)
		}
		if strings.Contains(def, "scan_findings_digest") {
			t.Fatalf("%s: artifact_findings_ack_complete is %q, must NOT mention "+
				"scan_findings_digest: an acknowledgment for a since-changed digest is a valid "+
				"stale state, not a constraint violation", step, def)
		}
	}

	if err := goose.UpTo(db, "migrations", 27); err != nil {
		t.Fatalf("up to 27: %v", err)
	}
	if _, _, ok := column("baseline (v27)", "scan_findings_digest"); ok {
		t.Fatal("baseline (v27): artifact.scan_findings_digest exists before 00028 ran")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 28); err != nil {
			t.Fatalf("cycle %d: up to 28: %v", cycle, err)
		}
		assertPresent(fmt.Sprintf("cycle %d: after up", cycle))

		if err := goose.DownTo(db, "migrations", 27); err != nil {
			t.Fatalf("cycle %d: down to 27: %v", cycle, err)
		}
		for _, name := range []string{"scan_findings_digest", "findings_ack_digest", "findings_ack_by", "findings_ack_at"} {
			if _, _, ok := column(fmt.Sprintf("cycle %d: after down", cycle), name); ok {
				t.Fatalf("cycle %d: artifact.%s survived the Down", cycle, name)
			}
		}
		if got := constraintDef(fmt.Sprintf("cycle %d: after down", cycle), "artifact_findings_ack_complete"); got != "" {
			t.Fatalf("cycle %d: artifact_findings_ack_complete survived the Down as %q; the "+
				"second up would then fail on an already-existing constraint", cycle, got)
		}
	}

	// Leave the database at 28, matching every other test in this file.
	if err := goose.UpTo(db, "migrations", 28); err != nil {
		t.Fatalf("final up to 28: %v", err)
	}
	assertPresent("final (v28)")
}

// TestArtifactRevisionTenantFKDownUpRoundTrip pins 00032's shape and
// exercises its Down. Closes audit B37: artifact_revision was the only
// child of artifact without the composite (tenant_id, artifact_id) ->
// artifact(tenant_id, id) foreign key 00010 gave entitlement and
// artifact_entitlement as a schema-level backstop against a cross-tenant
// child row.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already
// exists" error on the SECOND Up, not the first -- a single cycle cannot
// distinguish a correct Down from one that silently no-ops.
func TestArtifactRevisionTenantFKDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-artifact-revision-tenant-fk-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	constraintDef := func(step string) string {
		var def string
		err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`,
			"artifact_revision_artifact_tenant_fk").Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("%s: constraintdef: %v", step, err)
		}
		return def
	}

	if err := goose.UpTo(db, "migrations", 31); err != nil {
		t.Fatalf("up to 31: %v", err)
	}
	if got := constraintDef("baseline (v31)"); got != "" {
		t.Fatalf("baseline (v31): artifact_revision_artifact_tenant_fk already exists as %q", got)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 32); err != nil {
			t.Fatalf("cycle %d: up to 32: %v", cycle, err)
		}
		def := constraintDef(fmt.Sprintf("cycle %d: after up", cycle))
		if !strings.Contains(def, "tenant_id") || !strings.Contains(def, "artifact_id") ||
			!strings.Contains(def, "REFERENCES artifact(tenant_id, id)") {
			t.Fatalf("cycle %d: artifact_revision_artifact_tenant_fk = %q, want a composite FK on "+
				"(tenant_id, artifact_id) referencing artifact(tenant_id, id)", cycle, def)
		}
		if !strings.Contains(def, "ON DELETE CASCADE") {
			t.Fatalf("cycle %d: artifact_revision_artifact_tenant_fk = %q, want ON DELETE CASCADE "+
				"(matching the existing single-column FK)", cycle, def)
		}

		if err := goose.DownTo(db, "migrations", 31); err != nil {
			t.Fatalf("cycle %d: down to 31: %v", cycle, err)
		}
		if got := constraintDef(fmt.Sprintf("cycle %d: after down", cycle)); got != "" {
			t.Fatalf("cycle %d: artifact_revision_artifact_tenant_fk survived the Down as %q; the "+
				"second up would then fail on an already-existing constraint", cycle, got)
		}
	}

	// Leave the database at the head migration.
	if err := goose.UpTo(db, "migrations", 33); err != nil {
		t.Fatalf("final up to 33: %v", err)
	}
}

// TestVirtualKeyLookupIndexDropDownUpRoundTrip pins 00033's shape and
// exercises its Down. Closes audit B37's measurable item: virtual_key_lookup
// (migration 00020) indexed the exact same (tenant_id, client_id) pair, in
// the same order, as virtual_key_client_id_uniq's own auto-generated unique
// index -- a genuine duplicate costing a second B-tree insert per write for
// zero read benefit.
func TestVirtualKeyLookupIndexDropDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-virtual-key-lookup-drop-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	indexExists := func(step string) bool {
		var exists bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'virtual_key_lookup')`).
			Scan(&exists)
		if err != nil {
			t.Fatalf("%s: pg_indexes lookup: %v", step, err)
		}
		return exists
	}
	uniqueIndexExists := func(step string) bool {
		var exists bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'virtual_key_client_id_uniq')`).
			Scan(&exists)
		if err != nil {
			t.Fatalf("%s: pg_indexes lookup: %v", step, err)
		}
		return exists
	}

	if err := goose.UpTo(db, "migrations", 32); err != nil {
		t.Fatalf("up to 32: %v", err)
	}
	if !indexExists("baseline (v32)") {
		t.Fatal("baseline (v32): virtual_key_lookup does not exist before 00033 runs")
	}
	if !uniqueIndexExists("baseline (v32)") {
		t.Fatal("baseline (v32): virtual_key_client_id_uniq's own index does not exist -- the " +
			"redundancy claim this migration is built on has no basis")
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 33); err != nil {
			t.Fatalf("cycle %d: up to 33: %v", cycle, err)
		}
		if indexExists(fmt.Sprintf("cycle %d: after up", cycle)) {
			t.Fatalf("cycle %d: virtual_key_lookup survived the Up (should have been dropped)", cycle)
		}
		if !uniqueIndexExists(fmt.Sprintf("cycle %d: after up", cycle)) {
			t.Fatalf("cycle %d: virtual_key_client_id_uniq's index is gone too -- the wrong index "+
				"was dropped", cycle)
		}

		if err := goose.DownTo(db, "migrations", 32); err != nil {
			t.Fatalf("cycle %d: down to 32: %v", cycle, err)
		}
		if !indexExists(fmt.Sprintf("cycle %d: after down", cycle)) {
			t.Fatalf("cycle %d: virtual_key_lookup did not come back on the Down; the second up "+
				"would then fail trying to create a duplicate index", cycle)
		}
	}

	// Leave the database at the head migration.
	if err := goose.UpTo(db, "migrations", 33); err != nil {
		t.Fatalf("final up to 33: %v", err)
	}
}

// TestArtifactDeploymentTenantFKDownUpRoundTrip pins 00034's shape and
// exercises its Down. Closes the second (and, per cascade_index_test.go's
// inboundForeignKeys["artifact"], last) child of artifact 00010's sweep
// missed: artifact_deployment (00017), like artifact_revision before 00032,
// carried a bare artifact_id -> artifact(id) foreign key with no composite
// (tenant_id, artifact_id) -> artifact(tenant_id, id) backstop.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already
// exists" error on the SECOND Up, not the first -- a single cycle cannot
// distinguish a correct Down from one that silently no-ops.
func TestArtifactDeploymentTenantFKDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-artifact-deployment-tenant-fk-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	constraintDef := func(step string) string {
		var def string
		err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`,
			"artifact_deployment_artifact_tenant_fk").Scan(&def)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("%s: constraintdef: %v", step, err)
		}
		return def
	}

	if err := goose.UpTo(db, "migrations", 33); err != nil {
		t.Fatalf("up to 33: %v", err)
	}
	if got := constraintDef("baseline (v33)"); got != "" {
		t.Fatalf("baseline (v33): artifact_deployment_artifact_tenant_fk already exists as %q", got)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 34); err != nil {
			t.Fatalf("cycle %d: up to 34: %v", cycle, err)
		}
		def := constraintDef(fmt.Sprintf("cycle %d: after up", cycle))
		if !strings.Contains(def, "tenant_id") || !strings.Contains(def, "artifact_id") ||
			!strings.Contains(def, "REFERENCES artifact(tenant_id, id)") {
			t.Fatalf("cycle %d: artifact_deployment_artifact_tenant_fk = %q, want a composite FK on "+
				"(tenant_id, artifact_id) referencing artifact(tenant_id, id)", cycle, def)
		}
		if !strings.Contains(def, "ON DELETE CASCADE") {
			t.Fatalf("cycle %d: artifact_deployment_artifact_tenant_fk = %q, want ON DELETE CASCADE "+
				"(matching the existing single-column FK)", cycle, def)
		}

		if err := goose.DownTo(db, "migrations", 33); err != nil {
			t.Fatalf("cycle %d: down to 33: %v", cycle, err)
		}
		if got := constraintDef(fmt.Sprintf("cycle %d: after down", cycle)); got != "" {
			t.Fatalf("cycle %d: artifact_deployment_artifact_tenant_fk survived the Down as %q; the "+
				"second up would then fail on an already-existing constraint", cycle, got)
		}
	}

	// Leave the database at the head migration.
	if err := goose.UpTo(db, "migrations", 34); err != nil {
		t.Fatalf("final up to 34: %v", err)
	}
}

// TestUsersLastSeenNullableBackfill pins 00035's own claim: the epoch
// sentinel a pre-00035 database could hold in last_seen_at (audit B9's
// documented stand-in for "provisioned, never authenticated", store.go's
// former neverAuthenticated) is converted to NULL, its schema-correct
// replacement, while a real timestamp on another row is left untouched.
//
// Both rows are seeded BEFORE 00035 runs, which is the only arrangement that
// can observe a backfill at all: a row written after the migration goes
// through UpsertProvisionedUser's NULL literal directly, so a test built on
// one alone could pass with the UPDATE statement deleted.
func TestUsersLastSeenNullableBackfill(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-lastseen-nullable-backfill-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Migrate up to just before 00035.
	if err := goose.UpTo(db, "migrations", 34); err != nil {
		t.Fatalf("up to 34: %v", err)
	}

	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('lastseen-nullable-bf') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// A pre-existing provisioned-but-never-authenticated row, exactly as
	// UpsertProvisionedUser wrote it under the old scheme.
	var neverAuthID string
	if err := db.QueryRow(`
		INSERT INTO users (tenant_id, subject, email, display_name, last_seen_at)
		VALUES ($1, 'never-authenticated', 'p@x.io', 'P', '1970-01-01 00:00:00+00')
		RETURNING id::text`, tenantID).Scan(&neverAuthID); err != nil {
		t.Fatalf("seed never-authenticated user: %v", err)
	}

	// A real, ordinary login timestamp, which the migration must leave alone.
	realTimestamp := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	var realID string
	if err := db.QueryRow(`
		INSERT INTO users (tenant_id, subject, email, display_name, last_seen_at)
		VALUES ($1, 'real-login', 'r@x.io', 'R', $2)
		RETURNING id::text`, tenantID, realTimestamp).Scan(&realID); err != nil {
		t.Fatalf("seed real-login user: %v", err)
	}

	// Apply 00035 (nullable + backfill).
	if err := goose.UpTo(db, "migrations", 35); err != nil {
		t.Fatalf("up to 35: %v", err)
	}

	var neverAuthLastSeen sql.NullTime
	if err := db.QueryRow(`SELECT last_seen_at FROM users WHERE id = $1`, neverAuthID).Scan(&neverAuthLastSeen); err != nil {
		t.Fatalf("query never-authenticated last_seen_at: %v", err)
	}
	if neverAuthLastSeen.Valid {
		t.Fatalf("never-authenticated row's last_seen_at = %s after 00035, want NULL: the epoch sentinel "+
			"must convert to its schema-correct replacement", neverAuthLastSeen.Time)
	}

	var realLastSeen sql.NullTime
	if err := db.QueryRow(`SELECT last_seen_at FROM users WHERE id = $1`, realID).Scan(&realLastSeen); err != nil {
		t.Fatalf("query real-login last_seen_at: %v", err)
	}
	if !realLastSeen.Valid {
		t.Fatal("real-login row's last_seen_at is NULL after 00035, want the untouched real timestamp: " +
			"the backfill predicate matched a row it must not have")
	}
	if !realLastSeen.Time.Equal(realTimestamp) {
		t.Fatalf("real-login row's last_seen_at = %s after 00035, want unchanged %s", realLastSeen.Time, realTimestamp)
	}
}

// TestUsersLastSeenNullableDownUpRoundTrip pins 00035's shape and exercises
// its Down: the column's nullability toggles correctly, and a NULL row
// converts back to the epoch sentinel so NOT NULL can be reapplied without
// error.
//
// The Down cycle runs TWICE for the reason TestRowVersionDownUpRoundTrip
// gives: an object the Down failed to remove surfaces as an "already exists"
// (here: a NOT NULL violation) error on the SECOND Up, not the first -- a
// single cycle cannot distinguish a correct Down from one that silently
// no-ops.
func TestUsersLastSeenNullableDownUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-migrate-lastseen-nullable-roundtrip-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	isNullable := func(step string) bool {
		var nullable string
		if err := db.QueryRow(`
			SELECT is_nullable FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'last_seen_at'`).Scan(&nullable); err != nil {
			t.Fatalf("%s: information_schema lookup: %v", step, err)
		}
		return nullable == "YES"
	}

	if err := goose.UpTo(db, "migrations", 34); err != nil {
		t.Fatalf("up to 34: %v", err)
	}
	if isNullable("baseline (v34)") {
		t.Fatal("baseline (v34): last_seen_at is already nullable before 00035 runs")
	}

	var tenantID string
	if err := db.QueryRow(`INSERT INTO tenant (name) VALUES ('lastseen-nullable-rt') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := goose.UpTo(db, "migrations", 35); err != nil {
			t.Fatalf("cycle %d: up to 35: %v", cycle, err)
		}
		if !isNullable(fmt.Sprintf("cycle %d: after up", cycle)) {
			t.Fatalf("cycle %d: last_seen_at is still NOT NULL after 00035", cycle)
		}

		// Insert a row with an explicit NULL last_seen_at -- only possible
		// once the column is nullable -- so the Down below has something to
		// convert back to the sentinel.
		var id string
		if err := db.QueryRow(`
			INSERT INTO users (tenant_id, subject, email, display_name, last_seen_at)
			VALUES ($1, $2, 'n@x.io', 'N', NULL)
			RETURNING id::text`, tenantID, fmt.Sprintf("null-row-cycle-%d", cycle)).Scan(&id); err != nil {
			t.Fatalf("cycle %d: seed NULL row: %v", cycle, err)
		}

		if err := goose.DownTo(db, "migrations", 34); err != nil {
			t.Fatalf("cycle %d: down to 34: %v", cycle, err)
		}
		if isNullable(fmt.Sprintf("cycle %d: after down", cycle)) {
			t.Fatalf("cycle %d: last_seen_at is still nullable after the Down; the second up would then "+
				"fail trying to re-add NOT NULL over a null row", cycle)
		}
		var restored time.Time
		if err := db.QueryRow(`SELECT last_seen_at FROM users WHERE id = $1`, id).Scan(&restored); err != nil {
			t.Fatalf("cycle %d: query restored last_seen_at: %v", cycle, err)
		}
		if !restored.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("cycle %d: last_seen_at after down = %s, want the epoch sentinel %s", cycle, restored, time.Unix(0, 0).UTC())
		}
	}

	// Leave the database at the head migration.
	if err := goose.UpTo(db, "migrations", 35); err != nil {
		t.Fatalf("final up to 35: %v", err)
	}
}
