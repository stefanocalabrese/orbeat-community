// Command orbeat-sync authenticates a user (OAuth device flow) and syncs their
// role-entitled orbeat artifacts into the local ~/.claude tree.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

// exitError carries the process exit code the sync contract requires:
//
//	0 — everything synced.
//	1 — partial: some units failed non-fatally (retry when the condition clears).
//	2 — fatal: integrity/security (corrupt manifest, path escape, unsafe name).
//	    The sync stopped early; do NOT retry — investigate the managed state.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCode maps a command error to a process exit code: nil -> 0, an exitError
// -> its code, a fatal syncclient error (e.g. a corrupt-manifest abort surfaced
// by `project remove`, which is not wrapped in an exitError) -> 2, any other
// error -> 1 (a general failure, e.g. auth or fetch).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if syncclient.IsFatal(err) {
		return 2 // integrity/security abort from a path that doesn't build an exitError
	}
	return 1
}

// errUsage signals a top-level usage error (missing or unknown subcommand): main
// prints usage() and exits 2. It is distinct from a subcommand's own errors,
// which print via the "orbeat-sync: <err>" path and use exitCode.
var errUsage = errors.New("usage")

func main() {
	err := dispatch(context.Background(), syncclient.LoadConfig(), os.Args[1:])
	if err == nil {
		return
	}
	if errors.Is(err, errUsage) {
		usage()
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "orbeat-sync: %v\n", err)
	os.Exit(exitCode(err))
}

// dispatch routes a command line (os.Args[1:]) to its handler. No subcommand
// silently ignores an argument it does not understand, which is the property
// that matters here: v1.18.0 closed a trap where `orbeat-sync sync --dry-run`
// was accepted and silently performed a REAL sync.
//
// The mechanism is NOT flag.Parse — it only rejects unrecognised FLAGS, so a
// bare positional word (e.g. "sync bogus") lands untouched in fs.Args() and
// would otherwise be silently ignored. login/logout/status take no flags at
// all, so any leftover argument is rejected by noExtraArgs. sync, connect and
// doctor each parse their own flag.FlagSet and then check fs.NArg() explicitly
// (mirroring runDoctor, the original of the three — see its doc comment).
// project takes no flags of its own; each of its subcommands (add/remove/list)
// checks its own argument count directly. pin mirrors project's shape for its
// remove/list subcommands, and runSync's own flag.FlagSet handling for its
// "pin <type>/<name> [--revision N]" form. Either way the user gets a hard
// error rather than a run that quietly did something other than what was
// asked.
func dispatch(ctx context.Context, cfg syncclient.Config, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "--version":
		// A dedicated check rather than noExtraArgs: --version's own message
		// names itself rather than taking cmd as a parameter.
		if len(args) > 1 {
			return fmt.Errorf("--version takes no arguments, got %q", args[1])
		}
		fmt.Println("orbeat-sync " + version.Version)
		return nil
	case "login":
		return runLogin(ctx, cfg, args[1:])
	case "sync":
		return runSync(ctx, cfg, args[1:])
	case "logout":
		if err := noExtraArgs("logout", args[1:]); err != nil {
			return err
		}
		return runLogout()
	case "status":
		if err := noExtraArgs("status", args[1:]); err != nil {
			return err
		}
		return runStatus(cfg)
	case "project":
		return runProject(args[1:])
	case "connect":
		return runConnect(ctx, cfg, args[1:])
	case "pin":
		return runPin(ctx, cfg, args[1:])
	case "doctor":
		return runDoctor(args[1:])
	default:
		return errUsage
	}
}

// noExtraArgs rejects any leftover argument for a subcommand that accepts none.
// The returned error maps to exit 1 (a general failure) — deliberately NOT the
// fatal exit 2, and NOT errUsage (which would print the full usage banner).
func noExtraArgs(cmd string, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q — this command has no flags", cmd, args[0])
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orbeat-sync <login|sync|logout|status|project|connect|pin|doctor|--version>")
	fmt.Fprintln(os.Stderr, "       orbeat-sync login [--browser]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync project <add|remove|list> [path] [--tag <tag>]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync sync [--json] [--dry-run] [--watch] [--interval <dur>]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync connect [--tools=…] [--exclude=…] [--dry-run] [--remove]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync pin <type>/<name> [--revision N]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync pin remove <type>/<name>")
	fmt.Fprintln(os.Stderr, "       orbeat-sync pin list")
	fmt.Fprintln(os.Stderr, "       orbeat-sync doctor [--json]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync --version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "exit codes:")
	fmt.Fprintln(os.Stderr, "  0  clean   — everything synced")
	fmt.Fprintln(os.Stderr, "  1  partial — some units failed non-fatally; retry when the condition clears")
	fmt.Fprintln(os.Stderr, "  2  fatal   — integrity/security abort; do not retry, investigate")
}

// httpClient is the shared client for every network call this CLI makes.
// http.DefaultClient has NO timeout, so an unreachable or hung server (a
// half-open connection, a load balancer black-holing traffic) would hang
// orbeat-sync forever with no output and no way to know why. The timeout is
// per-request, which is safe for the device-flow poll loop: each poll is a short
// request — Keycloak answers `authorization_pending` immediately rather than
// long-polling — so the loop's overall duration is bounded by the device code's
// own expiry, not by this.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// acquireRunLock serializes mutating runs (sync / project remove / connect)
// via an exclusive lock on the orbeat config dir. On contention the returned
// error is syncclient.ErrLocked ("another orbeat-sync is running"), which maps
// to exit 1 — retryable, deliberately NOT the fatal exit 2.
func acquireRunLock() (release func(), err error) {
	tokPath, err := syncclient.DefaultTokenPath()
	if err != nil {
		return nil, err
	}
	return syncclient.AcquireLock(filepath.Dir(tokPath))
}

func authenticator(cfg syncclient.Config) *syncclient.Authenticator {
	return &syncclient.Authenticator{HTTPClient: httpClient, ClientID: cfg.ClientID, Sleep: syncclient.CtxSleep}
}

func runLogin(ctx context.Context, cfg syncclient.Config, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	browser := fs.Bool("browser", false, "open a browser and capture the redirect on 127.0.0.1 instead of showing a device code")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("login takes no positional arguments, got %q. Only --browser is defined", fs.Arg(0))
	}
	meta, err := syncclient.Discover(ctx, httpClient, cfg.OIDCDiscovery)
	if err != nil {
		return err
	}
	// The device flow stays the DEFAULT: it needs no listener, no redirect
	// registration, and works over SSH and in containers where opening a
	// browser is not a thing that can happen. --browser is for the desktop
	// case, where the device flow asks a person to retype a code the machine
	// could have carried itself.
	login := authenticator(cfg).Login
	if *browser {
		login = func(ctx context.Context, meta syncclient.Metadata, out io.Writer) (syncclient.Token, error) {
			return authenticator(cfg).LoginBrowser(ctx, meta, openBrowser, out)
		}
	}
	tok, err := login(ctx, meta, os.Stdout)
	if err != nil {
		return err
	}
	path, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	if err := syncclient.SaveToken(path, tok); err != nil {
		return err
	}
	fmt.Println("Logged in. Run 'orbeat-sync sync' to fetch your artifacts.")
	return nil
}

// openBrowser launches the platform's default browser. A failure here is NOT
// fatal: LoginBrowser prints the URL instead, which is the same position the
// device flow leaves a user in, so a machine with no browser degrades to
// copy-and-paste rather than to a dead end.
func openBrowser(rawURL string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	return exec.Command(cmd, append(args, rawURL)...).Start()
}

// loadValidToken loads the cached token, refreshing it if expired.
func loadValidToken(ctx context.Context, cfg syncclient.Config, path string) (syncclient.Token, error) {
	tok, err := syncclient.LoadToken(path)
	if err != nil {
		return syncclient.Token{}, fmt.Errorf("not logged in (run 'orbeat-sync login'): %w", err)
	}
	if tok.Valid() {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return syncclient.Token{}, fmt.Errorf("session expired; run 'orbeat-sync login'")
	}
	meta, err := syncclient.Discover(ctx, httpClient, cfg.OIDCDiscovery)
	if err != nil {
		return syncclient.Token{}, err
	}
	refreshed, err := authenticator(cfg).Refresh(ctx, meta.TokenEndpoint, tok.RefreshToken)
	if err != nil {
		// Keep the cause: a refresh can fail because the session really is over
		// OR because the network/IdP is unreachable. Reporting only "session
		// expired" sends the user to re-authenticate when the actual fix may be
		// connectivity — and re-authenticating will fail the same way.
		return syncclient.Token{}, fmt.Errorf("session expired or the identity provider is unreachable; if this persists after checking connectivity, run 'orbeat-sync login': %w", err)
	}
	if err := syncclient.SaveToken(path, refreshed); err != nil {
		// Not fatal for THIS run (the refreshed token is in hand), but with
		// refresh-token rotation the old refresh token is already revoked
		// server-side — a silently lost save costs the user their session on
		// the NEXT run. Say so now, while the cause is still visible.
		fmt.Fprintf(os.Stderr, "orbeat-sync: warning: could not save the refreshed token: %v (the next run may require 'orbeat-sync login')\n", err)
	}
	return refreshed, nil
}

// minWatchInterval floors --interval. A one-second watch would hammer the API
// and, worse, would spend most of its life holding the run lock, which is the
// same lock `project add` and `connect` take: a developer would find those
// commands blocking for no reason they can see.
const minWatchInterval = time.Minute

// defaultWatchInterval is the cadence a watch runs at when --interval is not
// given. Fifteen minutes is chosen against the thing this is racing: the
// gateway already re-reads entitlements at most five minutes after a change
// (sessionMaxAge), so file distribution being a few minutes behind that is not
// the weak link, and a shorter default would multiply requests across every
// developer machine to close a gap nobody is measuring.
const defaultWatchInterval = 15 * time.Minute

func runSync(ctx context.Context, cfg syncclient.Config, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit one JSON object describing the run on stdout")
	dryRun := fs.Bool("dry-run", false, "report what a sync would change without writing")
	watch := fs.Bool("watch", false, "keep running, re-syncing on --interval until interrupted")
	interval := fs.Duration("interval", defaultWatchInterval, "how often --watch re-syncs (minimum 1m)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync takes no positional arguments, got %q. Only --json, --dry-run, --watch and --interval are defined", fs.Arg(0))
	}
	if *interval < minWatchInterval {
		return fmt.Errorf("--interval must be at least %s, got %s", minWatchInterval, *interval)
	}
	if !*watch {
		return syncOnce(ctx, cfg, asJSON, dryRun)
	}
	return watchSync(ctx, cfg, asJSON, dryRun, *interval)
}

// watchSync re-runs a sync until the context ends or a run says not to retry.
//
// THE EXIT CONTRACT ALREADY DECIDES THIS, so there is no second policy here:
// 0 (clean) and 1 (partial, retry-able) both mean "run again later", and 2
// (fatal, do NOT retry) means stop and surface it. A watch that retried a
// tampered manifest or a path escape every fifteen minutes would turn a loud
// security stop into a log nobody reads.
//
// The first sync happens IMMEDIATELY rather than after one interval: a
// developer who starts a watch expects their machine to be current now, and a
// fifteen-minute wait before the first write would look like a hang.
func watchSync(ctx context.Context, cfg syncclient.Config, asJSON, dryRun *bool, interval time.Duration) error {
	// Signal-aware only HERE. Every other subcommand is a single short run
	// where the default signal behaviour (terminate) is the right one; a watch
	// is the only thing that lives long enough for a developer to expect Ctrl-C
	// to end it cleanly rather than to kill it mid-write.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	return watchLoop(ctx, interval, func() error { return syncOnce(ctx, cfg, asJSON, dryRun) })
}

// watchLoop is watchSync with the sync itself passed in, which is the seam its
// tests drive: the question here is WHEN THE LOOP STOPS, and answering it
// through real HTTP and a real filesystem would exercise everything except
// that.
func watchLoop(ctx context.Context, interval time.Duration, run func() error) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		err := run()
		if exitCode(err) == 2 {
			return err
		}
		if err != nil {
			// PRINTED HERE, not left for main() (B22): this branch is the one
			// that deliberately keeps looping instead of returning — exit 1,
			// a partial/retryable failure, per the exit contract's own doc
			// comment on exitError — so main()'s own "orbeat-sync: %v\n"
			// print (which only ever sees what watchSync ultimately RETURNS)
			// will never run for it. Without this, a watch whose session
			// expired, whose API was unreachable, or that was blocked behind
			// another run's lock printed nothing at every interval, forever:
			// syncOnce's own early failures (loadValidToken, FetchArtifacts,
			// acquireRunLock) return before ever reaching
			// renderHuman/renderJSON, so there was no report for the comment
			// below to be describing either. Same format as main()'s print,
			// so a partial failure during --watch reads identically to what
			// a single non-watch invocation would have shown.
			fmt.Fprintf(os.Stderr, "orbeat-sync: %v\n", err)
		}
		select {
		case <-ctx.Done():
			// A signal during the wait is an ordinary stop, not a failure:
			// whatever the last run reported — including a bare error with no
			// rendered summary, printed just above — has already been printed.
			return nil
		case <-t.C:
		}
	}
}

func syncOnce(ctx context.Context, cfg syncclient.Config, asJSON, dryRun *bool) error {
	release, err := acquireRunLock()
	if err != nil {
		return err
	}
	defer release()
	path, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	tok, err := loadValidToken(ctx, cfg, path)
	if err != nil {
		return err
	}

	// HOISTED from inside reportDeployments, where it used to run AFTER the
	// artifact fetch below (see reportDeployments' own doc comment):
	// pinning needs the pinning flag BEFORE that fetch, to decide whether
	// ?pin= belongs on it at all. Still exactly one round trip to this
	// endpoint per run: reportDeployments no longer fetches it a second
	// time, it reads what was learned here via reportInput.
	scfg, scfgErr := syncclient.FetchSyncConfig(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken)

	pinsPath, err := syncclient.DefaultPinsPath()
	if err != nil {
		return err
	}
	pins, err := syncclient.LoadPins(pinsPath)
	if err != nil {
		return err
	}
	// sendPins/forcedReason: a config-fetch failure and an explicit
	// pinning:false both mean the SAME thing to the fetch below, withhold
	// every pin, for two different reasons a developer needs told apart
	// (see pinOutcomeWarning). Sending ?pin= to a server that has not
	// affirmatively advertised support for it is exactly the
	// silently-served-latest failure the capability negotiation exists to
	// prevent.
	sendPins, forcedReason := pins, ""
	switch {
	case scfgErr != nil:
		sendPins, forcedReason = nil, pinOutcomeUnknown
	case !scfg.Pinning:
		sendPins, forcedReason = nil, pinOutcomeUnsupported
	}

	arts, err := syncclient.FetchArtifacts(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken, sendPins)
	if err != nil {
		return err
	}
	claudeDir, err := claudeDir()
	if err != nil {
		return err
	}
	pj, err := syncclient.DefaultProjectsPath()
	if err != nil {
		return err
	}
	projects, err := syncclient.LoadProjects(pj)
	if err != nil {
		return err
	}
	var p plans
	if *dryRun {
		// One Plan per reconciler, deliberately not shared: each Plan's
		// recorded Changes feed exactly one outcome section, and sharing one
		// Plan across reconcilers would make a change unattributable to the
		// section that actually intends it.
		p = plans{dryRun: true, artifacts: &syncclient.Plan{}, seeds: &syncclient.Plan{}, rules: &syncclient.Plan{}}
	}
	o, applied, err := reconcileAll(claudeDir, projects, arts, p)
	o.ExitCode = exitCode(err)
	if err != nil && o.ExitCode == 2 {
		msg := err.Error()
		o.Fatal = &msg
	}
	// Pin outcomes are computed from the fetch above regardless of dry-run or
	// a later fatal abort: they describe what THIS FETCH would apply (or
	// already applied), not a write, so a --dry-run plan and even an aborted
	// run still tell the developer what her held pins are doing. o.Artifacts
	// is never nil here: reconcileAll sets it before checking Reconcile's
	// own error, on every path.
	outcomes := pinOutcomes(pins, arts, forcedReason)
	o.Artifacts.Pins = outcomes
	o.Artifacts.Warnings = append(o.Artifacts.Warnings, pinOutcomeWarnings(outcomes)...)
	// After reconcileAll and before rendering, so the report's outcome is part
	// of the rendered result rather than a line printed beside it. It never
	// touches err: whatever the reconcilers decided about the exit code is
	// what runSync returns (see reportSection).
	o.Report = reportDeployments(ctx, reportInput{
		cfg: cfg, accessToken: tok.AccessToken,
		served: arts, applied: applied,
		exitCode: o.ExitCode, dryRun: p.dryRun,
		scfg: scfg, scfgErr: scfgErr,
		pins: pins,
	})
	if *asJSON {
		if jerr := renderJSON(os.Stdout, o); jerr != nil {
			return jerr
		}
	} else {
		renderHuman(os.Stdout, o)
	}
	return err
}

// plans carries one *syncclient.Plan per reconciler for a `sync --dry-run`.
// They are deliberately three independent Plans, not one shared: a Change
// recorded through a shared Plan couldn't be attributed back to the section
// (artifacts/seeds/rules) whose reconciler actually intended it. The zero
// value (dryRun false, all three plans nil) is the real, non-planning path —
// every existing call site keeps working unchanged.
type plans struct {
	dryRun                  bool
	artifacts, seeds, rules *syncclient.Plan
}

// reconcileAll runs the three reconcilers against one artifact slice and
// returns the outcome, the union of what they APPLIED, and an error carrying
// the sync exit contract.
//
// The applied union is Reconcile's and ReconcileRules' Applied slices and
// nothing else. SeedResult deliberately has none: a governed seed rides its
// subagent artifact rather than being one, so Reconcile has already judged
// that artifact applied-or-not, and a second source for the same id could only
// make the union less truthful (a subagent whose agent file lost an
// unmanaged-name collision still gets its MEMORY.md merged).
//
// It is returned UNFILTERED, including on the paths where it must not be
// reported: a fatal abort means the later reconcilers never ran, so the union
// is incomplete, and a dry run means Applied describes what a real run WOULD
// apply. Both decisions live in reportDeployments alone. Splitting them across
// producer and consumer would put the same rule in two places, where breaking
// either one leaves the other passing.
//
// A reconciler returns an error only when the failure is FATAL, so any error
// here aborts the remaining reconcilers and yields exit 2 — the local managed
// state can no longer be trusted, and the next reconciler would hit the same
// condition. Non-fatal per-unit failures are collected in each result's Failures
// and yield exit 1 on a REAL run: the healthy units synced, some did not. In
// plan mode nothing was ever applied, so a per-unit failure recorded while
// planning can never mean "partially applied" — p.dryRun short-circuits that
// branch below, and a fatal abort (exit 2) still applies either way, since a
// plan that can't even be computed is worth reporting as such.
//
// Sections for reconcilers that never ran are left nil, which is what lets the
// JSON renderer distinguish "aborted before rules" from "rules had nothing to do".
func reconcileAll(claudeDir string, projects []syncclient.Project, arts []syncclient.Artifact, p plans) (*syncOutcome, []syncclient.AppliedArtifact, error) {
	o := &syncOutcome{DryRun: p.dryRun}
	hadFailure := false
	var applied []syncclient.AppliedArtifact

	res, err := syncclient.Reconcile(claudeDir, arts, p.artifacts)
	o.Artifacts = &artifactsSection{
		Handled: res.Handled, Added: res.Added, Updated: res.Updated,
		Unchanged: res.Unchanged, Removed: res.Removed,
		Skipped: strs(res.Skipped), Warnings: strs(res.Warnings), Failures: strs(res.Failures),
		Changes: sectionChanges(p.artifacts),
	}
	if err != nil {
		// The reconcilers' errors already name their subsystem ("reconcile:",
		// "seed:", "rules:"), and the sections above show which stage reached.
		return o, applied, &exitError{code: 2, err: err}
	}
	applied = append(applied, res.Applied...)
	if len(res.Failures) > 0 {
		hadFailure = true
	}

	seedRes, err := syncclient.ReconcileSeeds(claudeDir, syncclient.ProjectPaths(projects), arts, p.seeds)
	o.Seeds = &blocksSection{
		Written: seedRes.Written, Unchanged: seedRes.Unchanged, Stripped: seedRes.Stripped,
		Warnings: strs(seedRes.Warnings), Failures: strs(seedRes.Failures),
		Changes: sectionChanges(p.seeds),
	}
	if err != nil {
		return o, applied, &exitError{code: 2, err: err}
	}
	if len(seedRes.Failures) > 0 {
		hadFailure = true
	}

	rulesRes, err := syncclient.ReconcileRules(claudeDir, projects, arts, p.rules)
	o.Rules = &blocksSection{
		Written: rulesRes.Written, Unchanged: rulesRes.Unchanged, Stripped: rulesRes.Stripped,
		Warnings: strs(rulesRes.Warnings), Failures: strs(rulesRes.Failures),
		Changes: sectionChanges(p.rules),
	}
	if err != nil {
		return o, applied, &exitError{code: 2, err: err}
	}
	applied = append(applied, rulesRes.Applied...)
	if len(rulesRes.Failures) > 0 {
		hadFailure = true
	}

	// Computed only once all three reconcilers have run to completion (no fatal
	// abort), matching the pre-refactor call site exactly: restartHintNeeded was
	// invoked after the third summary printed, never on an aborted run — even
	// though res (the first reconciler's result) can carry Added/Updated>0 on a
	// LATER fatal abort: Reconcile's remove loop resolves every manifest entry
	// (reconcile.go:168-172) AFTER its write loop, and manifest Files are
	// untrusted and unvalidated, so an entry escaping the sync root aborts
	// fatally with files already added. Setting this unconditionally right
	// after the first call would print the hint on that abort path, which the
	// original never did.
	//
	// In plan mode res.Added/Updated/Removed describe intended, not applied,
	// changes — but a real sync's next run would in fact change the agent set
	// on disk, so the hint (framed as an instruction to restart, not a claim
	// about what already happened on THIS run) still belongs on a dry-run
	// report: it tells the user what applying this plan would require.
	o.RestartRequired = restartHintNeeded(res)

	if hadFailure && !p.dryRun {
		return o, applied, &exitError{code: 1, err: errors.New("some artifacts failed to sync (see the failures above); re-run when the condition clears")}
	}
	return o, applied, nil
}

// restartHintNeeded reports whether the file reconciler actually changed the
// agent set on disk. Unchanged files don't count — before change detection
// (S7) every steady-state run reported "N updated" and fired the restart hint
// every single time.
func restartHintNeeded(res syncclient.ReconcileResult) bool {
	return res.Added > 0 || res.Updated > 0 || res.Removed > 0
}

// reportInput is everything the deployment report needs from one sync run.
// It is a struct rather than six positional parameters so the call site names
// each value, which is what makes the two that decide whether anything is sent
// at all (exitCode, dryRun) readable there rather than only here.
type reportInput struct {
	cfg         syncclient.Config
	accessToken string
	// served is the artifact slice the server returned. The report is judged
	// against it and not only against applied: see reportDeployments'
	// condition 4.
	served   []syncclient.Artifact
	applied  []syncclient.AppliedArtifact
	exitCode int
	dryRun   bool
	// pins is the FULL local pin set runSync loaded from pins.json, before
	// any withholding: reportedPinned re-derives whether each one was
	// actually sent this run from scfg.Pinning alone, so this is safe to
	// pass unconditionally rather than the (possibly nil) sendPins runSync
	// computed for the artifact fetch.
	pins []syncclient.Pin
	// scfg/scfgErr are the /v1/sync/config document runSync already fetched
	// (or the error fetching it) BEFORE this run reached this stage: see
	// the ordering-change note on reportDeployments. Threaded rather than
	// re-fetched: FetchSyncConfig runs exactly once per run, in runSync,
	// because its Pinning flag has to be known before the artifact fetch it
	// shapes, and fetching it a second time here would turn one round trip
	// into two.
	scfg    syncclient.SyncConfig
	scfgErr error
}

// reportDeployments files what this run applied with the server and returns
// the section to render: nil when the stage never ran, a section carrying one
// Warning when it ran and did not arrive, a section carrying the server's
// counts when it did.
//
// NOTHING HERE CAN CHANGE THE EXIT CODE. It returns no error, and
// reportSection has no Failures field, so a failed report is a Warning at exit
// 0 by construction rather than by anyone remembering. A run that delivered
// every file it was asked to deliver must not tell a cron loop to re-fetch and
// re-reconcile all of them because a POST returned 502.
//
// FOUR CONDITIONS SKIP THE REPORT, and three of them are about the same thing:
// a report is a REPLACE. It deletes every row this install previously reported
// that is not in the body, so an empty applied set says "this machine now
// holds none of them". That is exactly right for a developer whose grants were
// revoked, and exactly wrong for a run that never looked at the disk. The
// server cannot tell the two apart (its handler says so in as many words), so
// the client must not send the ambiguous one.
//
//  1. --dry-run. Nothing was applied: Applied names what a real run WOULD
//     apply, which is a plan and not a fact.
//  2. exit 2. A fatal abort is an integrity or security condition (a corrupt
//     manifest, a path escape, an unsafe artifact name, a sync root that
//     cannot be opened), and reconcileAll stops the remaining reconcilers on
//     any of them. On the corrupt-manifest path Reconcile returns before its
//     write loop runs at all, so the applied set is empty for a reason that
//     has nothing to do with what is on disk.
//  3. The server does not advertise deploymentRegistry. Asked on every run
//     rather than inferred from anything else: a Community build, a server
//     older than this feature, and an operator who turned the knob off since
//     yesterday all answer the same question. THE ASKING ITSELF NO LONGER
//     HAPPENS HERE, though: it moved to runSync, above the artifact fetch,
//     so the SAME document's Pinning flag can shape that fetch's ?pin=
//     before it is sent (see runSync's own ordering-change comment). This
//     stage only reads what runSync already learned (reportInput.scfg/
//     scfgErr); the POST is never sent to a route that may not exist.
//  4. The server served an artifact it did not identify. appendApplied drops
//     an id-less artifact, correctly (there is no key a deployment row could
//     be stored under), which leaves the applied set short by exactly those
//     artifacts, and the replace then deletes their rows: a de-entitlement
//     that did not happen. The two halves of this slice make that combination
//     unreachable, because id ships unconditionally in both editions while the
//     registry flag is Enterprise and opt-in. But "unreachable" is a claim
//     about two other files, and this is the one place where being wrong about
//     it destroys data rather than logging something odd.
//
// An empty applied set with a fully identified served set DOES report, and
// that is not an oversight: it is the only way a revoked grant ever becomes
// visible on the read side.
func reportDeployments(ctx context.Context, in reportInput) *reportSection {
	if in.dryRun || in.exitCode == 2 {
		return nil
	}
	if in.scfgErr != nil {
		// Warned rather than swallowed: "the server said it does not record
		// deployments" and "we could not ask" are different facts, and a
		// client that renders them identically hides a broken deployment from
		// the only person watching this run. This is a SEPARATE warning from
		// whatever runSync attached to the artifacts section over the same
		// scfgErr for withheld pins (pinOutcomeUnknown): two different
		// consequences of one failed fetch, told apart rather than merged
		// into one sentence that would bury either fact.
		return reportWarning("could not ask %s whether it records deployments, so nothing was reported: %v", in.cfg.APIBaseURL, in.scfgErr)
	}
	if !in.scfg.DeploymentRegistry {
		return nil
	}
	if n := unidentifiedServed(in.served); n > 0 {
		return reportWarning("nothing was reported: this server records deployments but did not identify %d of the artifact(s) it served, and a report built from them would read as a de-entitlement", n)
	}
	path, err := syncclient.DefaultInstallPath()
	if err != nil {
		return reportWarning("nothing was reported: %v", err)
	}
	// Created here, on the first report and not at login, so a client that
	// never reports never writes an install identity to disk.
	id, err := syncclient.EnsureInstallID(path)
	if err != nil {
		return reportWarning("nothing was reported: %v", err)
	}
	res, err := syncclient.ReportDeployments(ctx, httpClient, in.cfg.APIBaseURL, in.accessToken, id, in.applied,
		reportedPinned(in.pins, in.served, in.scfg.Pinning))
	if err != nil {
		return reportWarning("the deployment report did not reach %s, so what it holds about this machine is stale until a later sync succeeds: %v", in.cfg.APIBaseURL, err)
	}
	// [] rather than nil: this stage ran and had nothing to warn about, which
	// strs' doc comment explains is a different fact from null.
	return &reportSection{Recorded: res.Recorded, Dropped: res.Dropped, Warnings: []string{}}
}

// reportWarning builds the only shape a report failure may take: zero counts
// and one Warning naming what did not happen. There is deliberately no failure
// variant to build (see reportSection).
func reportWarning(format string, args ...any) *reportSection {
	return &reportSection{Warnings: []string{fmt.Sprintf(format, args...)}}
}

// unidentifiedServed counts artifacts this server sent with no id. It counts
// every served artifact rather than only the applicable types, because an
// id-less artifact is a property of the SERVER: one that does not name what it
// serves cannot be told a version of it.
func unidentifiedServed(arts []syncclient.Artifact) int {
	n := 0
	for _, a := range arts {
		if a.ID == "" {
			n++
		}
	}
	return n
}

func runLogout() error {
	path, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	if err := syncclient.ClearToken(path); err != nil {
		return err
	}
	fmt.Println("Logged out.")
	return nil
}

// runStatus reports local sync state: the cached token, then what this machine
// discloses to the server about itself.
//
// The deployment line prints on BOTH token branches, including "Not logged
// in.", and the early return that used to sit under it is gone. Whether a
// developer currently holds a token has nothing to do with what orbeat-api
// already holds about her machines: logging out deletes credentials.json and
// touches neither install.json nor the rows the server recorded under that
// install id. A disclosure that vanished the moment somebody logged out would
// be missing on exactly the machine whose owner is most likely to be asking.
func runStatus(cfg syncclient.Config) error {
	path, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	if tok, err := syncclient.LoadToken(path); err != nil {
		fmt.Println("Not logged in.")
	} else {
		fmt.Println(statusMessage(tok))
	}
	ipath, iid, ierr := loadInstallForStatus()
	fmt.Println(deploymentStatusMessage(cfg.APIBaseURL, ipath, iid, ierr))
	return nil
}

// loadInstallForStatus resolves and reads ~/.config/orbeat/install.json for
// `status`, returning the path, the id and whatever went wrong. It exists so
// deploymentStatusMessage stays pure and can be tested over all four states
// without a temp HOME.
//
// Offline and read-only, like everything else `status` does: it never calls
// EnsureInstallID, so asking what this machine reports must not be the thing
// that gives it an identity to report under.
func loadInstallForStatus() (path, id string, err error) {
	path, err = syncclient.DefaultInstallPath()
	if err != nil {
		return "", "", err
	}
	id, err = syncclient.LoadInstallID(path)
	return path, id, err
}

// deploymentStatusMessage is `orbeat-sync status`'s disclosure of the
// deployment registry to the developer whose machine is being recorded
// (docs/specs/2026-08-22-orbeat-artifact-deployment-registry-design.md
// sec 8.5): covert collection and disclosed collection are different products,
// and the difference costs one line.
//
// IT NEVER CLAIMS REPORTING IS CURRENTLY ON, because `status` cannot know
// that and will not guess. Whether a report is sent is the server's answer to
// GET /v1/sync/config, asked on every sync (reportDeployments condition 3), and
// an operator who unset ORBEAT_DEPLOYMENT_REGISTRY yesterday leaves this
// machine's install.json exactly where it was. What the local state does prove
// is bounded and worth saying: install.json is written only by
// EnsureInstallID, called only from reportDeployments after a server
// advertised the registry, so the file's presence means at least one report
// was built and its absence means none ever was.
//
// The install id and the destination are printed as two separate facts on
// purpose. install.json is per MACHINE, not per server: point ORBEAT_API_URL
// at a different orbeat and the same id is reused, so "this machine has
// reported to <the URL configured right now>" would be a claim the file cannot
// support. What the file says is that a report was filed; what the config says
// is where the next one goes.
//
// The parse failure gets its own branch rather than being folded into
// "absent". Absent means nothing has been reported; unparseable means the id
// this machine reports under is unreadable, and treating one as the other
// would tell a developer nothing is recorded when rows may well exist under an
// id she can no longer see. `doctor` calls the same file a PROBLEM.
func deploymentStatusMessage(apiBaseURL, installPath, installID string, loadErr error) string {
	collected := fmt.Sprintf("The next sync asks %s whether it records deployments and, if it does, "+
		"sends which artifacts at which revision this machine applied. Nothing else: no file paths, "+
		"no project names, no hostname, no username.", apiBaseURL)
	switch {
	case installPath == "":
		return fmt.Sprintf("Deployment reporting: cannot say, because the location of install.json "+
			"could not be worked out (%v).", loadErr)
	case loadErr != nil:
		return fmt.Sprintf("Deployment reporting: %s exists but will not parse (%v). Run 'orbeat-sync doctor'.",
			installPath, loadErr)
	case installID == "":
		return fmt.Sprintf("Deployment reporting: no report has ever been filed from this machine "+
			"(there is no %s). %s", installPath, collected)
	default:
		return fmt.Sprintf("Deployment reporting: this machine has filed at least one report, as "+
			"install %s (%s). %s", installID, installPath, collected)
	}
}

// statusMessage renders the human status line for a loaded token. It branches on
// the refresh token: an expired access token with NO refresh token cannot be
// refreshed by a sync, so promising "a sync will refresh it" would be a lie —
// the user must re-run login.
func statusMessage(tok syncclient.Token) string {
	switch {
	case tok.Valid():
		return fmt.Sprintf("Logged in (token valid until %s).", tok.Expiry.Local().Format(time.RFC3339))
	case tok.RefreshToken == "":
		return "Logged in but the access token is expired and there is no refresh token — run 'orbeat-sync login'."
	default:
		return "Logged in but the access token is expired (a sync will refresh it)."
	}
}

// parseProjectAdd reads `<path> [--tag <tag>]...` and nothing else.
//
// Hand-parsed rather than given a flag.FlagSet because the flag package stops
// at the first non-flag argument, so `project add ~/x --tag go` would silently
// ignore the flag with the path first, and demanding `--tag go ~/x` would be a
// worse command line than the feature is worth. Repeatable flags need a custom
// Value type either way.
//
// An unrecognised argument is an ERROR rather than something ignored: the
// stray-positional work in this release exists because `orbeat-sync connect
// codex` silently connected every tool, and a typo'd `--tags go` here would
// otherwise register a project whose targeting silently matches nothing.
func parseProjectAdd(args []string) (string, []string, error) {
	const usage = "usage: orbeat-sync project add <path> [--tag <tag>]..."
	var path string
	var tags []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--tag":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--tag needs a value\n%s", usage)
			}
			tags = append(tags, args[i+1])
			i++
		case strings.HasPrefix(args[i], "--tag="):
			tags = append(tags, strings.TrimPrefix(args[i], "--tag="))
		case strings.HasPrefix(args[i], "-"):
			return "", nil, fmt.Errorf("unknown flag %q\n%s", args[i], usage)
		case path == "":
			path = args[i]
		default:
			return "", nil, fmt.Errorf("project add takes one path, got %q as well\n%s", args[i], usage)
		}
	}
	if path == "" {
		return "", nil, fmt.Errorf("%s", usage)
	}
	return path, tags, nil
}

// runProject manages the registered-projects list for project-scope seeds.
func runProject(args []string) error {
	pj, err := syncclient.DefaultProjectsPath()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: orbeat-sync project <add|remove|list> [path]")
	}
	switch args[0] {
	case "add":
		path, tags, err := parseProjectAdd(args[1:])
		if err != nil {
			return err
		}
		// add mutates projects.json (writeFileAtomic) just like remove does —
		// unlocked, it would race a concurrent run's stale-temp cleanup and
		// last-write-wins the projects list.
		release, err := acquireRunLock()
		if err != nil {
			return err
		}
		defer release()
		abs, err := syncclient.AddProject(pj, path, tags)
		if err != nil {
			return err
		}
		if len(tags) > 0 {
			fmt.Printf("Registered project %s [%s]. Run 'orbeat-sync sync' to seed it.\n", abs, strings.Join(tags, " "))
		} else {
			fmt.Printf("Registered project %s. Run 'orbeat-sync sync' to seed it.\n", abs)
		}
		return nil
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: orbeat-sync project remove <path>")
		}
		release, err := acquireRunLock()
		if err != nil {
			return err
		}
		defer release()
		abs, found, err := syncclient.ResolveRegisteredProject(pj, args[1])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("project %s is not registered", abs)
		}
		// Remove = stop managing: strip governed blocks, keep agent notes.
		//
		// STRIP BEFORE DE-REGISTERING (B24), never the reverse: once a project
		// drops out of the registered set, its Rules/Seeds ledger entries stop
		// being trusted (trustedSeedBoundary and its rules-side counterpart,
		// B23) and no future ordinary sync will ever reach them again — there
		// is no self-heal to fall back on if this strip fails partway. Every
		// early `return err` below therefore happens BEFORE
		// syncclient.RemoveProject ever runs, so a failed strip leaves the
		// project registered (and thus still reachable by a retry, or by an
		// ordinary sync) rather than de-registered with its blocks stranded.
		var n, rn int
		reachable := false
		if _, statErr := os.Stat(abs); statErr == nil {
			reachable = true
			cd, err := claudeDir()
			if err != nil {
				return err
			}
			n, err = syncclient.StripProjectSeeds(cd, abs)
			if err != nil {
				return err
			}
			rn, err = syncclient.StripProjectRules(cd, abs)
			if err != nil {
				return err
			}
		}
		if _, _, err := syncclient.RemoveProject(pj, args[1]); err != nil {
			return err
		}
		if reachable {
			fmt.Printf("Unregistered %s (stripped %d seed + %d rule block(s); dev content preserved).\n", abs, n, rn)
		} else {
			// S5: the path is unreachable (deleted OR an unmounted volume — os.Stat
			// cannot tell them apart). Do NOT claim "nothing to strip": if the volume
			// remounts, any governed blocks are still there — but this project is
			// now de-registered, so (B23) they are no longer reachable by an
			// ordinary sync either. Re-registering it (and then removing it again
			// once reachable) is the only path left to strip them.
			fmt.Printf("Unregistered %s (path unreachable; if any governed blocks remain there they are now unmanaged — reconnect the path, then 'orbeat-sync project add %s' and 'orbeat-sync project remove %s' again to strip them).\n", abs, abs, abs)
		}
		return nil
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("project list takes no arguments, got %q", args[1])
		}
		list, err := syncclient.LoadProjects(pj)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No projects registered. Add one with 'orbeat-sync project add <path>'.")
			return nil
		}
		for _, p := range list {
			if len(p.Tags) > 0 {
				fmt.Printf("%s [%s]\n", p.Path, strings.Join(p.Tags, " "))
			} else {
				fmt.Println(p.Path)
			}
		}
		return nil
	default:
		return fmt.Errorf("usage: orbeat-sync project <add|remove|list> [path]")
	}
}

func claudeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// runDoctor is an offline, read-only diagnosis of local sync state — it takes
// no lock and touches no network, mirroring the line runStatus already draws
// (docs/orbeat-sync-guide.md: "never talks to the network"), because doctor
// has to work on the machine where things are broken. It cannot use
// noExtraArgs the way login/logout/status do: it takes --json. Any leftover
// positional argument after flag parsing (e.g. `doctor bogus`) is rejected
// explicitly via fs.NArg() instead — flag.Parse only rejects unrecognized
// FLAGS, so a bare word is otherwise accepted and silently ignored, exactly
// the trap dispatch's own doc comment says no subcommand should set (S9).
//
// The sync root and registered projects are resolved exactly as runSync
// resolves them (claudeDir + DefaultProjectsPath/LoadProjects), reusing the
// same helpers rather than re-deriving the paths.
//
// The returned error is non-nil ONLY when doctor itself could not run (e.g.
// DefaultProjectsPath failing, or a projects.json that will not parse) — see
// spec §6. Whatever Diagnose finds, including problems, is reported and
// returns nil: a diagnosis that reports a problem is working, not failing,
// and this repo's cron recipe (docs/orbeat-sync-guide.md) branches on exit
// codes, so a non-zero exit here would abort automation over a benign
// finding. Machine consumers branch on --json instead.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit one JSON object describing the diagnosis on stdout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("doctor takes no positional arguments, got %q — only --json is defined", fs.Arg(0))
	}

	cd, err := claudeDir()
	if err != nil {
		return err
	}
	pj, err := syncclient.DefaultProjectsPath()
	if err != nil {
		return err
	}
	// A load failure does NOT abort the diagnosis. It used to: doctor returned
	// this error and reported nothing at all, so a JSON syntax error in one
	// file meant the tool could say nothing about the sync root, the ledgers,
	// the pins or anything else. That is the wrong shape for a diagnostic,
	// which should be most useful exactly when something is broken.
	//
	// Diagnose runs with an empty project list instead, and checkProjectsFile
	// reports the failure as a PROBLEM naming the parse error, so the other
	// checks' silence is explained rather than mistaken for health.
	projects, loadErr := syncclient.LoadProjects(pj)
	if loadErr != nil {
		projects = nil
	}
	// Resolved here, beside the other two paths, and a resolution failure is
	// returned rather than swallowed: doctor cannot honestly report on a file
	// whose location it could not work out.
	ip, err := syncclient.DefaultInstallPath()
	if err != nil {
		return err
	}
	pp, err := syncclient.DefaultPinsPath()
	if err != nil {
		return err
	}

	rep := syncclient.Diagnose(cd, syncclient.ProjectPaths(projects), ip, pp, pj)
	if *asJSON {
		return renderDoctorJSON(os.Stdout, rep)
	}
	renderDoctorHuman(os.Stdout, rep)
	return nil
}

// doctorJSON is the --json envelope for `doctor`. Findings is normalised to
// [] rather than null when there is nothing to report — the same convention
// outcome.go's strs applies to warnings/failures. This schema has no
// competing meaning reserved for null (unlike syncOutcome's sections, where
// null means "never ran"), but a consumer still shouldn't need a nil check
// just to range over an empty result.
type doctorJSON struct {
	Findings []syncclient.Finding `json:"findings"`
	Problems int                  `json:"problems"`
}

func renderDoctorJSON(w io.Writer, rep syncclient.Report) error {
	findings := rep.Findings
	if findings == nil {
		findings = []syncclient.Finding{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doctorJSON{Findings: findings, Problems: rep.Problems()})
}

// renderDoctorHuman groups findings by check and ends with a problem/note
// count, prefixed "Clean — " when there are zero problems. Problems and notes
// are labelled distinctly, on purpose: a preserved ledger entry is a NOTE —
// correct, deliberate behaviour a user should know about, not something to
// fix — and rendering it the same as a malformed marker (a PROBLEM) would
// send someone to "fix" state that is protecting them (spec §4, check 3;
// doctor.go's Severity doc comment).
//
// There is no longer an all-findings-empty early return. Diagnose always
// appends one CheckAuth note (deferring to 'orbeat-sync status') on every
// call, so rep.Findings can never actually be empty — that branch became
// permanently unreachable the day the auth note was added, and unreachable
// code is worse than no code (see internal/syncclient/doctor.go's CheckAuth
// and checkAuth doc comments). "Clean" is redefined to mean zero PROBLEMS,
// not zero output: the note is always shown, in the normal per-check listing
// like any other finding, never suppressed or special-cased into the summary
// line — it exists precisely so the user whose real problem is auth sees it,
// and hiding it on the one path most likely to be read (a run that otherwise
// found nothing) would defeat that.
func renderDoctorHuman(w io.Writer, rep syncclient.Report) {
	var order []syncclient.Check
	byCheck := map[syncclient.Check][]syncclient.Finding{}
	for _, f := range rep.Findings {
		if _, seen := byCheck[f.Check]; !seen {
			order = append(order, f.Check)
		}
		byCheck[f.Check] = append(byCheck[f.Check], f)
	}

	for _, check := range order {
		fmt.Fprintf(w, "%s:\n", check)
		for _, f := range byCheck[check] {
			label := "note"
			if f.Severity == syncclient.SeverityProblem {
				label = "PROBLEM"
			}
			if f.Path != "" {
				fmt.Fprintf(w, "  [%s] %s\n", label, f.Path)
			} else {
				// The auth note concerns no specific file — an empty Path would
				// otherwise print as "  [note] " with a dangling trailing space.
				fmt.Fprintf(w, "  [%s]\n", label)
			}
			fmt.Fprintf(w, "        %s\n", f.Detail)
			fmt.Fprintf(w, "        remedy: %s\n", f.Remedy)
		}
	}

	problems := rep.Problems()
	summary := fmt.Sprintf("%d problem(s), %d note(s).", problems, len(rep.Findings)-problems)
	if problems == 0 {
		summary = "Clean — " + summary
	}
	fmt.Fprintln(w, summary)
}

func runConnect(ctx context.Context, cfg syncclient.Config, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	tools := fs.String("tools", "", "comma-separated tools to target (default: all installed)")
	exclude := fs.String("exclude", "", "comma-separated tools to skip")
	dryRun := fs.Bool("dry-run", false, "print what would change without writing")
	remove := fs.Bool("remove", false, "remove the orbeat-gateway entry instead of writing it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("connect takes no positional arguments, got %q — only --tools, --exclude, --dry-run and --remove are defined", fs.Arg(0))
	}

	release, err := acquireRunLock()
	if err != nil {
		return err
	}
	defer release()

	ledgerPath, err := syncclient.DefaultConnectLedgerPath()
	if err != nil {
		return err
	}
	opts := syncclient.ConnectOptions{
		LedgerPath: ledgerPath,
		Only:       splitList(*tools),
		Exclude:    splitList(*exclude),
		DryRun:     *dryRun,
		Remove:     *remove,
	}

	if !*remove {
		tokPath, err := syncclient.DefaultTokenPath()
		if err != nil {
			return err
		}
		tok, err := loadValidToken(ctx, cfg, tokPath)
		if err != nil {
			return err
		}
		gw, err := syncclient.FetchGatewayURL(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken)
		if err != nil {
			return err
		}
		opts.GatewayURL = gw
	}

	results, err := syncclient.RunConnect(opts)
	return reportConnect(results, *remove, err)
}

// reportConnect prints the per-tool results — including the successful tools'
// one-time auth hints/caveats — and THEN returns err. Printing before surfacing
// the error is load-bearing under S6's isolation semantics: a summary error from
// one failed adapter must not discard the visibility (paths + one-time-auth
// hints) for the tools that DID succeed. The "no tools selected" line is
// suppressed for an early selection error (empty results), where it would be
// misleading — those errors carry their own message on stderr.
func reportConnect(results []syncclient.ToolResult, remove bool, err error) error {
	if err == nil || len(results) > 0 {
		printConnectResults(results, remove)
	}
	return err
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func printConnectResults(results []syncclient.ToolResult, remove bool) {
	if len(results) == 0 {
		fmt.Println("No tools selected (none installed, or filtered out).")
		return
	}
	var hints []string
	for _, r := range results {
		switch {
		case r.Result.Note != "":
			fmt.Printf("  %-12s %s\n", r.Tool, r.Result.Note)
		case r.Result.Changed && remove:
			fmt.Printf("  %-12s removed (%s)\n", r.Tool, r.Result.Path)
		case r.Result.Changed:
			fmt.Printf("  %-12s configured (%s)\n", r.Tool, r.Result.Path)
			if h := r.Adapter.AuthHint(); h != "" {
				hints = append(hints, h)
			}
			if c := r.Adapter.Caveat(); c != "" {
				hints = append(hints, c)
			}
		default:
			fmt.Printf("  %-12s unchanged\n", r.Tool)
		}
	}
	if len(hints) > 0 {
		fmt.Println("\nNext steps (one-time auth per tool):")
		for _, h := range hints {
			fmt.Println("  - " + h)
		}
	}
}

// runPin dispatches the pin subcommands. Unlike project's add/remove/list,
// none of which ever touch the network, 'pin <type>/<name>' by itself IS a
// network command: it resolves the name against a live fetch, so a typo'd
// name or an out-of-range --revision fails now rather than surfacing as a
// silent "pruned" warning at the next sync. 'remove' and 'list' stay
// offline, mirroring project's own remove/list: removing a pin needs
// nothing the server knows (it matches on the LABEL already in pins.json),
// and listing only reads the file.
func runPin(ctx context.Context, cfg syncclient.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orbeat-sync pin <type>/<name> [--revision N] | orbeat-sync pin remove <type>/<name> | orbeat-sync pin list")
	}
	switch args[0] {
	case "list":
		if err := noExtraArgs("pin list", args[1:]); err != nil {
			return err
		}
		return runPinList()
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: orbeat-sync pin remove <type>/<name>")
		}
		return runPinRemove(args[1])
	default:
		return runPinSet(ctx, cfg, args[0], args[1:])
	}
}

// splitPinTarget parses "<type>/<name>", the label both 'pin' and
// 'pin remove' take on the command line.
func splitPinTarget(target string) (typ, name string, err error) {
	typ, name, ok := strings.Cut(target, "/")
	if !ok || typ == "" || name == "" {
		return "", "", fmt.Errorf("pin target must be \"<type>/<name>\", got %q", target)
	}
	return typ, name, nil
}

// runPinSet resolves <type>/<name> against a live fetch and writes (or
// replaces) its pin. It refuses outright against a server that has not
// advertised pinning; the capability check in the body carries the argument
// for why that is a refusal and not a warning.
//
// With no --revision it pins the revision the fetch just served ("stop moving
// this one", the dominant intent) rather than always the latest, so re-running
// it against an artifact already pinned re-affirms the SAME revision instead
// of jumping to head. THAT PROPERTY RESTS ON THE CAPABILITY CHECK, not on the
// discovery fetch carrying the existing pins. The fetch does carry them, but a
// server that never parses ?pin= answers with head anyway and
// syncArtifactDTO.Revision reports head unconditionally, so before the check
// this comment asserted the opposite of what ran: reproduced against a fake
// server at pinning:false, a pin seeded at revision 1 was rewritten to 3 and
// the command printed "Pinned skill/pinme to revision 3" as a success.
//
// With --revision N it is validated against that same response's
// OldestServable/Latest before anything is written; a response that still
// carries no window for this artifact (Latest == 0) leaves N unvalidatable,
// and this refuses to write an unvalidated pin rather than silently accept it.
// The capability check removes the one cause of that which was never about the
// artifact at all, a server edition that reports no window for anything, so
// what the guard still covers is a per-artifact gap.
func runPinSet(ctx context.Context, cfg syncclient.Config, target string, rest []string) error {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	revision := fs.Int("revision", 0, "pin to this revision instead of the one currently served")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("pin takes no positional arguments after <type>/<name>, got %q (only --revision is defined)", fs.Arg(0))
	}
	typ, name, err := splitPinTarget(target)
	if err != nil {
		return err
	}

	// Locked before any network call, like every mutating form (project add's
	// comment records why: an unlocked write to this directory races a
	// concurrent run's stale-temp cleanup and last-write-wins the file).
	release, err := acquireRunLock()
	if err != nil {
		return err
	}
	defer release()

	tokPath, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	tok, err := loadValidToken(ctx, cfg, tokPath)
	if err != nil {
		return err
	}

	// The capability check internal/syncclient/api.go's FetchArtifacts declares
	// a CALLER obligation, met here for the same reason syncOnce meets it and
	// with a harder consequence. GET /v1/sync/artifacts read no ?pin= before
	// that parameter existed and net/http rejects no unknown query parameter,
	// so a server without pinning (internal/api/pinning.community.go returns
	// false, and its own comment says ?pin is ignored rather than rejected on
	// purpose) answers the fetch below with the LATEST revision. Revision is
	// unconditional on that DTO, so found.Revision is head, and the write at
	// the end of this function would move a held pin forward while reporting
	// success.
	//
	// REFUSE, NEVER WARN, and the asymmetry with syncOnce is the whole point.
	// syncOnce can degrade to "serve latest and warn per held pin" because it
	// writes no pin file: the developer's recorded intent survives the run
	// untouched, and the next sync against a capable server honours it again.
	// This command's entire product IS the pins.json entry, so a warning
	// printed before the write leaves her pinned to a revision she never chose,
	// in the file she will read later to find out what she chose. A failed
	// fetch refuses on the same reasoning: "the server did not answer" is not
	// "the server keeps pins", and guessing the permissive half of that is how
	// the write happens anyway.
	scfg, err := syncclient.FetchSyncConfig(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken)
	if err != nil {
		return fmt.Errorf("%w. No pin was written: pinning support could not be confirmed", err)
	}
	if !scfg.Pinning {
		return fmt.Errorf("%s does not support pinning, so no pin was written: it ignores ?pin= and would keep serving the latest revision of %s/%s", cfg.APIBaseURL, typ, name)
	}

	pinsPath, err := syncclient.DefaultPinsPath()
	if err != nil {
		return err
	}
	existing, err := syncclient.LoadPins(pinsPath)
	if err != nil {
		return err
	}
	arts, err := syncclient.FetchArtifacts(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken, existing)
	if err != nil {
		return err
	}
	var found *syncclient.Artifact
	for i := range arts {
		if arts[i].Type == typ && arts[i].Name == name {
			found = &arts[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no entitled artifact named %s/%s", typ, name)
	}
	rev := found.Revision
	if *revision > 0 {
		if found.Latest == 0 {
			return fmt.Errorf("%s did not report a revision window for %s/%s, so --revision cannot be validated (is pinning supported here?)", cfg.APIBaseURL, typ, name)
		}
		if *revision < found.OldestServable || *revision > found.Latest {
			return fmt.Errorf("revision %d is out of range for %s/%s (this server currently serves %d..%d)",
				*revision, typ, name, found.OldestServable, found.Latest)
		}
		rev = *revision
	}
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: found.ID, Type: typ, Name: name, Revision: rev}); err != nil {
		return err
	}
	fmt.Printf("Pinned %s/%s to revision %d. Run 'orbeat-sync sync' to apply it.\n", typ, name, rev)
	return nil
}

// runPinRemove drops the pin labelled type/name, offline: pins.json already
// carries the label, so no fetch is needed to know what to remove.
func runPinRemove(target string) error {
	typ, name, err := splitPinTarget(target)
	if err != nil {
		return err
	}
	release, err := acquireRunLock()
	if err != nil {
		return err
	}
	defer release()
	pinsPath, err := syncclient.DefaultPinsPath()
	if err != nil {
		return err
	}
	found, err := syncclient.RemovePin(pinsPath, typ, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no pin for %s/%s", typ, name)
	}
	fmt.Printf("Removed the pin for %s/%s. Run 'orbeat-sync sync' to apply.\n", typ, name)
	return nil
}

// pinListFooter qualifies the listing above it by naming the signal that
// already exists, rather than computing a second one. Whether a pin is
// honoured is a fact only the server has, and 'pin list' is deliberately
// offline: it shares that property with 'doctor' so it still answers on the
// machine where things are broken and while the token is expired. A fetch
// here would trade that away for information the next 'sync' prints anyway,
// per pin, already telling pinOutcomeUnsupported ("the server said no") apart
// from pinOutcomeUnknown ("could not ask") in pinOutcomeWarning.
//
// The window it covers is narrow on purpose, and naming it is the point of
// this comment. runPinSet refuses to write a pin against a server that has
// not advertised pinning, so an inert pin has only three provenances left: it
// was written before that check shipped, the server changed edition under it,
// or pins.json was hand-edited.
const pinListFooter = "\nWhether the server honours them is not knowable offline. Run 'orbeat-sync sync': it warns for each pin it could not honour, and says whether the server refused pinning or could not be asked."

// runPinList prints every locally held pin, offline. It takes no lock: like
// 'project list', it never writes.
//
// The footer prints only when something was listed. On an empty file the "No
// pins set." line is already the whole answer, and qualifying it would raise
// a question no pin asked.
func runPinList() error {
	pinsPath, err := syncclient.DefaultPinsPath()
	if err != nil {
		return err
	}
	pins, err := syncclient.LoadPins(pinsPath)
	if err != nil {
		return err
	}
	if len(pins) == 0 {
		fmt.Println("No pins set.")
		return nil
	}
	for _, p := range pins {
		fmt.Printf("%s/%s -> revision %d\n", p.Type, p.Name, p.Revision)
	}
	fmt.Println(pinListFooter)
	return nil
}
