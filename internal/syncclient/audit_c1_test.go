package syncclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- S5: unreachable de-registered projects must not orphan their blocks ---

// A rules ledger entry for a project whose ROOT is unreachable (unmounted
// volume) must survive a sync even when the project is no longer registered —
// the ORBEAT-RULES block may still be on disk under the unreachable root, and
// dropping the ledger entry as if cleanly stripped orphans it forever once the
// volume remounts. The v1.15.0 notLive rule covered only registered projects.
func TestReconcileRulesPreservesLedgerForUnreachableDeregisteredProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	gone := filepath.Join(home, "unmounted-volume", "repo") // never created on disk
	must(t, saveManifest(claudeDir, manifest{Rules: []string{gone}}, nil))

	// Project no longer registered (projects empty), no rule artifacts.
	res, err := ReconcileRules(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range got.Rules {
		if filepath.Clean(p) == filepath.Clean(gone) {
			found = true
		}
	}
	if !found {
		t.Fatalf("unreachable de-registered rules ledger entry was dropped (orphaned): %v", got.Rules)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about the unreachable de-registered project")
	}
}

// Seed mirror of the rules S5 fix: a project-scope seed ledger entry whose
// project root is unreachable must survive a sync (not be dropped as a clean
// strip), so a remount does not leave the ORBEAT-SEED block orphaned.
func TestReconcileSeedsPreservesLedgerForUnreachableDeregisteredProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cd := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(cd, 0o755))
	gone := filepath.Join(home, "unmounted-volume", "repo")
	seedPath := filepath.Join(gone, ".claude", "agent-memory", "rev", "MEMORY.md")
	must(t, saveManifest(cd, manifest{Seeds: map[string][]string{"rev": {seedPath}}}, nil))

	res, err := ReconcileSeeds(cd, nil, nil, nil) // project not registered, no artifacts
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seeds["rev"]) != 1 {
		t.Fatalf("unreachable de-registered seed ledger entry dropped (orphaned): %+v", got.Seeds)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about the unreachable de-registered project")
	}
}

// A reachable, REGISTERED project with the managed file genuinely absent must
// STILL strip cleanly (entry drops) — the S5 fix must not preserve entries for
// reachable roots, only unreachable ones.
//
// The project is REGISTERED here, and that changed with the trustedProjects
// fix (B23): this test used to pass projects=nil, because the strip pass
// reached into unregistered projects by trusting any path that shape-checked
// and existed on disk. It no longer does — a project must be one this run was
// actually handed, or the entry is preserved and skipped rather than dropped
// (TestReconcileRulesRefusesToStripAnUnregisteredProjectLedgerEntry owns that
// case now). Registering the project keeps this test pinning what it was
// built to pin — mirroring TestReconcileSeedsDropsLedgerForReachableAbsentProject,
// whose own comment records the identical update when trustedSeedBoundary
// landed for seeds.
func TestReconcileRulesDropsLedgerForReachableAbsentProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	reachable := t.TempDir() // exists, but carries no ORBEAT-RULES block
	must(t, saveManifest(claudeDir, manifest{Rules: []string{reachable}}, nil))

	if _, err := ReconcileRules(claudeDir, projs(reachable), nil, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 0 {
		t.Fatalf("a reachable project with no block must drop from the ledger, got %v", got.Rules)
	}
}

// Seed mirror of TestReconcileRulesDropsLedgerForReachableAbsentProject: a
// project that is REACHABLE but carries no seed block must STILL drop its
// ledger entry, because the S5 preservation must key off unreachability and
// not merely the absence of a block, or every stripped seed would be
// preserved forever.
//
// The project is REGISTERED here, and that changed with the seedBoundary fix.
// This test used to pass projects=nil, because the strip pass reached into
// unregistered projects by deriving a containment root from the ledger path
// itself. It no longer does: a containment root now comes only from the
// trusted set (claudeDir plus the registered projects), so a de-registered
// project's entry is skipped and PRESERVED rather than stripped and dropped.
// TestReconcileSeedsLeavesALedgerPathOutsideEveryTrustedRootAlone and
// TestReconcileSeedsPreservesTheLedgerEntryItRefusedToTouch own that case
// now. Registering the project keeps this test pinning what it was built
// to pin: preservation must not become blanket.
func TestReconcileSeedsDropsLedgerForReachableAbsentProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cd := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(cd, 0o755))
	reachable := t.TempDir() // exists, but carries no seed block
	seedPath := filepath.Join(reachable, ".claude", "agent-memory", "rev", "MEMORY.md")
	must(t, saveManifest(cd, manifest{Seeds: map[string][]string{"rev": {seedPath}}}, nil))

	if _, err := ReconcileSeeds(cd, []string{reachable}, nil, nil); err != nil { // registered, no artifacts
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seeds) != 0 {
		t.Fatalf("a reachable registered project with no block must drop from the ledger, got %+v", got.Seeds)
	}
}

// --- S6: connect abort-cascade ---

// A corrupt managed block (begin marker without end) must be a skip+Note, not an
// error — matching the unparseable-file handling one branch below — so it cannot
// trigger the connect abort-cascade.
func TestWriteTOMLManagedBlockCorruptMarkerSkips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	corrupt := tomlBeginMarker + "\n[mcp_servers.orbeat-gateway]\nurl = \"x\"\n" // no end marker
	must(t, os.WriteFile(path, []byte(corrupt), 0o644))
	res, err := writeTOMLManagedBlock(path, "[mcp_servers.orbeat-gateway]\nurl = \"y\"\n", true)
	if err != nil {
		t.Fatalf("corrupt marker must be a skip+note, not an error: %v", err)
	}
	if res.Note == "" {
		t.Fatal("expected a Note explaining the skip")
	}
	if res.Changed {
		t.Fatal("must not rewrite a corrupt-marker file")
	}
}

// An adapter error must not abort the whole connect run: later tools are still
// attempted and configured, and a summary error is returned (exit 1 preserved).
func TestRunConnectContinuesAfterAdapterError(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "connect.json")
	boom := fakeAdapter{name: "boom", detect: true, writeErr: errors.New("kaboom")}
	ok := fakeAdapter{name: "ok", detect: true, changed: true}
	// boom is FIRST — under the old cascade it aborted before ok was attempted.
	results, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: ledgerPath,
		adapters: []ToolAdapter{boom, ok},
	})
	if err == nil {
		t.Fatal("expected a summary error when an adapter fails")
	}
	okConfigured := false
	for _, r := range results {
		if r.Tool == "ok" && r.Result.Changed {
			okConfigured = true
		}
	}
	if !okConfigured {
		t.Fatalf("a later tool was never configured after an earlier adapter error: %+v", results)
	}
	l, _ := LoadConnectLedger(ledgerPath)
	if _, present := l["ok"]; !present {
		t.Fatal("ledger did not record the tool configured after the earlier failure")
	}
}

// --- S9: --remove against an unparseable config must not drop the ledger ---

func TestRunConnectRemoveKeepsLedgerOnUnparseableConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	must(t, os.MkdirAll(filepath.Join(home, ".cursor"), 0o755))
	ledgerPath := filepath.Join(home, "connect.json")
	if _, err := RunConnect(ConnectOptions{GatewayURL: "https://gw", LedgerPath: ledgerPath}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the config the orbeat entry was written into.
	must(t, os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte("{ not json"), 0o644))

	results, err := RunConnect(ConnectOptions{Remove: true, LedgerPath: ledgerPath})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	l, _ := LoadConnectLedger(ledgerPath)
	if _, present := l["cursor"]; !present {
		t.Fatal("ledger entry dropped despite an unparseable config (orbeat entry now unmanageable)")
	}
	noted := false
	for _, r := range results {
		if r.Tool == "cursor" && r.Result.Note != "" {
			noted = true
		}
	}
	if !noted {
		t.Fatal("expected a Note about the unparseable config on remove")
	}
}

// --- S9: Detect() must not stat CWD-relative paths when HOME is unresolvable ---

func TestCodexDetectFalseWhenHomeUnresolvable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	must(t, os.Mkdir(filepath.Join(dir, ".codex"), 0o755))
	t.Setenv("HOME", "") // makes os.UserHomeDir error on unix/darwin
	if newCodexAdapter().Detect() {
		t.Fatal("Detect must be false when HOME is unresolvable (must not stat a CWD-relative .codex)")
	}
}

func TestJSONDetectFalseWhenHomeUnresolvable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	must(t, os.Mkdir(filepath.Join(dir, ".cursor"), 0o755))
	t.Setenv("HOME", "")
	if newCursorAdapter().Detect() {
		t.Fatal("Detect must be false when HOME is unresolvable (must not stat a CWD-relative .cursor)")
	}
}

// --- S9: seed writes must preserve the target file's existing mode ---

func TestReconcileSeedsPreservesFileMode(t *testing.T) {
	cd := t.TempDir()
	if _, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v1")}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
	must(t, os.Chmod(target, 0o600))
	if _, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v2")}, nil); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(target)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("seed write widened mode to %v (must preserve 0600)", fi.Mode().Perm())
	}
}

// --- S9: unknown MemoryScope warns (matching the unknown-type warn) ---

func TestReconcileSeedsUnknownScopeWarns(t *testing.T) {
	cd := t.TempDir()
	res, err := ReconcileSeeds(cd, nil, []Artifact{
		{Type: "subagent", Name: "rev", Content: "c", MemoryScope: "workspace", MemorySeed: "S"},
	}, nil)

	if err != nil {
		t.Fatalf("an unknown scope must be skipped, not fatal: %v", err)
	}
	if res.Written != 0 {
		t.Fatalf("unknown scope must not write, got %d", res.Written)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for an unrecognized memory scope")
	}
}

// The known-but-not-seeded "local" scope must stay silent (not warn).
func TestReconcileSeedsLocalScopeSilent(t *testing.T) {
	cd := t.TempDir()
	res, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("loc", "local", "s")}, nil)
	if err != nil || res.Written != 0 {
		t.Fatalf("local scope must not seed: res=%+v err=%v", res, err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("the known 'local' scope must not warn, got %v", res.Warnings)
	}
}

// --- S9: seeds ledger must dedupe paths (rules already does) ---

func TestReconcileSeedsLedgerDedupesPaths(t *testing.T) {
	cd := t.TempDir()
	proj := filepath.Join(cd, "proj")
	must(t, os.MkdirAll(proj, 0o755))
	// A duplicated project entry would append the same seed path twice.
	if _, err := ReconcileSeeds(cd, []string{proj, proj}, []Artifact{seedArt("rev", "project", "s")}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Seeds["rev"]) != 1 {
		t.Fatalf("seeds ledger must dedupe paths, got %v", m.Seeds["rev"])
	}
}

// --- S9: device login must tolerate a few transient poll errors ---

type flakyRoundTripper struct {
	base  http.RoundTripper
	failN int
	n     *int
}

func (f flakyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/token") {
		*f.n++
		if *f.n <= f.failN {
			return nil, fmt.Errorf("simulated transport failure %d", *f.n)
		}
	}
	return f.base.RoundTrip(req)
}

func TestLoginToleratesTransientPollErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":300}`))
	}))
	defer srv.Close()
	n := 0
	client := &http.Client{Transport: flakyRoundTripper{base: http.DefaultTransport, failN: 2, n: &n}}
	a := &Authenticator{HTTPClient: client, ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	tok, err := a.Login(context.Background(), meta, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("login must tolerate a couple of transient poll errors: %v", err)
	}
	if tok.AccessToken != "AT" {
		t.Fatalf("bad token: %+v", tok)
	}
}

// Persistent transport errors must still eventually give up (bounded, not infinite).
func TestLoginGivesUpAfterConsecutivePollErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":300}`))
	}))
	defer srv.Close()
	n := 0
	client := &http.Client{Transport: flakyRoundTripper{base: http.DefaultTransport, failN: 1000, n: &n}}
	a := &Authenticator{HTTPClient: client, ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	if _, err := a.Login(context.Background(), meta, &bytes.Buffer{}); err == nil {
		t.Fatal("persistent poll errors must eventually fail login")
	}
}

// A token-endpoint failure with an empty "error" field must still yield a
// non-empty, diagnosable message (include the status), never "device login failed: ".
func TestLoginErrorMessageNeverEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":""}`)) // non-200, empty error
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	_, err := a.Login(context.Background(), meta, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.TrimSpace(strings.TrimPrefix(err.Error(), "device login failed:")) == "" {
		t.Fatalf("error message is empty/uninformative: %q", err.Error())
	}
}

// error_description must be decoded into the message when present.
func TestLoginErrorMessageIncludesDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"the user rejected the request"}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	_, err := a.Login(context.Background(), meta, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "the user rejected the request") {
		t.Fatalf("error must include error_description, got %v", err)
	}
}

// --- S9: reconcile removal accounting + prune depth ---

// The empty-dir prune after a removal must never delete a SHARED top-level type
// dir (agents/) — only an artifact-owned subdir (skills/<name>). Removing the
// last subagent must leave ~/.claude/agents in place.
func TestReconcileRemovalKeepsSharedAgentsDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reconcile(dir, []Artifact{{Type: "subagent", Name: "rev", Content: "A"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(dir, nil, nil); err != nil { // de-entitle → agents/rev.md removed
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "rev.md")); !os.IsNotExist(err) {
		t.Fatal("subagent file should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "agents")); err != nil {
		t.Fatalf("shared agents/ dir must survive the prune, got: %v", err)
	}
}

// `Removed` must count only files this run actually removed, not manifest
// entries that were already absent from disk.
func TestReconcileRemovedCountsOnlyActualRemovals(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(dir, 0o755))
	// Manifest claims a managed file that does not exist on disk.
	must(t, saveManifest(dir, manifest{Files: []string{"skills/ghost/SKILL.md"}}, nil))
	res, err := Reconcile(dir, nil, nil) // ghost no longer desired, but already absent
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Fatalf("an already-missing managed file must not count as Removed, got %d", res.Removed)
	}
}

// --- S9: IsFatal export ---

func TestIsFatalExported(t *testing.T) {
	if !IsFatal(markFatal(errors.New("x"))) {
		t.Fatal("IsFatal must report a fatal error")
	}
	if IsFatal(errors.New("plain")) {
		t.Fatal("IsFatal must be false for a plain error")
	}
	if IsFatal(nil) {
		t.Fatal("IsFatal(nil) must be false")
	}
}
