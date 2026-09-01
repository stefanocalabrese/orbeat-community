package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// THIS FILE IS AN ORDINARY _test.go AND EVERY ASSERTION IN IT IS TRUE IN BOTH
// EDITIONS, which is a constraint rather than an accident. internal/communitygen
// copies ordinary test files verbatim into the generated Community tree, where
// TestGenerateProducesTestableTree runs them on every `go test ./...`, so an
// assertion that only holds in this repo's Enterprise build would turn the free
// edition's own suite red. Everything that asserts pinning WORKS lives in
// sync_pins.ee_test.go, which the generator drops.
//
// The split is not "pure functions here, wired behaviour there" either.
// pinResolve is shared, untagged code, present and correct in both trees, so
// its table test belongs here; what belongs in the .ee file is anything whose
// answer depends on New, because New is where the edition enters.

// approveRevision sets the working copy's content to body and approves it,
// appending exactly ONE revision carrying exactly those bytes, pruning to keep
// (0 = no pruning).
//
// The content edit is the whole point: approveArtifact (sync_test.go) approves
// the SAME body every time, so a chain built with it cannot tell revision 3's
// bytes from revision 5's and a clamp serving the wrong one would pass every
// content assertion.
func approveRevision(t *testing.T, s *store.Store, tenantID, id, body string, keep int) {
	t.Helper()
	ctx := context.Background()
	cur, err := s.GetArtifact(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("read artifact before revision: %v", err)
	}
	cur.Content = body
	if _, err := s.UpdateArtifact(ctx, cur, cur.RowVersion); err != nil {
		t.Fatalf("write revision body: %v", err)
	}
	if err := s.InTx(ctx, func(tx *store.Store) error {
		if _, e := tx.GetArtifactForUpdate(ctx, tenantID, id); e != nil {
			return e
		}
		_, _, e := tx.SetArtifactApproved(ctx, tenantID, id, "approver", keep)
		return e
	}); err != nil {
		t.Fatalf("approve revision: %v", err)
	}
}

// revisionBody is the body of revision n of the named artifact. The marker is
// distinct per revision and long enough that a containment check on it cannot
// match a neighbouring revision's.
func revisionBody(name string, n int) string {
	return fmt.Sprintf("---\nname: %s\ndescription: d\n---\nPIN-BODY-V%d-ONLY", name, n)
}

// pinFixture creates a fresh tenant, role and role-visibility artifact carrying
// revisions 1..revisions, each with its own body, pruned to keep. It returns
// the artifact's name too, because every content assertion is against
// revisionBody(name, n) and recomputing the name at the call site is one more
// place for it to drift.
func pinFixture(t *testing.T, prefix, artifactType string, revisions, keep int) (*store.Store, store.Tenant, store.Role, store.Artifact, string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, err := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	role, err := s.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	name := prefix + "-art"
	art, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: artifactType, Name: name, Description: "d",
		Content: revisionBody(name, 0), Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := s.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}
	for n := 1; n <= revisions; n++ {
		approveRevision(t, s, tn.ID, art.ID, revisionBody(name, n), keep)
	}
	return s, tn, role, art, name
}

// setFloor writes min_revision_num on its own connection, which keeps every
// clamp fixture here independent of whatever route writes the floor: the clamp
// reads the column, and a fixture driven through an admin handler would fail
// for that handler's reasons as well as its own. That route now exists,
// PUT /v1/admin/artifacts/{id}/min-revision, and this helper still does not use
// it, deliberately: the one test that has to prove the route and the clamp meet
// drives both for real (TestFloorSetThroughTheRouteReachesTheClamp), and every
// other fixture here stays independent of it.
func setFloor(t *testing.T, artifactID string, floor int) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("open connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE artifact SET min_revision_num=$1 WHERE id=$2`, floor, artifactID); err != nil {
		t.Fatalf("set min_revision_num=%d: %v", floor, err)
	}
}

// syncArtifacts drives handleSyncArtifacts with an injected identity and
// returns the status plus the decoded artifacts keyed by name. A non-200
// returns a nil map, so a caller asserting a 400 asserts the status and
// nothing else, which is deliberate: an implementation that accepted a bad pin,
// found no such revision and fell back to approved_content serves output
// byte-identical to a correct one, so a content assertion would pass on it.
func syncArtifacts(t *testing.T, srv *Server, tn store.Tenant, role store.Role, query string) (int, map[string]map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts"+query, nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode sync artifacts: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, a := range body.Artifacts {
		byName[a["name"].(string)] = a
	}
	return rec.Code, byName
}

// jsonInt reads a JSON number that decoded into any as float64, failing if the
// key is absent rather than reading a missing key as 0 (omitempty makes those
// two states genuinely different on this DTO).
func jsonInt(t *testing.T, got map[string]any, key string) int {
	t.Helper()
	raw, ok := got[key]
	if !ok {
		t.Fatalf("the response carries no %q key at all: %+v", key, got)
	}
	n, isNum := raw.(float64)
	if !isNum {
		t.Fatalf("%s = %v (%T), want a number", key, raw, raw)
	}
	return int(n)
}

// TestPinResolveClampsEachArmIndependently is spec §4.2's whole rule, one
// subtest per arm.
//
// EVERY CASE MAKES ALL FOUR INPUTS DISTINCT wherever the arm allows it. A
// fixture where two of pin, floor, minNum and maxNum are equal cannot tell them
// apart, so a clamp reading the floor where it means the prune boundary would
// pass it. The cases that deliberately DO tie a pair are the two boundary ones,
// where the tie IS the property under test, and the equal-bounds one, where the
// tie is what the tie-break rule is about.
//
// One arm per subtest, so a mutant that breaks one bound fails one case rather
// than the whole table: a suite where every case goes red for every mutant says
// nothing about where the defect is. Each `why` names the wrong answer that
// case exists to exclude, which is also the record of what it was red-proven
// against.
func TestPinResolveClampsEachArmIndependently(t *testing.T) {
	for _, c := range []struct {
		name                     string
		pin, floor, minNum, maxN int
		wantServed, wantOldest   int
		wantReason               string
		why                      string
	}{
		{
			name: "honoured between both bounds", pin: 3, floor: 2, minNum: 1, maxN: 5,
			wantServed: 3, wantOldest: 2, wantReason: "",
			why: "the pin sits inside the window, so it is served exactly; serving the floor (2) or the latest (5) both ignore it",
		},
		{
			name: "floor raises the pin", pin: 2, floor: 4, minNum: 1, maxN: 7,
			wantServed: 4, wantOldest: 4, wantReason: pinOverrideFloor,
			why: "the floor is the binding bound; low=min(floor,minNum) would serve the pin unchanged at 2 and low=maxNum would serve 7",
		},
		{
			name: "prune boundary raises the pin", pin: 2, floor: 0, minNum: 4, maxN: 9,
			wantServed: 4, wantOldest: 4, wantReason: pinOverridePruned,
			why: "a pruned pin serves the OLDEST SURVIVOR (4), never the latest (9); degrading to latest is the same as ignoring the pin",
		},
		{
			name: "both bite and the prune boundary binds", pin: 2, floor: 5, minNum: 8, maxN: 11,
			wantServed: 8, wantOldest: 8, wantReason: pinOverridePruned,
			why: "spec §4.3's own example: lowering the floor to 5 would still not produce revision 2, so naming the floor sends an admin to change a control that is not what is stopping anybody",
		},
		{
			name: "both bite and the floor binds", pin: 2, floor: 6, minNum: 3, maxN: 9,
			wantServed: 6, wantOldest: 6, wantReason: pinOverrideFloor,
			why: "revision 3 still exists, so the floor is what withholds it and lowering the floor is the action that changes this outcome",
		},
		{
			name: "equal bounds report pruned", pin: 1, floor: 4, minNum: 4, maxN: 7,
			wantServed: 4, wantOldest: 4, wantReason: pinOverridePruned,
			why: "a floor AT the prune boundary withholds nothing that survives, so pruned is the actionable half of the tie",
		},
		{
			name: "pin above the newest revision", pin: 9, floor: 2, minNum: 3, maxN: 6,
			wantServed: 6, wantOldest: 3, wantReason: pinOverrideAhead,
			why: "the upper clamp is real: served=max(pin,low) with no min() would hand back 9, a revision that does not exist",
		},
		{
			name: "pin exactly at the oldest servable revision", pin: 4, floor: 4, minNum: 2, maxN: 8,
			wantServed: 4, wantOldest: 4, wantReason: "",
			why: "a pin AT the floor is honoured, not overridden; measured, this is the only case that fails when the honoured arm is narrowed to `served == pin && pin > low`",
		},
		{
			name: "pin exactly at the newest revision", pin: 6, floor: 2, minNum: 1, maxN: 6,
			wantServed: 6, wantOldest: 2, wantReason: "",
			why: "the ceiling is maxNum and not maxNum-1; an off-by-one there serves revision 5 while every case whose pin sits strictly inside the window stays green",
		},
		{
			name: "no pin serves the latest even under a floor", pin: 0, floor: 3, minNum: 2, maxN: 8,
			wantServed: 8, wantOldest: 3, wantReason: "",
			why: "an unpinned caller has always received the newest approved revision and a floor must not change that; serving low (3) would silently downgrade every unpinned machine",
		},
		{
			name: "floor above every surviving revision", pin: 2, floor: 9, minNum: 3, maxN: 6,
			wantServed: 6, wantOldest: 9, wantReason: pinOverrideFloor,
			why: "reachable only by direct SQL or a restore to an earlier point; the upper clamp still wins, so a caller gets the newest rather than nothing",
		},
		{
			name: "artifact with no revision rows", pin: 3, floor: 0, minNum: 0, maxN: 0,
			wantServed: 0, wantOldest: 0, wantReason: pinOverrideAhead,
			why: "served==maxNum==0 makes the caller serve approved_content and read no revision at all, which is §5.1's degradation to current behaviour rather than to an error",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := pinResolve(c.pin, c.floor, c.minNum, c.maxN)
			if got.Served != c.wantServed {
				t.Errorf("pinResolve(pin=%d, floor=%d, minNum=%d, maxNum=%d).Served = %d, want %d: %s",
					c.pin, c.floor, c.minNum, c.maxN, got.Served, c.wantServed, c.why)
			}
			if got.Oldest != c.wantOldest {
				t.Errorf("pinResolve(pin=%d, floor=%d, minNum=%d, maxNum=%d).Oldest = %d, want max(floor,minNum)=%d",
					c.pin, c.floor, c.minNum, c.maxN, got.Oldest, c.wantOldest)
			}
			if got.Reason != c.wantReason {
				t.Errorf("pinResolve(pin=%d, floor=%d, minNum=%d, maxNum=%d).Reason = %q, want %q: %s",
					c.pin, c.floor, c.minNum, c.maxN, got.Reason, c.wantReason, c.why)
			}
		})
	}
}

// TestSyncConfigReportsWhatTheHandlerActsOn drives handleSyncConfig with
// s.pinning forced BOTH ways and asserts the advertised flag follows it.
//
// The flag is a promise a client acts on before it can check: a server
// advertising pinning: true that then ignores ?pin= serves the LATEST revision
// with no error, which on an unchanged artifact is byte-identical to a pin that
// was honoured. So the mutant this exists for is a hardcoded literal in
// handleSyncConfig, and a single-valued assertion cannot see one.
//
// Both directions are asserted in both editions because handleSyncConfig is
// SHARED: it is the same function in a generated Community tree, and what
// differs there is only the value New puts in the field.
func TestSyncConfigReportsWhatTheHandlerActsOn(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(fmt.Sprintf("pinning=%v", want), func(t *testing.T) {
			srv := New(nil, nil, nil, nil, nil)
			srv.pinning = want

			req := httptest.NewRequest(http.MethodGet, "/v1/sync/config", nil)
			req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()),
				authz.ResolvedContext{TenantID: "t1", UserID: "u1"}))
			rec := httptest.NewRecorder()
			srv.handleSyncConfig(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/sync/config = %d, want 200", rec.Code)
			}
			var cfg map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
				t.Fatalf("decode sync config: %v", err)
			}
			raw, present := cfg["pinning"]
			if !present {
				t.Fatalf("the config body carries no pinning key at all, so a client cannot negotiate: %+v", cfg)
			}
			if raw != any(want) {
				t.Errorf("pinning = %v, want %v: the advertised flag must be the field handleSyncArtifacts acts on, not a literal", raw, want)
			}
		})
	}
}

// TestSyncWithoutPinningIgnoresPinsEntirely is the Community contract, asserted
// here rather than in the .ee file because it is what a generated Community
// tree actually does: New sets pinning from pinningSupported(), false there, so
// this is that build's real behaviour and not a hypothetical.
//
// Three properties, each with its own mutant:
//
//   - A pin is IGNORED, not obeyed: the served bytes are the latest revision's.
//     A pin path that ran regardless of the flag would hand back revision 1.
//   - A malformed pin is IGNORED, not a 400. A Community server that rejected a
//     new client's ?pin= would turn a warn-and-continue into a broken sync for
//     a developer who did nothing wrong and cannot change the server.
//   - The three pinning fields are ABSENT, not zero. A client must not be told
//     a window it cannot pin into.
//
// The content assertion is what makes the field-absence one non-vacuous: an
// implementation that clamped correctly and merely stopped emitting the three
// fields would pass an absence-only check while quietly serving a Community
// developer an old revision.
func TestSyncWithoutPinningIgnoresPinsEntirely(t *testing.T) {
	s, tn, role, art, name := pinFixture(t, "pinoff", "skill", 3, 0)

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	srv.pinning = false // exactly what New itself produces in a generated Community tree

	code, got := syncArtifacts(t, srv, tn, role, "?pin="+art.ID+":1")
	if code != http.StatusOK {
		t.Fatalf("a server without pinning must not reject ?pin=, got %d", code)
	}
	a := got[name]
	if a == nil {
		t.Fatalf("the entitled artifact is missing from the response: %+v", got)
	}
	if content, _ := a["content"].(string); content != revisionBody(name, 3) {
		t.Errorf("content = %q, want revision 3's body: a pin must change nothing on a server that does not support pinning", content)
	}
	if rev := jsonInt(t, a, "revision"); rev != 3 {
		t.Errorf("revision = %d, want 3", rev)
	}
	for _, k := range []string{"oldestServable", "latest", "pinOverride"} {
		if v, present := a[k]; present {
			t.Errorf("a server without pinning must omit %q entirely, got %v", k, v)
		}
	}

	// Malformed input on the same server is still a 200, because the parser is
	// never reached at all.
	for _, q := range []string{"?pin=notauuid:1", "?pin=" + art.ID + ":0", "?pin=" + art.ID} {
		t.Run("malformed is not rejected: "+q, func(t *testing.T) {
			if code, _ := syncArtifacts(t, srv, tn, role, q); code != http.StatusOK {
				t.Errorf("GET /v1/sync/artifacts%s = %d, want 200: a Community server must ignore ?pin, never reject it", q, code)
			}
		})
	}
}

// findSyncListEvent returns the one sync.list event among evs (newest
// first), failing loudly rather than indexing evs[0] blindly: a future
// fixture change that makes handleSyncArtifacts fire twice, or that adds
// another audited call before it, must not silently read the wrong event.
// Pure lookup, no edition-dependent assertion, so it lives here rather than
// in sync_pins.ee_test.go or sync_audit.ee_test.go: it is what lets
// TestSyncListAuditOmitsPinningKeysWithoutPinning below run inside a
// generated Community tree.
func findSyncListEvent(t *testing.T, evs []store.AuditEvent) store.AuditEvent {
	t.Helper()
	for _, e := range evs {
		if e.Action == "sync.list" {
			return e
		}
	}
	t.Fatalf("no sync.list audit event found among %d events", len(evs))
	return store.AuditEvent{}
}

// TestSyncListAuditOmitsPinningKeysWithoutPinning is the Community contract
// for the sync.list audit event, mirroring TestSyncWithoutPinningIgnoresPins
// Entirely above for the DTO: a server with s.pinning false must not write
// pinned/overridden/overriddenArtifacts/truncated at all, not even as
// zeros, because those keys describe a feature this server does not have.
// Asserted here rather than left to the handler's own comment, because a
// generated Community tree runs exactly this code path (New sets pinning
// from pinningSupported(), false there) and a key silently emitted as 0
// would be a promise the handler does not keep, invisible to a check that
// only reads values and never checks for presence.
func TestSyncListAuditOmitsPinningKeysWithoutPinning(t *testing.T) {
	s, tn, role, art, name := pinFixture(t, "pinauditoff", "skill", 2, 0)

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	srv.pinning = false // exactly what New itself produces in a generated Community tree
	code, got := syncArtifacts(t, srv, tn, role, "?pin="+art.ID+":1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got[name] == nil {
		t.Fatalf("entitled artifact missing from response: %+v", got)
	}

	evs, err := s.ListAuditEventsByTenant(context.Background(), tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	ev := findSyncListEvent(t, evs)
	for _, k := range []string{"pinned", "overridden", "overriddenArtifacts", "truncated"} {
		if v, present := ev.Metadata[k]; present {
			t.Errorf("a server without pinning must omit metadata[%q] entirely, got %v", k, v)
		}
	}
	if _, present := ev.Metadata["count"]; !present {
		t.Error("metadata[count] is missing: turning pinning off must not take the base field with it")
	}
}

// newManualPinMetrics builds a *telemetry.Metrics backed by a manual reader
// (mirrors internal/gateway/session_metrics_test.go's newManualSessionMetrics
// and internal/telemetry/metrics_test.go's registration tests) plus a
// collector for exactly the counter the tests below care about,
// orbeat.sync.pin.payload_race_fallback.
func newManualPinMetrics(t *testing.T) (metrics *telemetry.Metrics, collect func() int64) {
	t.Helper()
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := telemetry.NewMetrics(mp.Meter("sync-pin-race-test"))
	collect = func() int64 {
		var rm metricdata.ResourceMetrics
		if err := rdr.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != "orbeat.sync.pin.payload_race_fallback" {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("unexpected data type %T for %s", md.Data, md.Name)
				}
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
		return total
	}
	return m, collect
}

// pinRaceLogLine is the shape of recordPinnedPayloadRaceFallback's log line.
type pinRaceLogLine struct {
	Event            string `json:"event"`
	Tenant           string `json:"tenant"`
	Actor            string `json:"actor"`
	ArtifactID       string `json:"artifact_id"`
	Artifact         string `json:"artifact"`
	ResolvedRevision int    `json:"resolvedRevision"`
	ServedRevision   int    `json:"servedRevision"`
	Reason           string `json:"reason"`
}

// TestResolveSyncPayloadMissingKeyRecordsFallback drives resolveSyncPayload
// (the function handleSyncArtifacts itself calls per artifact) directly with
// a payloads map missing the key res.Served resolves to. That IS the
// complete observable symptom of the race open-points.md's row names: an
// approval prunes the exact revision the window read resolved before the
// second, batched read runs. No single-threaded test can land a real
// approval between handleSyncArtifacts' two queries (open-points.md
// explains why: Server.store is a concrete *store.Store, not an interface,
// and closing that needs an integration harness or a seam this slice was
// told not to add), but nothing downstream of the window read cares HOW the
// key came to be missing, so this test builds that exact condition by hand.
func TestResolveSyncPayloadMissingKeyRecordsFallback(t *testing.T) {
	var logBuf bytes.Buffer
	metrics, collect := newManualPinMetrics(t)
	srv := &Server{logger: slog.New(slog.NewJSONHandler(&logBuf, nil)), metrics: metrics}

	a := store.Artifact{
		ID: "art-1", Name: "sk", Type: "skill", Revision: 9,
		Content: "---\nname: sk\ndescription: d\n---\nAPPROVED-SNAPSHOT-V9",
	}
	res := pinResolution{Served: 4, Oldest: 4} // resolved to revision 4, but its payload will not be found
	payloads := map[store.ArtifactRevisionKey]store.ArtifactRevisionPayload{
		// A DIFFERENT revision's payload is present, so a bug that looks up
		// the wrong key (or ignores the key entirely) cannot pass by luck.
		{ArtifactID: "art-1", RevisionNum: 7}: {Content: "wrong revision, must never be returned"},
	}

	src, seed, revision, override := srv.resolveSyncPayload(context.Background(), "tenant-1", "dev1", a, res, payloads)

	if override != pinOverridePruned {
		t.Fatalf("override = %q, want %q", override, pinOverridePruned)
	}
	if src.Content != a.Content {
		t.Fatalf("content = %q, want the approved snapshot %q", src.Content, a.Content)
	}
	if revision != a.Revision {
		t.Fatalf("revision = %d, want the artifact's current revision %d: the fallback degrades to the approved snapshot", revision, a.Revision)
	}
	if seed != a.MemorySeed {
		t.Fatalf("seed = %q, want the artifact's own memory seed %q", seed, a.MemorySeed)
	}

	if got := collect(); got != 1 {
		t.Fatalf("PinnedPayloadRaceFallback count = %d, want 1", got)
	}

	line := strings.TrimSpace(logBuf.String())
	if line == "" {
		t.Fatal("no log output at all")
	}
	var got pinRaceLogLine
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("log line is not JSON: %v; got: %s", err, line)
	}
	if got.Event != "sync.pin_payload_race" {
		t.Errorf("event = %q, want sync.pin_payload_race: %s", got.Event, line)
	}
	if got.Tenant != "tenant-1" || got.Actor != "dev1" {
		t.Errorf("tenant/actor = %q/%q, want tenant-1/dev1: %s", got.Tenant, got.Actor, line)
	}
	if got.ArtifactID != "art-1" || got.Artifact != "sk" {
		t.Errorf("artifact_id/artifact = %q/%q, want art-1/sk: %s", got.ArtifactID, got.Artifact, line)
	}
	// Deliberately distinct values (4 vs 9) so a mutant that swaps the two
	// arguments fails here rather than passing by coincidence.
	if got.ResolvedRevision != 4 {
		t.Errorf("resolvedRevision = %d, want 4 (what the clamp resolved to, the missing key)", got.ResolvedRevision)
	}
	if got.ServedRevision != 9 {
		t.Errorf("servedRevision = %d, want 9 (what was actually served, the approved snapshot)", got.ServedRevision)
	}
	if got.Reason != pinOverridePruned {
		t.Errorf("reason = %q, want %q", got.Reason, pinOverridePruned)
	}
}

// TestResolveSyncPayloadFoundNeverRecordsFallback is the complementary arm:
// an ordinary honoured pin, whose payload IS found, must not log or count
// anything. Without this arm, a mutant that calls
// recordPinnedPayloadRaceFallback unconditionally, or on every
// res.Served < a.Revision regardless of whether the payload was found,
// would pass the test above and still be wrong on every real pinned sync
// that succeeds.
func TestResolveSyncPayloadFoundNeverRecordsFallback(t *testing.T) {
	var logBuf bytes.Buffer
	metrics, collect := newManualPinMetrics(t)
	srv := &Server{logger: slog.New(slog.NewJSONHandler(&logBuf, nil)), metrics: metrics}

	a := store.Artifact{ID: "art-2", Name: "sk2", Type: "skill", Revision: 9, Content: "latest, must not be returned"}
	res := pinResolution{Served: 4, Oldest: 4}
	payloads := map[store.ArtifactRevisionKey]store.ArtifactRevisionPayload{
		{ArtifactID: "art-2", RevisionNum: 4}: {Type: "skill", Name: "sk2", Content: "PINNED-V4-CONTENT"},
	}

	src, _, revision, override := srv.resolveSyncPayload(context.Background(), "tenant-1", "dev1", a, res, payloads)

	if override != "" {
		t.Fatalf("override = %q, want empty: a found payload is not an override", override)
	}
	if src.Content != "PINNED-V4-CONTENT" {
		t.Fatalf("content = %q, want the pinned revision's own content", src.Content)
	}
	if revision != 4 {
		t.Fatalf("revision = %d, want 4", revision)
	}
	if got := collect(); got != 0 {
		t.Fatalf("PinnedPayloadRaceFallback count = %d, want 0: an ordinary honoured pin must never record the fallback", got)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("unexpected log output for an ordinary honoured pin: %s", logBuf.String())
	}
}

// TestResolveSyncPayloadUnpinnedNeverRecordsFallback covers the third and
// most common case, no pin at all (res.Served == a.Revision): the whole
// `if res.Served < a.Revision` branch, fallback included, must never run.
//
// This is also the shape every artifact takes on a Community server, where
// s.pinning is always false (pinning.community.go) and pins therefore stays
// nil (handleSyncArtifacts: `var pins map[string]int; if s.pinning { pins,
// err = parsePins(...) }`), so pins[a.ID] reads 0 for every artifact and
// pinResolve's `pin <= 0` arm always returns Served == maxNum. That chain is
// a structural fact about the source, not a runtime behaviour with its own
// seam to test: a LIVE test attempting to force the fallback's specific
// missing-key branch through the real handler was tried and removed, because
// it could not fail for the reason it claimed to. res.Served < a.Revision
// only ever holds, in any single-threaded run, when the resolved revision
// still exists (minNum/maxNum are read in the SAME query as the window), so
// the payload read that follows always finds it: the missing-key branch
// needs the real cross-request race, which open-points.md already records as
// having no single-threaded reproduction, in EITHER edition. What IS
// verifiable live, and is: TestSyncWithoutPinningIgnoresPinsEntirely above
// proves pins itself never influences a response when s.pinning is false,
// for both malformed and well-formed pin values, which is exactly the
// precondition this branch's unreachability depends on.
func TestResolveSyncPayloadUnpinnedNeverRecordsFallback(t *testing.T) {
	var logBuf bytes.Buffer
	metrics, collect := newManualPinMetrics(t)
	srv := &Server{logger: slog.New(slog.NewJSONHandler(&logBuf, nil)), metrics: metrics}

	a := store.Artifact{ID: "art-3", Name: "sk3", Type: "skill", Revision: 9, Content: "APPROVED-V9"}
	res := pinResolution{Served: 9, Oldest: 0} // no pin: served == maxNum
	src, _, revision, override := srv.resolveSyncPayload(context.Background(), "tenant-1", "dev1", a, res, nil)

	if override != "" || src.Content != a.Content || revision != a.Revision {
		t.Fatalf("unpinned resolve = (content %q, revision %d, override %q), want the artifact's own content/revision/no override",
			src.Content, revision, override)
	}
	if got := collect(); got != 0 {
		t.Fatalf("PinnedPayloadRaceFallback count = %d, want 0", got)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("unexpected log output for an unpinned sync: %s", logBuf.String())
	}
}
