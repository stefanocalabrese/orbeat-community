package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// This file gates the postgres 16 -> 18 upgrade end to end, against real
// containers of both majors.
//
// It exists because of a measured false green. A Dependabot PR moving the
// stack from 16 to 18 passed `go test ./...` AND the Playwright e2e suite, and
// was still broken. The reason is structural: the testcontainer suites and the
// dev compose stack mount nothing at postgres's data path, so 18 simply
// initdb-s a new cluster and every assertion about a freshly seeded database
// holds. Only the prod stack has a persistent volume at that path, so only
// `make smoke-prod` failed. A gate that asserts "18 starts" therefore proves
// nothing about upgrading TO 18. It is exactly the assertion that already
// passed on the broken change.
//
// So the two tests below assert the two things that are actually at stake:
// that data created on 16 can be carried into 18 (the operator procedure in
// docs/upgrade-guide.md), and that skipping that procedure is caught rather
// than silently swallowed.
//
// Readiness note, verified rather than assumed: postgres:18.6-alpine logs
// "database system is ready to accept connections" exactly twice on first
// boot, identically to 16.14-alpine. The house wait strategy (that line
// WithOccurrence(2), plus the port) is therefore still correct on 18, which
// matters, because if 18 had changed that count every testcontainer suite in
// this repo would have gone flaky in a way that looks random.

const (
	oldPGImage = "postgres:16-alpine"
	newPGImage = "postgres:18-alpine"

	// pgUser, pgPass and pgDB mirror the compose stack so the commands here
	// are the ones an operator would actually run.
	pgUser = "orbeat"
	pgPass = "orbeat"
	pgDB   = "orbeat"

	// oldPGDATAMount is where postgres <= 17 keeps its cluster, and what the
	// prod compose file mounted before this upgrade.
	oldPGDATAMount = "/var/lib/postgresql/data"
)

// pgReady is the repo-wide Postgres wait strategy: Postgres opens the port,
// then restarts during init, so waiting on the port alone connects mid-restart.
func pgReady() testcontainers.CustomizeRequestOption {
	return testcontainers.WithWaitStrategy(
		wait.ForAll(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(90*time.Second),
		),
	)
}

// psql runs a query with -tA (tuples only, unaligned) and returns the trimmed
// output, failing the test on a non-zero exit so a broken query can never be
// mistaken for an empty result.
func psql(t *testing.T, ctx context.Context, c testcontainers.Container, db, query string) string {
	t.Helper()
	code, r, err := c.Exec(ctx,
		[]string{"psql", "-U", pgUser, "-d", db, "-tAc", query},
		tcexec.Multiplexed(),
	)
	if err != nil {
		t.Fatalf("exec psql %q: %v", query, err)
	}
	out, _ := io.ReadAll(r)
	if code != 0 {
		t.Fatalf("psql %q exited %d: %s", query, code, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPostgres16To18DumpRestoreRoundTrip is the real gate on this upgrade:
// create orbeat-shaped data on 16, dump it, bring 18 up on a fresh data
// directory, restore, and assert the data actually arrived.
//
// The assertion is deliberately not "the table exists" or "count > 0". It
// captures a fingerprint of the row FROM THE 16 SERVER, including a
// server-generated random uuid and a server-generated timestamp, and demands
// the identical string back from 18. A fresh initdb cannot produce that uuid,
// and neither can a hardcoded expectation in this file, so the test can only
// pass if the bytes genuinely crossed the major boundary.
//
// The dump is taken with the OLD major's own pg_dump, inside the old
// container. That is what the prod stack's backup sidecar does and what
// docs/upgrade-guide.md tells the operator to do, and it is always a valid
// combination because client and server are the same version. (The reverse,
// an old pg_dump against a new server, is refused outright, see
// TestPostgresMajorIsPinnedEverywhere.)
func TestPostgres16To18DumpRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()

	// --- 1. The old major, with data on it. ---
	old, err := tcpostgres.Run(ctx, oldPGImage,
		tcpostgres.WithDatabase(pgDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPass),
		pgReady(),
	)
	if err != nil {
		t.Fatalf("start %s: %v", oldPGImage, err)
	}
	t.Cleanup(func() { _ = old.Terminate(ctx) })

	// Column types chosen to match this repo's SQL conventions (CLAUDE.md):
	// uuid PK via gen_random_uuid(), timestamptz, text[], jsonb.
	psql(t, ctx, old, pgDB, `
		CREATE TABLE tenant (
			id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			name       text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			tags       text[] NOT NULL,
			meta       jsonb NOT NULL
		);
		INSERT INTO tenant (name, tags, meta)
		VALUES ('acme', ARRAY['alpha','beta'], '{"seats": 10}'::jsonb);`)

	const fingerprint = `SELECT id::text || '|' || name || '|' ||
		array_to_string(tags, ',') || '|' || (meta->>'seats') || '|' ||
		to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
		FROM tenant ORDER BY name`

	want := psql(t, ctx, old, pgDB, fingerprint)

	// Guard the guard: an empty or malformed capture here would make the
	// comparison below vacuously true ("" == ""), which is the exact shape of
	// a test that cannot fail. Demand a well-formed 5-field fingerprint.
	if got := strings.Count(want, "|"); got != 4 || len(want) < 40 {
		t.Fatalf("fingerprint captured from %s is not usable as an assertion: %q (%d separators, want 4)", oldPGImage, want, got)
	}
	t.Logf("captured on %s: %s", oldPGImage, want)

	// --- 2. Dump, with the old major's own pg_dump. ---
	const dumpPath = "/tmp/orbeat-upgrade.dump"
	code, r, err := old.Exec(ctx,
		[]string{"pg_dump", "-U", pgUser, "-d", pgDB, "-Fc", "-f", dumpPath},
		tcexec.Multiplexed(),
	)
	if err != nil {
		t.Fatalf("exec pg_dump: %v", err)
	}
	if out, _ := io.ReadAll(r); code != 0 {
		t.Fatalf("pg_dump exited %d: %s", code, out)
	}

	rc, err := old.CopyFileFromContainer(ctx, dumpPath)
	if err != nil {
		t.Fatalf("copy dump out of %s: %v", oldPGImage, err)
	}
	dump, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if len(dump) == 0 {
		t.Fatal("pg_dump produced an empty file")
	}
	hostDump := filepath.Join(t.TempDir(), "orbeat-upgrade.dump")
	if err := os.WriteFile(hostDump, dump, 0o600); err != nil {
		t.Fatalf("write dump to host: %v", err)
	}

	// The old major is done. Stopping it before the restore mirrors the
	// operator procedure (the 16 stack is down while 18 comes up) and proves
	// the restore cannot be reading through to the old server.
	if err := old.Terminate(ctx); err != nil {
		t.Fatalf("terminate %s: %v", oldPGImage, err)
	}

	// --- 3. The new major, on a FRESH data directory. ---
	fresh, err := tcpostgres.Run(ctx, newPGImage,
		tcpostgres.WithDatabase(pgDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPass),
		pgReady(),
	)
	if err != nil {
		t.Fatalf("start %s: %v", newPGImage, err)
	}
	t.Cleanup(func() { _ = fresh.Terminate(ctx) })

	// It really is empty before the restore. Without this the test could not
	// distinguish "the restore worked" from "the data was somehow already
	// there", and the whole point of the exercise is that 18 does not inherit
	// 16's cluster.
	if n := psql(t, ctx, fresh, pgDB, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`); n != "0" {
		t.Fatalf("%s came up with %s public tables before the restore, want 0", newPGImage, n)
	}

	// --- 4. Restore. ---
	if err := fresh.CopyFileToContainer(ctx, hostDump, dumpPath, 0o644); err != nil {
		t.Fatalf("copy dump into %s: %v", newPGImage, err)
	}
	code, r, err = fresh.Exec(ctx,
		[]string{"pg_restore", "-U", pgUser, "-d", pgDB, dumpPath},
		tcexec.Multiplexed(),
	)
	if err != nil {
		t.Fatalf("exec pg_restore: %v", err)
	}
	if out, _ := io.ReadAll(r); code != 0 {
		t.Fatalf("pg_restore exited %d: %s", code, out)
	}

	// --- 5. The data crossed the major boundary intact. ---
	got := psql(t, ctx, fresh, pgDB, fingerprint)
	if got != want {
		t.Errorf("row did not survive the %s -> %s dump/restore:\n  on %s: %q\n  on %s: %q",
			oldPGImage, newPGImage, oldPGImage, want, newPGImage, got)
	}
}

// TestPostgres18RefusesToStartOnAPre18DataDirectory pins the failure mode the
// upgrade guide promises, because the guide's value depends on it.
//
// postgres 18 keeps its cluster in a major-version subdirectory
// (/var/lib/postgresql/18/docker) rather than the flat /var/lib/postgresql/data
// used through 17. Pointed at a directory holding an older cluster, the image
// refuses to boot and explains why, rather than initdb-ing a fresh empty one
// beside it. That distinction is the entire risk profile of this upgrade: a
// stack that will not start is an outage you notice in seconds, while a stack
// that starts empty is a silent restore-from-backup you notice later.
//
// This asserts BOTH halves. A non-zero exit alone would pass on any unrelated
// crash, so the log must also name the reason.
func TestPostgres18RefusesToStartOnAPre18DataDirectory(t *testing.T) {
	ctx := context.Background()

	vol := fmt.Sprintf("orbeat-pgupgrade-gate-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		// Docker creates the named volume implicitly on first mount; nothing
		// labels it as a testcontainers resource, so the reaper will not take
		// it. Best-effort removal, and never a test failure: a leaked volume
		// is noise, not a wrong answer.
		if out, err := exec.Command("docker", "volume", "rm", "-f", vol).CombinedOutput(); err != nil {
			t.Logf("could not remove test volume %s: %v: %s", vol, err, out)
		}
	})

	// --- 1. A real 16 cluster on a persistent volume. ---
	old, err := tcpostgres.Run(ctx, oldPGImage,
		tcpostgres.WithDatabase(pgDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPass),
		testcontainers.WithMounts(testcontainers.VolumeMount(vol, oldPGDATAMount)),
		pgReady(),
	)
	if err != nil {
		t.Fatalf("start %s: %v", oldPGImage, err)
	}
	psql(t, ctx, old, pgDB, `CREATE TABLE tenant (id uuid PRIMARY KEY DEFAULT gen_random_uuid());
		INSERT INTO tenant DEFAULT VALUES;`)
	if v := psql(t, ctx, old, pgDB, `SELECT count(*) FROM tenant`); v != "1" {
		t.Fatalf("setup: tenant count on %s = %q, want 1", oldPGImage, v)
	}
	if err := old.Terminate(ctx); err != nil {
		t.Fatalf("terminate %s: %v", oldPGImage, err)
	}

	// --- 2. The new major, pointed at that same directory. ---
	// wait.ForExit is the assertion mechanism, not a convenience: if the image
	// were to start happily here it would never exit, the wait would time out,
	// and Run returns an error we report as the silent-data-loss finding.
	newC, err := testcontainers.Run(ctx, newPGImage,
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER":     pgUser,
			"POSTGRES_PASSWORD": pgPass,
			"POSTGRES_DB":       pgDB,
		}),
		testcontainers.WithMounts(testcontainers.VolumeMount(vol, oldPGDATAMount)),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(90*time.Second)),
	)
	if newC != nil {
		t.Cleanup(func() { _ = newC.Terminate(ctx) })
	}
	if err != nil {
		t.Fatalf("%s did not exit when pointed at a %s data directory at %s (%v).\n"+
			"That means it started anyway, which is the silent failure mode: the old cluster is ignored, "+
			"a fresh empty one is created elsewhere, and the stack comes up healthy with no data. "+
			"docs/upgrade-guide.md tells operators this case fails loudly. Fix the guide, not this test.",
			newPGImage, oldPGImage, oldPGDATAMount, err)
	}

	state, err := newC.State(ctx)
	if err != nil {
		t.Fatalf("inspect %s: %v", newPGImage, err)
	}
	if state.ExitCode == 0 {
		t.Errorf("%s exited 0 on a %s data directory, want a non-zero exit", newPGImage, oldPGImage)
	}

	rc, err := newC.Logs(ctx)
	if err != nil {
		t.Fatalf("read %s logs: %v", newPGImage, err)
	}
	logs, _ := io.ReadAll(rc)
	_ = rc.Close()

	// Both markers come from the image's own refusal message. Checking the
	// exit code alone would pass on any unrelated startup crash, which would
	// leave the upgrade guide's central claim unpinned.
	for _, marker := range []string{
		"major-version-specific directory names",
		oldPGDATAMount,
	} {
		if !strings.Contains(string(logs), marker) {
			t.Errorf("%s exited %d but its logs never mention %q, so it did not stop for the reason the upgrade guide documents.\nLogs:\n%s",
				newPGImage, state.ExitCode, marker, logs)
		}
	}
}
