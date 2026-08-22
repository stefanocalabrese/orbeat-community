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
	"path/filepath"
	"strings"
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
// checks its own argument count directly. Either way the user gets a hard
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
		if err := noExtraArgs("login", args[1:]); err != nil {
			return err
		}
		return runLogin(ctx, cfg)
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
		return runStatus()
	case "project":
		return runProject(args[1:])
	case "connect":
		return runConnect(ctx, cfg, args[1:])
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
	fmt.Fprintln(os.Stderr, "usage: orbeat-sync <login|sync|logout|status|project|connect|doctor|--version>")
	fmt.Fprintln(os.Stderr, "       orbeat-sync project <add|remove|list> [path]")
	fmt.Fprintln(os.Stderr, "       orbeat-sync connect [--tools=…] [--exclude=…] [--dry-run] [--remove]")
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

func runLogin(ctx context.Context, cfg syncclient.Config) error {
	meta, err := syncclient.Discover(ctx, httpClient, cfg.OIDCDiscovery)
	if err != nil {
		return err
	}
	tok, err := authenticator(cfg).Login(ctx, meta, os.Stdout)
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

func runSync(ctx context.Context, cfg syncclient.Config, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit one JSON object describing the run on stdout")
	dryRun := fs.Bool("dry-run", false, "report what a sync would change without writing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync takes no positional arguments, got %q — only --json and --dry-run are defined", fs.Arg(0))
	}

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
	arts, err := syncclient.FetchArtifacts(ctx, httpClient, cfg.APIBaseURL, tok.AccessToken)
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
	o, err := reconcileAll(claudeDir, projects, arts, p)
	o.ExitCode = exitCode(err)
	if err != nil && o.ExitCode == 2 {
		msg := err.Error()
		o.Fatal = &msg
	}
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
// returns the outcome plus an error carrying the sync exit contract.
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
func reconcileAll(claudeDir string, projects []string, arts []syncclient.Artifact, p plans) (*syncOutcome, error) {
	o := &syncOutcome{DryRun: p.dryRun}
	hadFailure := false

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
		return o, &exitError{code: 2, err: err}
	}
	if len(res.Failures) > 0 {
		hadFailure = true
	}

	seedRes, err := syncclient.ReconcileSeeds(claudeDir, projects, arts, p.seeds)
	o.Seeds = &blocksSection{
		Written: seedRes.Written, Unchanged: seedRes.Unchanged, Stripped: seedRes.Stripped,
		Warnings: strs(seedRes.Warnings), Failures: strs(seedRes.Failures),
		Changes: sectionChanges(p.seeds),
	}
	if err != nil {
		return o, &exitError{code: 2, err: err}
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
		return o, &exitError{code: 2, err: err}
	}
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
		return o, &exitError{code: 1, err: errors.New("some artifacts failed to sync (see the failures above); re-run when the condition clears")}
	}
	return o, nil
}

// restartHintNeeded reports whether the file reconciler actually changed the
// agent set on disk. Unchanged files don't count — before change detection
// (S7) every steady-state run reported "N updated" and fired the restart hint
// every single time.
func restartHintNeeded(res syncclient.ReconcileResult) bool {
	return res.Added > 0 || res.Updated > 0 || res.Removed > 0
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

func runStatus() error {
	path, err := syncclient.DefaultTokenPath()
	if err != nil {
		return err
	}
	tok, err := syncclient.LoadToken(path)
	if err != nil {
		fmt.Println("Not logged in.")
		return nil
	}
	fmt.Println(statusMessage(tok))
	return nil
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
		if len(args) != 2 {
			return fmt.Errorf("usage: orbeat-sync project add <path>")
		}
		// add mutates projects.json (writeFileAtomic) just like remove does —
		// unlocked, it would race a concurrent run's stale-temp cleanup and
		// last-write-wins the projects list.
		release, err := acquireRunLock()
		if err != nil {
			return err
		}
		defer release()
		abs, err := syncclient.AddProject(pj, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Registered project %s. Run 'orbeat-sync sync' to seed it.\n", abs)
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
		abs, found, err := syncclient.RemoveProject(pj, args[1])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("project %s is not registered", abs)
		}
		// Remove = stop managing: strip governed blocks, keep agent notes.
		if _, statErr := os.Stat(abs); statErr == nil {
			cd, err := claudeDir()
			if err != nil {
				return err
			}
			n, err := syncclient.StripProjectSeeds(cd, abs)
			if err != nil {
				return err
			}
			rn, err := syncclient.StripProjectRules(cd, abs)
			if err != nil {
				return err
			}
			fmt.Printf("Unregistered %s (stripped %d seed + %d rule block(s); dev content preserved).\n", abs, n, rn)
		} else {
			// S5: the path is unreachable (deleted OR an unmounted volume — os.Stat
			// cannot tell them apart). Do NOT claim "nothing to strip": if the volume
			// remounts, any governed blocks are still there, and the manifest ledger
			// deliberately keeps their entries so the next reachable sync strips them.
			fmt.Printf("Unregistered %s (path unreachable; any governed blocks there remain and will be stripped when the path is reachable and a sync runs).\n", abs)
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
			fmt.Println(p)
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
	projects, err := syncclient.LoadProjects(pj)
	if err != nil {
		return err
	}

	rep := syncclient.Diagnose(cd, projects)
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
