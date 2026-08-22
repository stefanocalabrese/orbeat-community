package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	// admin-list order). Critically: 00003's `artifact_tenant_active_idx` must
	// be ABSENT (00006's DROP COLUMN status took it), superseded by
	// artifact_tenant_distributable_idx.
	wantArtifact := []string{
		"artifact_pkey",
		"artifact_tenant_distributable_idx",
		"artifact_tenant_id_type_name_key",
		"artifact_tenant_id_uniq",
		"artifact_tenant_state_type_name_id_idx",
		"artifact_tenant_type_name_id_idx",
	}
	if got := indexNames("artifact"); !equal(got, wantArtifact) {
		t.Fatalf("artifact indexes = %v, want %v", got, wantArtifact)
	}

	// audit_event: PK + the keyset index WITH the id tiebreak, + the ts index
	// for the retention prune's `ts < cutoff` filter (00011). The old
	// (tenant_id, ts DESC)-only `audit_event_tenant_ts_idx` must be gone.
	wantAudit := []string{
		"audit_event_pkey",
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
// otherwise has ZERO coverage: goose.Down/DownTo appears nowhere else in this
// repo, so `go test ./internal/migrate/... -count=1` passing does not mean a
// single line of any migration's down block has ever executed. That matters
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
