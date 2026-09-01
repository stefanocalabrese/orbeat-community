package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

// stubAdapter is a minimal ToolAdapter for exercising the connect reporting path.
type stubAdapter struct{ name, hint, caveat string }

func (s stubAdapter) Name() string { return s.name }
func (s stubAdapter) Detect() bool { return true }
func (s stubAdapter) WriteMCP(string, bool) (syncclient.Result, error) {
	return syncclient.Result{}, nil
}
func (s stubAdapter) RemoveMCP() (syncclient.Result, error) { return syncclient.Result{}, nil }
func (s stubAdapter) AuthHint() string                      { return s.hint }
func (s stubAdapter) Caveat() string                        { return s.caveat }

// captureOutput runs fn with the given standard stream (&os.Stdout or &os.Stderr)
// redirected to a pipe and returns what was printed.
func captureOutput(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	orig := *stream
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	*stream = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	*stream = orig
	return <-done
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t, &os.Stderr, fn)
}

// fable-audit §7 #6: `usage()` never mentioned the 0/1/2 exit contract, even
// though it's documented in a source comment (exitError's doc comment above)
// and the changelog — a user running `orbeat-sync` with no args (the most
// likely way to discover it) saw neither. It must be visible right here.
//
// Each needle pairs a CODE with its MEANING contiguously, deliberately. An
// earlier version of this test asserted the tokens separately ("0", "clean",
// … "do not retry") and would have passed a banner that listed all the right
// words against all the WRONG codes — e.g. "0 fatal … 2 clean". A test for a
// contract that survives the contract being inverted is not a test.
func TestUsageMentionsExitContract(t *testing.T) {
	out := captureStderr(t, usage)
	for _, want := range []string{
		"0  clean",
		"1  partial",
		"2  fatal",
		"retry when the condition clears",
		"do not retry",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage() output missing %q (the 0/1/2 exit contract must be visible, with each code next to its own meaning); got:\n%s", want, out)
		}
	}
}

// fable-audit §7 #15: nothing in orbeat could say what version it is. usage()
// is the surface a user reaches for first (`orbeat-sync` with no args), so
// --version must be visible there too, not just work when guessed.
func TestUsageMentionsVersionFlag(t *testing.T) {
	out := captureStderr(t, usage)
	if !strings.Contains(out, "--version") {
		t.Fatalf("usage() output missing \"--version\"; got:\n%s", out)
	}
}

// TestDispatchVersionPrintsVersion proves `orbeat-sync --version` prints
// "orbeat-sync <internal/version.Version>" and exits cleanly (nil), and that
// it tracks whatever internal/version.Version currently holds rather than a
// value frozen at some other point — the same defect class as the gateway's
// stale hardcoded literal (fable-audit §7 #15). Flipping version.Version
// mid-test and re-dispatching proves this is a live read, not a copy taken
// once at package init.
func TestDispatchVersionPrintsVersion(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	for _, v := range []string{"dev", "sync-version-test-7c1a"} {
		version.Version = v
		var err error
		out := captureStdout(t, func() {
			err = dispatch(context.Background(), syncclient.Config{}, []string{"--version"})
		})
		if err != nil {
			t.Fatalf("dispatch(--version) with Version=%q: err = %v, want nil", v, err)
		}
		want := "orbeat-sync " + v
		if strings.TrimSpace(out) != want {
			t.Fatalf("dispatch(--version) with Version=%q: stdout = %q, want %q", v, strings.TrimSpace(out), want)
		}
	}
}

// TestDispatchVersionRejectsExtraArgs: --version takes no arguments.
func TestDispatchVersionRejectsExtraArgs(t *testing.T) {
	err := dispatch(context.Background(), syncclient.Config{}, []string{"--version", "extra"})
	if err == nil {
		t.Fatal("--version extra: expected an error, got nil (the extra arg was silently ignored)")
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("--version extra: err = %v, want a 'takes no arguments' rejection", err)
	}
}

// S6: a summary error from one failed adapter must NOT discard the successful
// tools' one-time auth hints — reportConnect prints results BEFORE returning err.
func TestReportConnectPrintsHintsDespiteAdapterError(t *testing.T) {
	results := []syncclient.ToolResult{
		{Tool: "boom", Adapter: stubAdapter{name: "boom"}, Result: syncclient.Result{Note: "error: kaboom"}},
		{Tool: "ok", Adapter: stubAdapter{name: "ok", hint: "OK: run `ok mcp login`"}, Result: syncclient.Result{Changed: true, Path: "/x/ok"}},
	}
	var got error
	out := captureStdout(t, func() {
		got = reportConnect(results, false, errors.New("connect: 1 tool(s) failed: boom: kaboom"))
	})
	if got == nil {
		t.Fatal("reportConnect must still return the summary error")
	}
	if !strings.Contains(out, "OK: run `ok mcp login`") {
		t.Fatalf("the successful adapter's auth hint was discarded on a partial failure; stdout=%q", out)
	}
	if !strings.Contains(out, "configured (/x/ok)") {
		t.Fatalf("the successful adapter's configured line was discarded; stdout=%q", out)
	}
}

// S9: subcommands that take no flags must REJECT extra args instead of silently
// ignoring them — most dangerously `orbeat-sync sync --dry-run`, which would
// otherwise perform a REAL sync (there is no real --dry-run). The guard must
// fire BEFORE the handler runs; asserting the "takes no arguments" message
// proves the guard fired, not a real run.
func TestDispatchRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from the real ~/.config/orbeat and ~/.claude
	cfg := syncclient.Config{}
	for _, sub := range []string{"logout", "status"} {
		err := dispatch(context.Background(), cfg, []string{sub, "--dry-run"})
		if err == nil {
			t.Fatalf("%s --dry-run: expected an error, got nil (the arg was silently ignored)", sub)
		}
		if !strings.Contains(err.Error(), "takes no arguments") {
			t.Fatalf("%s --dry-run: err = %v, want a 'takes no arguments' rejection (proves the guard fired, not a real run)", sub, err)
		}
	}
	// login owns a FlagSet now (--browser), so its rejection comes from the
	// flag package rather than from noExtraArgs. The property under test is
	// unchanged and is the one that matters: an unrecognised flag must STOP the
	// command rather than be ignored, because a login that silently ignored
	// --browser would run the device flow while the user believes a browser is
	// about to open.
	err := dispatch(context.Background(), cfg, []string{"login", "--dry-run"})
	if err == nil {
		t.Fatal("login --dry-run: expected an error, got nil (the flag was silently ignored)")
	}
	if !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("login --dry-run: err = %v, want the flag package's unknown-flag rejection", err)
	}
	// And a positional argument, which the flag package does NOT reject on its
	// own: it stops parsing and hands it back, so login has to refuse it.
	err = dispatch(context.Background(), cfg, []string{"login", "stray"})
	if err == nil || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("login stray: err = %v, want a positional-argument rejection", err)
	}
}

// TestSyncAcceptsJSONFlag proves --json is parsed rather than rejected. It is
// the inverse of TestDispatchRejectsExtraArgs, which still guards the other
// subcommands.
func TestSyncAcceptsJSONFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := dispatch(context.Background(), syncclient.Config{}, []string{"sync", "--json"})
	if err != nil && strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("sync --json was rejected as an extra argument: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("sync --json is not a defined flag: %v", err)
	}
	// Any OTHER error is fine here: with no token the run fails at auth, which
	// is exactly how far this test intends to get.
}

// TestSyncAcceptsDryRunFlag mirrors TestSyncAcceptsJSONFlag now that --dry-run
// is real: it must reach the same auth-time failure --json does, not be
// rejected as an extra/undefined argument the way v1.18.0's trap made it look.
func TestSyncAcceptsDryRunFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := dispatch(context.Background(), syncclient.Config{}, []string{"sync", "--dry-run"})
	if err != nil && strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("sync --dry-run was rejected as an extra argument: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("sync --dry-run is not a defined flag: %v", err)
	}
	// Any OTHER error is fine here: with no token the run fails at auth, which
	// is exactly how far this test intends to get.
}

// S9.3: a fatal integrity abort raised via `project remove` (which does not wrap
// its error in an exitError) must still map to exit 2, not 1.
func TestProjectRemoveFatalManifestExits2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	cd := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	// A corrupt manifest makes StripProjectSeeds/Rules return a FATAL error.
	if err := os.WriteFile(filepath.Join(cd, ".orbeat-sync-manifest.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	if _, err := syncclient.AddProject(pj, proj, nil); err != nil {
		t.Fatal(err)
	}
	err := runProject([]string{"remove", proj})
	if got := exitCode(err); got != 2 {
		t.Fatalf("a corrupt-manifest fatal from `project remove` must exit 2, got code %d (err %v)", got, err)
	}
}

// B24: `project remove` must not de-register a project until its strip has
// actually succeeded. Before this fix, syncclient.RemoveProject ran FIRST
// (writing projects.json), and only THEN did the handler attempt
// StripProjectSeeds/StripProjectRules — so a non-fatal, per-candidate strip
// failure (an unreadable MEMORY.md; here, EISDIR) still returned an error to
// the user, but the project was ALREADY gone from projects.json. Once a
// project is unregistered, its ledger entries stop being trusted
// (trustedSeedBoundary and its rules-side counterpart, B23), so a future
// ordinary sync would never reach them again — the block strands permanently.
//
// This reproduces the exact scenario the audit named: one unreadable
// MEMORY.md, reachable project, non-fatal (not corrupt-manifest) failure.
func TestProjectRemoveDoesNotDeregisterOnANonFatalStripFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cd := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	if _, err := syncclient.AddProject(pj, proj, nil); err != nil {
		t.Fatal(err)
	}

	// A real seed first, so it lands in the manifest ledger the normal way...
	art := []syncclient.Artifact{{Type: "subagent", Name: "badagent", MemoryScope: "project", MemorySeed: "x"}}
	if _, err := syncclient.ReconcileSeeds(cd, []string{proj}, art, nil); err != nil {
		t.Fatalf("seed badagent: %v", err)
	}
	badPath := filepath.Join(proj, ".claude", "agent-memory", "badagent", "MEMORY.md")
	// ...then corrupted into an unreadable candidate (EISDIR), reproducing
	// "one unreadable MEMORY.md during project remove" without needing
	// root-independent permission bits.
	if err := os.Remove(badPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(badPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := runProject([]string{"remove", proj})
	if err == nil {
		t.Fatal("an unreadable candidate must surface as an error, not silently succeed")
	}
	if got := exitCode(err); got == 2 {
		t.Fatalf("a per-candidate I/O failure is non-fatal (retryable), must not exit 2: %v", err)
	}

	list, lerr := syncclient.LoadProjects(pj)
	if lerr != nil {
		t.Fatal(lerr)
	}
	found := false
	for _, p := range list {
		if p.Path == proj {
			found = true
		}
	}
	if !found {
		t.Fatalf("the project must stay REGISTERED when the strip did not fully succeed, or its remaining blocks strand permanently once unregistered; projects=%v", list)
	}
}

// S9.7: `status` must not promise a refresh when there is no refresh token.
func TestStatusMessageBranchesOnRefreshToken(t *testing.T) {
	noRefresh := syncclient.Token{AccessToken: "a", Expiry: time.Now().Add(-time.Hour)}
	if msg := statusMessage(noRefresh); !strings.Contains(msg, "run 'orbeat-sync login'") {
		t.Fatalf("expired token with no refresh token must direct the user to login, got %q", msg)
	}
	withRefresh := syncclient.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}
	if msg := statusMessage(withRefresh); strings.Contains(msg, "there is no refresh token") {
		t.Fatalf("a token WITH a refresh token must not claim there is none, got %q", msg)
	}
	valid := syncclient.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}
	if msg := statusMessage(valid); !strings.Contains(msg, "valid until") {
		t.Fatalf("a valid token must report its validity, got %q", msg)
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{" a , b ,,c ", []string{"a", "b", "c"}},
		{",", nil},
	}
	for _, tc := range cases {
		if got := splitList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"clean", nil, 0},
		{"partial", &exitError{code: 1, err: errors.New("x")}, 1},
		{"fatal", &exitError{code: 2, err: errors.New("y")}, 2},
		{"plain error defaults to 1", errors.New("boom"), 1},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("%s: exitCode = %d, want %d", c.name, got, c.want)
		}
	}
}

// S7: the "restart Claude Code" hint must fire only when the agent set actually
// changed on disk — a steady-state run (everything Unchanged) must stay silent,
// while an add, update, or removal each warrant a restart.
func TestRestartHintNeeded(t *testing.T) {
	cases := []struct {
		name string
		res  syncclient.ReconcileResult
		want bool
	}{
		{"steady state all unchanged", syncclient.ReconcileResult{Unchanged: 3, Handled: 3}, false},
		{"nothing at all", syncclient.ReconcileResult{}, false},
		{"added", syncclient.ReconcileResult{Added: 1}, true},
		{"updated", syncclient.ReconcileResult{Updated: 1, Unchanged: 2}, true},
		{"removed only", syncclient.ReconcileResult{Removed: 1, Unchanged: 2}, true},
	}
	for _, c := range cases {
		if got := restartHintNeeded(c.res); got != c.want {
			t.Errorf("%s: restartHintNeeded = %v, want %v", c.name, got, c.want)
		}
	}
}

// S3: `project add` mutates projects.json (writeFileAtomic), so it must hold
// the run lock like sync / project remove / connect — an unlocked add racing a
// locked run reintroduces the stale-temp glob-delete race the lock closes.
func TestProjectAddRequiresRunLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir honors $HOME on unix

	release, err := syncclient.AcquireLock(filepath.Join(home, ".config", "orbeat"))
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer release()

	err = runProject([]string{"add", t.TempDir()})
	if !errors.Is(err, syncclient.ErrLocked) {
		t.Fatalf("project add while another run holds the lock: err = %v, want ErrLocked", err)
	}
}

// Gate 10 (artifact-version-pinning plan, Task 6): every MUTATING pin form
// holds the run lock exactly like project add above: a blind write to
// pins.json while a real run is in flight races that run's own reconcile.
// Both fail before ever reaching the network, so no fake server is needed
// here at all. 'pin list' is deliberately excluded: it never writes and
// mirrors 'status'/'doctor' in staying lock-free.
func TestPinRequiresRunLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir honors $HOME on unix

	release, err := syncclient.AcquireLock(filepath.Join(home, ".config", "orbeat"))
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer release()

	if err := runPinSet(context.Background(), syncclient.Config{}, "skill/pinme", nil); !errors.Is(err, syncclient.ErrLocked) {
		t.Fatalf("pin while another run holds the lock: err = %v, want ErrLocked", err)
	}
	if err := runPinRemove("skill/pinme"); !errors.Is(err, syncclient.ErrLocked) {
		t.Fatalf("pin remove while another run holds the lock: err = %v, want ErrLocked", err)
	}
}

func TestReconcileAllCleanExit0(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	_, _, err := reconcileAll(claudeDir, projs(proj),
		[]syncclient.Artifact{{Type: "rule", Name: "r", Content: "x"}}, plans{})
	if err != nil {
		t.Fatalf("clean sync must return nil (exit 0), got %v", err)
	}
}

// A non-fatal per-unit failure must be visible and map to exit 1.
func TestReconcileAllPartialExit1(t *testing.T) {
	claudeDir := t.TempDir()
	bad := t.TempDir()
	// A directory at AGENTS.md makes the rules write fail non-fatally.
	if err := os.MkdirAll(filepath.Join(bad, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := reconcileAll(claudeDir, projs(bad),
		[]syncclient.Artifact{{Type: "rule", Name: "r", Content: "x"}}, plans{})
	if exitCode(err) != 1 {
		t.Fatalf("a non-fatal failure must map to exit 1, got %v (code %d)", err, exitCode(err))
	}
}

// The same fixture in PLAN mode must never yield exit 1: nothing was applied,
// so nothing partially failed. See TestPlanModeNeverExitsOne below for the
// dedicated exit-contract coverage; this one stays exit-1 for the real path.
//
// A fatal error maps to 2 AND stops the later reconcilers (the Level-1 cascade fix).
func TestReconcileAllFatalExit2SkipsLater(t *testing.T) {
	claudeDir := t.TempDir()
	// Corrupt manifest -> the FIRST reconciler returns fatal.
	if err := os.WriteFile(filepath.Join(claudeDir, ".orbeat-sync-manifest.json"),
		[]byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	_, _, err := reconcileAll(claudeDir, projs(proj),
		[]syncclient.Artifact{{Type: "rule", Name: "r", Content: "x"}}, plans{})
	if exitCode(err) != 2 {
		t.Fatalf("a fatal error must map to exit 2, got %v (code %d)", err, exitCode(err))
	}
	// ReconcileRules must NOT have run.
	if b, _ := os.ReadFile(filepath.Join(proj, "AGENTS.md")); strings.Contains(string(b), "ORBEAT-RULES") {
		t.Fatal("rules reconciler ran after a fatal abort — the cascade was not stopped")
	}
}

// A planned run that recorded failures must still exit 0: exit 1 means a
// partial APPLY, and a plan applies nothing. Reuses TestReconcileAllPartialExit1's
// fixture (a directory sitting at AGENTS.md) — mergeRulesFile reads the target
// through the containment root BEFORE any write decision, and reads are never
// gated by plan mode, so the same read failure fires whether or not writes are
// being recorded, without ever touching disk.
func TestPlanModeNeverExitsOne(t *testing.T) {
	claudeDir := t.TempDir()
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	o, _, err := reconcileAll(claudeDir, projs(bad),
		[]syncclient.Artifact{{Type: "rule", Name: "r", Content: "x"}},
		plans{dryRun: true, artifacts: &syncclient.Plan{}, seeds: &syncclient.Plan{}, rules: &syncclient.Plan{}})
	if exitCode(err) != 0 {
		t.Fatalf("plan mode must never exit 1, got %v (code %d)", err, exitCode(err))
	}
	// Precondition: the failure really happened, so a vacuous "nothing failed"
	// path isn't what made this pass.
	if o.Rules == nil || len(o.Rules.Failures) == 0 {
		t.Fatalf("precondition failed: the plan must have recorded a failure; rules = %+v", o.Rules)
	}
}

// A fatal abort must NOT set RestartRequired even when the file reconciler
// already changed the agent set. syncclient.Reconcile increments Added and can
// THEN abort fatally: its remove loop resolves untrusted manifest entries
// (reconcile.go:168-172) after the write loop. The pre-refactor code never
// reached restartHintNeeded on an abort, and the two renderers must not
// disagree about the same run (spec §5).
func TestReconcileAllFatalNeverSetsRestartRequired(t *testing.T) {
	claudeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, ".orbeat-sync-manifest.json"),
		[]byte(`{"files":["../escape.md"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, _, err := reconcileAll(claudeDir, nil,
		[]syncclient.Artifact{{Type: "subagent", Name: "alpha", Content: "hi"}}, plans{})
	if exitCode(err) != 2 {
		t.Fatalf("want exit 2, got %v (code %d)", err, exitCode(err))
	}
	// Precondition: without a real write before the abort this passes vacuously
	// — RestartRequired would be false for the wrong reason.
	if o.Artifacts == nil || o.Artifacts.Added == 0 {
		t.Fatalf("precondition failed: the abort must follow a real write; artifacts = %+v", o.Artifacts)
	}
	if o.RestartRequired {
		t.Fatal("RestartRequired set on a fatal abort — renderHuman would print the restart hint the pre-refactor code never printed")
	}
	if o.Seeds != nil || o.Rules != nil {
		t.Fatalf("sections after the abort must stay nil: seeds=%+v rules=%+v", o.Seeds, o.Rules)
	}
}

// fable-audit §7 #15 (doctor half): usage() must mention the new subcommand,
// the same way it already mentions login/sync/logout/status/project/connect —
// otherwise the surface a user reaches for first never tells them it exists.
func TestUsageMentionsDoctor(t *testing.T) {
	out := captureStderr(t, usage)
	if !strings.Contains(out, "doctor") {
		t.Fatalf("usage() output missing \"doctor\"; got:\n%s", out)
	}
}

// Task 5, test 1: doctor must be reachable through dispatch, not fall through
// to errUsage the way an unrecognised subcommand does.
func TestDispatchDoctorReachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from the real ~/.claude and ~/.config/orbeat

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if errors.Is(err, errUsage) {
		t.Fatal("dispatch(doctor) returned errUsage — the doctor subcommand is not wired into dispatch")
	}
	if err != nil {
		t.Fatalf("dispatch(doctor) = %v, want nil", err)
	}
	if out == "" {
		t.Fatal("dispatch(doctor) produced no output")
	}
}

// Gate (review finding on 15f41f3, applied to a sibling wiring point):
// runDoctor threads DefaultInstallPath and DefaultPinsPath into Diagnose in
// the order its signature expects (installPath, then pinsPath). No other
// doctor test in this file seeds both files distinctly enough to see a
// swap: this one does. A HEALTHY install.json (a valid uuid) sits beside an
// UNPARSEABLE pins.json; under correct wiring the PROBLEM lands on the
// "pins" check group, and the "install" group never prints at all. Under a
// swapped call the malformed content lands on install.json's path (still a
// PROBLEM, but the wrong check) while the healthy install.json's content
// parses harmlessly as an empty pins document (unknown JSON keys are
// ignored), so "pins" would go silent and "install" would fire instead.
func TestDoctorPassesInstallAndPinsPathsInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(home, ".config", "orbeat")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "install.json"),
		[]byte(`{"installId":"11111111-1111-4111-8111-111111111111"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "pins.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "pins:") {
		t.Fatalf("doctor did not report the pins check at all (install/pins paths swapped?); got:\n%s", out)
	}
	if strings.Contains(out, "install:") {
		t.Fatalf("doctor reported an install finding against a HEALTHY install.json (install/pins paths swapped?); got:\n%s", out)
	}
}

// Task 5, test 2: a healthy tree (an existing sync root, no registered
// projects) must report clean and exit 0.
func TestDoctorExitsCleanOnHealthyTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if err != nil {
		t.Fatalf("doctor on a healthy tree: err = %v, want nil", err)
	}
	if exitCode(err) != 0 {
		t.Fatalf("doctor on a healthy tree: exitCode = %d, want 0", exitCode(err))
	}
	if !strings.Contains(out, "Clean") {
		t.Fatalf("doctor on a healthy tree did not say so plainly; got:\n%s", out)
	}
}

// Task 5, test 3 — THE ONE THAT MATTERS (spec §6): doctor exits 0 even when it
// finds real problems. Exit code is reserved for doctor itself being unable to
// run; a diagnosis that reports a problem is working, not failing. A cron
// recipe that branches on orbeat-sync's exit code must not abort because
// doctor found something to tell a human about.
func TestDoctorExitsCleanEvenWithProblems(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Register a project, then remove it from disk — CheckProject fires: a
	// registered project doctor cannot stat is a SeverityProblem finding.
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	proj := t.TempDir()
	if _, err := syncclient.AddProject(pj, proj, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(proj); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	// Precondition: a problem was actually reported, so the exit-0 assertion
	// below isn't passing vacuously because nothing was found.
	if !strings.Contains(out, "PROBLEM") {
		t.Fatalf("precondition failed: no PROBLEM finding was reported; got:\n%s", out)
	}
	if err != nil {
		t.Fatalf("doctor exited non-zero (%v) on a tree WITH problems — exit code must ALWAYS be 0 here; non-zero is reserved for doctor itself failing to run (spec §6)", err)
	}
	if exitCode(err) != 0 {
		t.Fatalf("doctor on a tree WITH problems: exitCode = %d, want 0", exitCode(err))
	}
}

// TestDoctorReportsAMalformedProjectsFileEntryEndToEnd drives the real
// dispatch("doctor") path, not Diagnose directly — it is the wiring proof
// that runDoctor actually resolves projects.json's own path (pj, already
// computed for LoadProjects) and threads it through to Diagnose's new
// parameter, rather than Diagnose's check existing but never being reachable
// from the CLI a developer actually runs.
func TestDoctorReportsAMalformedProjectsFileEntryEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	if err := os.MkdirAll(filepath.Dir(pj), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pj, []byte(`{"projects":["relative/path"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "projects-file:") {
		t.Fatalf("doctor did not report the projects-file check at all; got:\n%s", out)
	}
	if !strings.Contains(out, "relative/path") {
		t.Fatalf("doctor's finding did not name the malformed entry; got:\n%s", out)
	}
	if exitCode(err) != 0 {
		t.Fatalf("doctor on a tree WITH problems: exitCode = %d, want 0", exitCode(err))
	}
}

// TestDoctorHumanRendersTheLiteralProblemCount drives renderDoctorHuman
// through the real dispatch path (not a hand-built Report) and pins the
// trailing summary line's exact text. TestDoctorExitsCleanEvenWithProblems
// above only greps for the per-finding "PROBLEM" label, which a Report whose
// Problems() always returns 0 satisfies just as well: renderDoctorHuman would
// still print "[PROBLEM] ..." for the finding itself and then, on the
// trailing line, "0 problem(s), 2 note(s)." — silently wrong, and nothing
// upstream of this test would notice. Same fixture as
// TestDoctorExitsCleanEvenWithProblems (one unreachable registered project),
// plus the always-present CheckAuth note every Diagnose call appends — so the
// tree has exactly one problem and exactly one note, not zero notes.
func TestDoctorHumanRendersTheLiteralProblemCount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	proj := t.TempDir()
	if _, err := syncclient.AddProject(pj, proj, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(proj); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if err != nil {
		t.Fatalf("dispatch(doctor) = %v, want nil", err)
	}
	if !strings.Contains(out, "1 problem(s), 1 note(s).") {
		t.Fatalf("want the literal trailing line %q, got:\n%s", "1 problem(s), 1 note(s).", out)
	}
	// Not "Clean — " prefixed: problems > 0 here, unlike the healthy-tree case.
	if strings.Contains(out, "Clean") {
		t.Fatalf("a tree with a real problem must not be labelled Clean; got:\n%s", out)
	}
}

// Task 5, test 4: --json's "findings" must serialise as [] rather than null
// when there is nothing to report — the outcome.go convention (see strs).
//
// A healthy tree can no longer produce zero findings: Diagnose always appends
// the CheckAuth note (see internal/syncclient/doctor.go), so this now asserts
// exactly one finding — that note — with zero problems, rather than zero
// findings. The narrower "nil slice serialises as [] not null" guarantee this
// test originally existed to pin is no longer reachable through a live
// Diagnose call at all (there is always at least one finding); it still holds
// structurally in renderDoctorJSON's nil-guard, proven directly by
// TestRenderDoctorJSONNilFindingsSerializesAsEmptyArray below.
func TestDoctorJSONHealthyTreeHasOnlyTheAuthNote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor", "--json"})
	})
	if err != nil {
		t.Fatalf("doctor --json: err = %v, want nil", err)
	}

	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
		t.Fatalf("doctor --json did not emit one parseable object: %v; got:\n%s", jerr, out)
	}
	findings, ok := m["findings"].([]any)
	if !ok {
		t.Fatalf(`doctor --json "findings" = %#v (%T), want an array, not null`, m["findings"], m["findings"])
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding (the always-present auth note) on a healthy tree, got %d: %v", len(findings), findings)
	}
	finding, ok := findings[0].(map[string]any)
	if !ok {
		t.Fatalf("finding[0] = %#v, want an object", findings[0])
	}
	if finding["check"] != string(syncclient.CheckAuth) {
		t.Errorf(`finding[0]["check"] = %v, want %q`, finding["check"], syncclient.CheckAuth)
	}
	if finding["severity"] != string(syncclient.SeverityNote) {
		t.Errorf(`finding[0]["severity"] = %v, want %q — it must never count as a problem`, finding["severity"], syncclient.SeverityNote)
	}
	if problems, ok := m["problems"].(float64); !ok || problems != 0 {
		t.Errorf(`"problems" = %v, want 0`, m["problems"])
	}
}

// TestRenderDoctorJSONNilFindingsSerializesAsEmptyArray pins the nil-guard in
// renderDoctorJSON directly, at the unit level. Diagnose itself can no longer
// produce a zero-finding Report (see TestDoctorJSONHealthyTreeHasOnlyTheAuthNote
// above), so the "findings serialises as [] never null" guarantee the schema
// promises is no longer reachable through a live diagnosis and needs its own
// proof that the guard code still does what it says.
func TestRenderDoctorJSONNilFindingsSerializesAsEmptyArray(t *testing.T) {
	var buf strings.Builder
	if err := renderDoctorJSON(&buf, syncclient.Report{}); err != nil {
		t.Fatalf("renderDoctorJSON: %v", err)
	}

	var m map[string]any
	if jerr := json.Unmarshal([]byte(buf.String()), &m); jerr != nil {
		t.Fatalf("not parseable JSON: %v; got:\n%s", jerr, buf.String())
	}
	findings, ok := m["findings"].([]any)
	if !ok {
		t.Fatalf(`"findings" = %#v (%T), want an empty array, not null`, m["findings"], m["findings"])
	}
	if len(findings) != 0 {
		t.Fatalf("want zero findings for a zero-value Report, got %d: %v", len(findings), findings)
	}
}

// TestDispatchDoctorRejectsExtraArgs is doctor-fixes §4 item 6: dispatch's own
// doc comment says no subcommand silently ignores an argument it does not
// understand (S9's login/logout/status/sync guard), but runDoctor never
// checked fs.NArg() — "orbeat-sync doctor bogus" parsed cleanly (flag.Parse
// only rejects unknown FLAGS, and "bogus" isn't one; it lands in fs.Args())
// and ran a completely normal diagnosis, exit 0, silently ignoring "bogus".
// runSync, runConnect and project list shared the same defect; they are fixed
// and covered by TestDispatchSyncRejectsExtraArgs, TestDispatchConnectRejectsExtraArgs
// and TestDispatchProjectListRejectsExtraArgs below (2026-08-19 slice).
func TestDispatchDoctorRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor", "bogus"})
	})
	if err == nil {
		t.Fatalf("doctor bogus: expected an error, got nil (the argument was silently ignored); stdout=%q", out)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("doctor bogus: err = %v, want it to name the unexpected argument", err)
	}
}

// 2026-08-19 slice (docs/specs/2026-08-19-orbeat-sync-positional-args-design.md):
// runSync, runConnect and project list had the exact same defect as doctor did
// before TestDispatchDoctorRejectsExtraArgs above — flag.Parse only rejects
// unrecognised FLAGS, so a bare positional word silently landed in fs.Args()
// (or, for project list, the raw args slice) and was never inspected. The
// hazard was not hypothetical: `orbeat-sync connect codex` reads like "connect
// codex" but, before this fix, connected every DETECTED tool (Codex, Cursor,
// Gemini CLI, Antigravity, Windsurf), silently writing into all of their
// config files — see TestDispatchConnectRejectsExtraArgs below and its
// companion.
//
// Each rejection test below is paired with a "still works" test for the SAME
// site, because all three rejection tests would also pass on a check that
// rejects EVERYTHING, including `sync --dry-run` — a check that broad would be
// a worse bug than the one being fixed. The companion proves the guard lets a
// real, flagged (or, for project list, argument-less) invocation through to
// its normal codepath.

// TestDispatchSyncRejectsExtraArgs mirrors TestDispatchDoctorRejectsExtraArgs
// for `sync`.
func TestDispatchSyncRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"sync", "bogus"})
	})
	if err == nil {
		t.Fatalf("sync bogus: expected an error, got nil (the argument was silently ignored); stdout=%q", out)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("sync bogus: err = %v, want it to name the unexpected argument", err)
	}
}

// TestDispatchSyncFlaggedInvocationStillWorks is TestDispatchSyncRejectsExtraArgs's
// load-bearing companion: it proves the new fs.NArg() check does not also
// reject a real, flagged `sync` invocation. It asserts the run reaches
// loadValidToken's "not logged in" error — deterministic under an isolated,
// never-logged-in $HOME — rather than merely asserting "some other error",
// which would also pass on a check broad enough to reject --dry-run itself.
func TestDispatchSyncFlaggedInvocationStillWorks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := dispatch(context.Background(), syncclient.Config{}, []string{"sync", "--dry-run", "--json"})
	if err == nil {
		t.Fatal("sync --dry-run --json: expected an error (not logged in), got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("sync --dry-run --json: err = %v, want it to reach the auth stage (\"not logged in\"), proving the extra-args guard let the flagged invocation through", err)
	}
}

// TestDispatchConnectRejectsExtraArgs mirrors TestDispatchDoctorRejectsExtraArgs
// for `connect`.
func TestDispatchConnectRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"connect", "bogus"})
	})
	if err == nil {
		t.Fatalf("connect bogus: expected an error, got nil (the argument was silently ignored); stdout=%q", out)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("connect bogus: err = %v, want it to name the unexpected argument", err)
	}
}

// TestDispatchConnectFlaggedInvocationStillWorks is
// TestDispatchConnectRejectsExtraArgs's load-bearing companion: --dry-run and
// --tools are real, defined connect flags, and the invocation carries zero
// positional arguments, so the extra-args guard must let it through to
// loadValidToken's deterministic "not logged in" error under an isolated,
// never-logged-in $HOME.
func TestDispatchConnectFlaggedInvocationStillWorks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := dispatch(context.Background(), syncclient.Config{}, []string{"connect", "--dry-run", "--tools=codex"})
	if err == nil {
		t.Fatal("connect --dry-run --tools=codex: expected an error (not logged in), got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("connect --dry-run --tools=codex: err = %v, want it to reach the auth stage (\"not logged in\"), proving the extra-args guard let the flagged invocation through", err)
	}
}

// TestDispatchProjectListRejectsExtraArgs mirrors
// TestDispatchDoctorRejectsExtraArgs for `project list`, whose add/remove
// siblings already checked len(args) != 2 but list checked nothing.
func TestDispatchProjectListRejectsExtraArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"project", "list", "bogus"})
	})
	if err == nil {
		t.Fatalf("project list bogus: expected an error, got nil (the argument was silently ignored); stdout=%q", out)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("project list bogus: err = %v, want it to name the unexpected argument", err)
	}
}

// TestDispatchProjectListFlaggedInvocationStillWorks is
// TestDispatchProjectListRejectsExtraArgs's load-bearing companion. `project
// list` takes no flags at all, so its "still works" proof is the plain,
// argument-less invocation actually listing what was registered — a full
// round trip through `project add` then `project list`, both offline.
func TestDispatchProjectListFlaggedInvocationStillWorks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()

	if err := runProject([]string{"add", proj}); err != nil {
		t.Fatalf("project add %s: %v", proj, err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"project", "list"})
	})
	if err != nil {
		t.Fatalf("project list: err = %v, want nil", err)
	}
	if !strings.Contains(out, proj) {
		t.Fatalf("project list: want output to contain the registered project %q, got:\n%s", proj, out)
	}
}

// TestDeploymentStatusMessageBranches is the disclosure gate for Task 8: the
// four states `orbeat-sync status` can find install.json in must produce four
// DIFFERENT sentences, and each must be true of the state it describes.
//
// The branch order is the assertion that matters. Folding the unparseable case
// into the absent case (the likely refactor: one `if installID == ""` covering
// both) makes status tell a developer that nothing has ever been reported
// about her machine while rows sit on the server under an id she can no longer
// read. That is the one wrong answer this function can give, so it is checked
// by asserting the parse branch does NOT carry the absent branch's sentence.
//
// A single "the string is non-empty" assertion would pass on a constant, which
// is why every branch names something only it can say, and why the four
// results are compared against each other rather than only against literals.
func TestDeploymentStatusMessageBranches(t *testing.T) {
	const (
		api  = "https://orbeat.example.com"
		path = "/home/alice/.config/orbeat/install.json"
		id   = "6f3c1c2a-0d5e-4a71-9b3f-2c8e5d1a4f60"
	)
	reported := deploymentStatusMessage(api, path, id, nil)
	never := deploymentStatusMessage(api, path, "", nil)
	broken := deploymentStatusMessage(api, path, "", errors.New("carries \"nope\", which is not a uuid"))
	nowhere := deploymentStatusMessage(api, "", "", errors.New("install path: no home directory"))

	if !strings.Contains(reported, id) || !strings.Contains(reported, api) {
		t.Fatalf("a machine that has reported must name its install id and the API it reported to, got %q", reported)
	}
	if strings.Contains(never, id) {
		t.Fatalf("a machine that has never reported has no install id to print, got %q", never)
	}
	if !strings.Contains(never, "no report has ever been filed") {
		t.Fatalf("the absent case must say no report has ever been filed, got %q", never)
	}
	// The load-bearing one: an unreadable id is NOT the same fact as no id.
	if strings.Contains(broken, "no report has ever been filed") {
		t.Fatalf("an unparseable install.json must not be reported as \"nothing has ever been reported\": "+
			"rows may exist under an id this machine can no longer read; got %q", broken)
	}
	if !strings.Contains(broken, "doctor") {
		t.Fatalf("an unparseable install.json must send the user to doctor, got %q", broken)
	}
	if !strings.Contains(nowhere, "could not be worked out") {
		t.Fatalf("an unresolvable install path must say so rather than claim nothing is reported, got %q", nowhere)
	}
	// Every branch says what is NOT sent, in every state: the disclosure is
	// about the wire format, which does not change with the local file.
	for name, msg := range map[string]string{"reported": reported, "never": never} {
		if !strings.Contains(msg, "no project names") {
			t.Errorf("the %s branch must name what is not collected, got %q", name, msg)
		}
		// The destination belongs in BOTH: a developer who has never reported
		// is the one most in need of knowing where the next report would go.
		if !strings.Contains(msg, api) {
			t.Errorf("the %s branch must name the API the next sync would report to, got %q", name, msg)
		}
	}
	seen := map[string]string{}
	for name, msg := range map[string]string{"reported": reported, "never": never, "broken": broken, "nowhere": nowhere} {
		if prev, dup := seen[msg]; dup {
			t.Fatalf("branches %q and %q produce the same sentence %q", prev, name, msg)
		}
		seen[msg] = name
	}
}

// TestStatusPrintsDeploymentDisclosureEvenWhenLoggedOut pins the WIRING, which
// TestDeploymentStatusMessageBranches cannot: a runStatus that never calls
// deploymentStatusMessage leaves that test green and ships a client that
// discloses nothing.
//
// Logged out is the deliberate fixture. runStatus used to return early on the
// "Not logged in." branch, and restoring that early return is the mutant this
// fails on: `logout` deletes credentials.json and touches neither install.json
// nor the rows already on the server, so the machine whose owner is most
// likely to be asking what orbeat holds is exactly the one that would print
// nothing.
func TestStatusPrintsDeploymentDisclosureEvenWhenLoggedOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := syncclient.Config{APIBaseURL: "https://orbeat.example.com"}

	out := captureStdout(t, func() {
		if err := runStatus(cfg); err != nil {
			t.Errorf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, "Not logged in.") {
		t.Fatalf("no credentials.json must still report the token state, got %q", out)
	}
	if !strings.Contains(out, "Deployment reporting:") {
		t.Fatalf("status must disclose deployment reporting even when logged out, got %q", out)
	}

	// And with an install identity on disk, status names it: the developer can
	// read the id the server groups her machine's rows under.
	const id = "6f3c1c2a-0d5e-4a71-9b3f-2c8e5d1a4f60"
	ipath := filepath.Join(home, ".config", "orbeat", "install.json")
	if err := os.MkdirAll(filepath.Dir(ipath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ipath, []byte(`{"installId":"`+id+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		if err := runStatus(cfg); err != nil {
			t.Errorf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, id) {
		t.Fatalf("status must name this machine's install id, got %q", out)
	}
	if !strings.Contains(out, cfg.APIBaseURL) {
		t.Fatalf("status must name the API this machine reports to, got %q", out)
	}
}

// TestPinListPointsAtTheSignalThatAlreadyExists: 'pin list' reads pins.json
// offline and cannot know whether the server honours any of it, so against a
// Community server it prints entries that do nothing. The footer does not
// recompute that fact, which would cost the command its offline property; it
// names the run that already reports it per pin.
//
// The assertion is on the pointer, not on wording: the output must name
// 'orbeat-sync sync' and must not claim the pins are in effect.
func TestPinListPointsAtTheSignalThatAlreadyExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pinsPath, err := syncclient.DefaultPinsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: "a1", Type: "skill", Name: "brief", Revision: 3}); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() { runErr = runPinList() })
	if runErr != nil {
		t.Fatalf("pin list: %v", runErr)
	}
	if !strings.Contains(out, "skill/brief -> revision 3") {
		t.Fatalf("the pin itself must still be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "orbeat-sync sync") {
		t.Errorf("a pin may be inert against a server that does not support pinning, and 'sync' is the only thing that reports that; the listing must point at it, got:\n%s", out)
	}
}

// TestPinListPrintsNoFooterWhenThereAreNoPins: the footer answers a question
// only a listed pin raises, so on an empty file it is noise on a line that is
// already the whole answer. Asserted as an exact match, so any footer at all
// fails.
func TestPinListPrintsNoFooterWhenThereAreNoPins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var runErr error
	out := captureStdout(t, func() { runErr = runPinList() })
	if runErr != nil {
		t.Fatalf("pin list: %v", runErr)
	}
	if out != "No pins set.\n" {
		t.Errorf("nothing is listed, so there is nothing to qualify: want %q, got %q", "No pins set.\n", out)
	}
}

// TestDoctorStillDiagnosesWithAnUnparseableProjectsFile is the CLI half, and
// it is the one that matters: the defect was in runDoctor, not in Diagnose.
//
// runDoctor called LoadProjects and returned its error, so a JSON syntax error
// in one file made `orbeat-sync doctor` exit non-zero having reported nothing
// at all — not the sync root, not the ledgers, not the pins. A diagnostic tool
// is least useful exactly when it is most needed.
//
// The assertion is that doctor SUCCEEDS and reports, not merely that it names
// the file: an error return with the right message in it would still be a tool
// that diagnosed nothing.
func TestDoctorStillDiagnosesWithAnUnparseableProjectsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(home, ".config", "orbeat", "projects.json")
	if err := os.MkdirAll(filepath.Dir(pj), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pj, []byte("{ not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{}, []string{"doctor"})
	})
	if err != nil {
		t.Fatalf(`doctor returned an error instead of diagnosing: %v

An unparseable projects.json used to abort runDoctor before Diagnose ran, so the tool
reported nothing about anything. It must now report the file as a problem and carry on
with the rest of the diagnosis.`, err)
	}
	if !strings.Contains(out, "projects-file:") {
		t.Fatalf("doctor did not report the projects-file check; got:\n%s", out)
	}
	// And it must still have run the other checks, which is the whole point.
	if !strings.Contains(out, "auth:") {
		t.Fatalf(`doctor reported projects-file but not the unconditional auth check, so it
did not carry on with the rest of the diagnosis; got:\n%s`, out)
	}
}
