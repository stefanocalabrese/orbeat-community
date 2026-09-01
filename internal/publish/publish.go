// Package publish renders the orbeat marketplace from current catalog state and
// reconciles it into a git target: a local path (commit-in-place) or a remote
// URL (clone work-dir + push). Always renders full state — idempotent.
package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// Secrets resolves a credential reference to a token (satisfied by *secrets.Resolver).
type Secrets interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Snapshot returns the current active artifacts to render.
type Snapshot func(ctx context.Context) ([]marketplace.Artifact, error)

// Config parameterises the publisher.
type Config struct {
	GitURL        string        // local path or remote URL ("" disables publishing)
	WorkDir       string        // clone dir for remote targets (defaults to a temp dir)
	CredentialRef string        // SecretsProvider ref for remote push token
	GatewayURL    string        // for the connect plugin
	Timeout       time.Duration // per-publish-run deadline; 0 disables (git ops + secrets resolve are otherwise unbounded)
}

// Publisher renders and reconciles the marketplace into a git target.
type Publisher struct {
	cfg      Config
	secrets  Secrets
	snapshot Snapshot
}

// New creates a Publisher. snap must not be nil.
func New(cfg Config, sec Secrets, snap Snapshot) *Publisher {
	return &Publisher{cfg: cfg, secrets: sec, snapshot: snap}
}

// isRemote returns true when the git target should be treated as a remote
// (clone → commit → push). Remote mode activates when the URL contains a
// scheme/host (https://, git@…) OR when an explicit WorkDir is configured
// (covers local bare-repo testing).
func (p *Publisher) isRemote() bool {
	u := p.cfg.GitURL
	return strings.Contains(u, "://") || strings.HasPrefix(u, "git@") || p.cfg.WorkDir != ""
}

// Result reports the outcome of one publish run.
type Result struct {
	Commit  string // commit now on the target; "" only when publishing is disabled (on success)
	Changed bool   // true when this run committed new content
	// Attempt is 1 for the first run after a mutation and increments for each
	// retry of a run that failed, resetting on success. It is filled in by the
	// Worker, not by PublishOnce, because a single publish knows nothing about
	// the sequence it belongs to. Without it a recovery and a first-try success
	// are indistinguishable in the audit trail.
	Attempt int
}

// PublishOnce renders current state and reconciles the git target, returning
// the resulting Result (see its field docs for what Commit/Changed mean). Any
// error is wrapped in redactedError so credentials embedded in a git URL can
// never reach a consumer unredacted — PublishOnce is the single exported
// chokepoint every production caller (the Worker loop) goes through, so
// redaction lives here rather than at each call site (audit G12).
func (p *Publisher) PublishOnce(ctx context.Context) (Result, error) {
	res, err := p.publishOnce(ctx)
	if err != nil {
		return res, &redactedError{err: err}
	}
	return res, nil
}

// publishOnce is the unwrapped implementation; see PublishOnce.
func (p *Publisher) publishOnce(ctx context.Context) (Result, error) {
	if p.cfg.GitURL == "" {
		return Result{}, nil
	}
	if p.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Timeout)
		defer cancel()
	}
	arts, err := p.snapshot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("publish: snapshot: %w", err)
	}
	files, err := marketplace.RenderMarketplace(marketplace.Options{GatewayURL: p.cfg.GatewayURL}, arts)
	if err != nil {
		return Result{}, fmt.Errorf("publish: render: %w", err)
	}

	repo, wt, dir, err := p.openTarget(ctx)
	if err != nil {
		return Result{}, err
	}

	// Clear the two plugin trees + manifest so deletions propagate, then write fresh.
	for _, sub := range []string{".claude-plugin", "plugins"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil && !os.IsNotExist(err) {
			return Result{}, fmt.Errorf("publish: clear %s: %w", sub, err)
		}
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return Result{}, fmt.Errorf("publish: mkdir: %w", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return Result{}, fmt.Errorf("publish: write %s: %w", rel, err)
		}
	}
	if _, err := wt.Add("."); err != nil {
		return Result{}, fmt.Errorf("publish: git add: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return Result{}, fmt.Errorf("publish: status: %w", err)
	}
	changed := !status.IsClean()
	if changed {
		if _, err := wt.Commit("orbeat: publish marketplace", &git.CommitOptions{
			Author: &object.Signature{Name: "orbeat", Email: "orbeat@localhost", When: time.Now()},
		}); err != nil {
			return Result{}, fmt.Errorf("publish: commit: %w", err)
		}
	}

	// Push unconditionally. Whether the remote holds our state is never inferred
	// from the local tree: a previous push may have failed, leaving a commit here
	// that the remote never saw. Idempotent — push tolerates NoErrAlreadyUpToDate.
	if p.isRemote() {
		if err := p.push(ctx, repo); err != nil {
			return Result{}, err
		}
	}

	head, err := repo.Head()
	if err != nil {
		return Result{}, fmt.Errorf("publish: head: %w", err)
	}
	return Result{Commit: head.Hash().String(), Changed: changed}, nil
}

// openTarget returns the repo + worktree + on-disk dir. Local: open/init in
// place. Remote: open/clone a work-dir and reset it to the remote tip.
func (p *Publisher) openTarget(ctx context.Context) (*git.Repository, *git.Worktree, string, error) {
	if !p.isRemote() {
		dir := p.cfg.GitURL
		repo, err := git.PlainOpen(dir)
		if err != nil {
			repo, err = git.PlainInit(dir, false)
			if err != nil {
				return nil, nil, "", fmt.Errorf("publish: init local repo: %w", err)
			}
		}
		wt, err := repo.Worktree()
		if err != nil {
			return nil, nil, "", fmt.Errorf("publish: worktree: %w", err)
		}
		return repo, wt, dir, nil
	}

	dir := p.cfg.WorkDir
	if dir == "" {
		// Default work-dir is process-wide; safe because a single publisher
		// worker runs per process (see internal/publish Worker). Set Config.WorkDir
		// to isolate concurrent publishers.
		dir = filepath.Join(os.TempDir(), "orbeat-marketplace-work")
	}
	auth, err := p.auth(ctx)
	if err != nil {
		return nil, nil, "", err
	}

	// Try to open an existing work-dir clone first.
	repo, err := git.PlainOpen(dir)
	if err != nil {
		// Not yet cloned; try clone. ONLY an empty remote (its distinctive
		// sentinel error, same discrimination syncToRemoteTip applies to fetch)
		// falls back to init + add-remote; any other clone failure — auth, DNS,
		// transient network — is a real error and must surface as one, not be
		// misdiagnosed as a fresh remote (audit G10: that path proceeds to
		// commit and then fails later with a misleading push error).
		repo, err = git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: p.cfg.GitURL, Auth: auth})
		if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil, nil, "", fmt.Errorf("publish: clone: %w", err)
		}
		if err != nil {
			// Empty remote: init a fresh working repo and wire up the remote.
			if initErr := os.MkdirAll(dir, 0o755); initErr != nil {
				return nil, nil, "", fmt.Errorf("publish: mkdir work dir: %w", initErr)
			}
			repo, err = git.PlainInit(dir, false)
			if err != nil {
				return nil, nil, "", fmt.Errorf("publish: init work repo: %w", err)
			}
			if _, err = repo.CreateRemote(&config.RemoteConfig{
				Name: "origin",
				URLs: []string{p.cfg.GitURL},
			}); err != nil {
				return nil, nil, "", fmt.Errorf("publish: create remote: %w", err)
			}
		}
	} else {
		// Work-dir exists — fetch and hard-reset to the remote tip so the tree
		// mirrors the remote before we render. A fetch error must NOT be
		// swallowed: rendering on a stale base is how the work-dir and the
		// remote silently diverge.
		if err := p.syncToRemoteTip(ctx, repo, auth); err != nil {
			return nil, nil, "", err
		}
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, "", fmt.Errorf("publish: worktree: %w", err)
	}
	return repo, wt, dir, nil
}

// syncToRemoteTip fetches origin and hard-resets the worktree to the remote tip,
// establishing the invariant that the work-dir mirrors the remote before rendering.
//
// Two fetch outcomes are benign: an up-to-date remote, and an EMPTY remote — the
// latter reports transport.ErrEmptyRemoteRepository, not NoErrAlreadyUpToDate, and
// is the normal state of a target that has never been published to. Treating it as
// fatal would permanently break the first publish.
func (p *Publisher) syncToRemoteTip(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", Auth: auth})
	switch {
	case err == nil,
		errors.Is(err, git.NoErrAlreadyUpToDate),
		errors.Is(err, transport.ErrEmptyRemoteRepository):
		// benign
	default:
		return fmt.Errorf("publish: fetch: %w", err)
	}

	head, err := repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // no commits yet — nothing to reset to
	}
	if err != nil {
		return fmt.Errorf("publish: head: %w", err)
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", head.Name().Short()), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // remote has no such branch yet — we are ahead by definition
	}
	if err != nil {
		return fmt.Errorf("publish: reference: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("publish: worktree: %w", err)
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: ref.Hash()}); err != nil {
		return fmt.Errorf("publish: reset to remote tip: %w", err)
	}
	return nil
}

// auth resolves the git credential into a transport.AuthMethod, or a genuine nil
// interface when no CredentialRef is configured. Returning the INTERFACE (not a
// concrete *githttp.BasicAuth) is load-bearing: a typed-nil *BasicAuth assigned
// to go-git's transport.AuthMethod field is a non-nil interface wrapping a nil
// pointer, which the git:// and ssh transports reject outright as "invalid auth
// method" (they refuse any non-nil auth). A nil interface is the correct
// "anonymous" signal for every transport.
func (p *Publisher) auth(ctx context.Context) (transport.AuthMethod, error) {
	if p.cfg.CredentialRef == "" {
		return nil, nil
	}
	tok, err := p.secrets.Resolve(ctx, p.cfg.CredentialRef)
	if err != nil {
		return nil, fmt.Errorf("publish: resolve git credential: %w", err)
	}
	if tok == "" {
		return nil, fmt.Errorf("publish: empty git credential for ref %q (fail closed)", p.cfg.CredentialRef)
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
}

func (p *Publisher) push(ctx context.Context, repo *git.Repository) error {
	auth, err := p.auth(ctx)
	if err != nil {
		return err
	}
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []config.RefSpec{"refs/heads/*:refs/heads/*"},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("publish: push: %w", err)
	}
	return nil
}

// Enqueuer is the minimal interface the API depends on to request a publish.
type Enqueuer interface {
	Enqueue()
}

// oncePublisher is the single method the Worker loop needs. It exists so the
// loop's retry behaviour can be driven by a scripted publisher in tests: the
// real *Publisher talks to git, and a retry schedule cannot be tested through
// something whose failures are that expensive to arrange.
type oncePublisher interface {
	PublishOnce(ctx context.Context) (Result, error)
}

// Retry schedule for a failed publish. A failure is retried on its own, with
// no further Enqueue, because the desired end state is that the marketplace
// matches the catalog, and nothing else in the system will notice that it does
// not. Retries never give up: giving up silently would leave exactly the stale
// marketplace this slice exists to prevent, and publish_state plus the
// marketplace.publish audit events are what an operator watches instead.
//
// No jitter, deliberately. Jitter spreads a thundering herd across many
// clients; there is one publish worker per orbeat instance, and it retries one
// target, so jitter would add a moving part with nothing to spread.
const (
	defaultRetryBase = 5 * time.Second
	defaultRetryMax  = 30 * time.Minute
)

// nextRetryDelay returns how long to wait before the (failures+1)-th attempt:
// base doubled once per prior failure, capped at max. Pure, so the schedule is
// pinned by a test that never waits for a clock.
func nextRetryDelay(failures int, base, max time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := base
	for i := 1; i < failures; i++ {
		if d >= max {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// Worker debounces publish requests and runs them serially. Construct with
// NewWorker, call Start(ctx) once, and Enqueue() on each artifact mutation.
type Worker struct {
	p         oncePublisher
	onResult  func(ctx context.Context, res Result, err error)
	ch        chan struct{}
	debounce  time.Duration
	retryBase time.Duration
	retryMax  time.Duration
}

// NewWorker creates a Worker. onResult is called after each PublishOnce (may be nil).
func NewWorker(p oncePublisher, debounce time.Duration, onResult func(ctx context.Context, res Result, err error)) *Worker {
	return &Worker{
		p: p, onResult: onResult, ch: make(chan struct{}, 1), debounce: debounce,
		retryBase: defaultRetryBase, retryMax: defaultRetryMax,
	}
}

// Enqueue requests a publish (non-blocking; coalesces with any pending request).
func (w *Worker) Enqueue() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// Start runs the worker until ctx is cancelled.
//
// Two waits, and which one the loop is in is the whole design. With no failure
// outstanding it blocks for an Enqueue. With one outstanding it waits for the
// backoff OR an Enqueue, whichever lands first: a mutation during a backoff is
// a fresh reason to publish and should not queue behind a wait that could be
// half an hour long. An Enqueue does NOT reset the failure count, only a
// successful publish does, so a persistently broken target keeps backing off
// however busy the catalog is.
func (w *Worker) Start(ctx context.Context) {
	failures := 0
	for {
		if failures == 0 {
			select {
			case <-ctx.Done():
				return
			case <-w.ch:
			}
			if !sleepOrDone(ctx, w.debounce) {
				return
			}
		} else if !w.waitBeforeRetry(ctx, failures) {
			return
		}

		attempt := failures + 1
		res, err := w.p.PublishOnce(ctx)
		res.Attempt = attempt
		if err != nil {
			failures++
		} else {
			failures = 0
		}
		if w.onResult != nil {
			w.onResult(ctx, res, err)
		}
	}
}

// waitBeforeRetry blocks until the backoff elapses or an Enqueue arrives,
// returning false when ctx ended first (the caller must stop). An Enqueue is
// followed by the ordinary debounce, so a burst of mutations during a backoff
// still coalesces into one publish.
func (w *Worker) waitBeforeRetry(ctx context.Context, failures int) bool {
	t := time.NewTimer(nextRetryDelay(failures, w.retryBase, w.retryMax))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-w.ch:
		return sleepOrDone(ctx, w.debounce)
	}
}

// sleepOrDone waits for d, returning false if ctx ended first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ActiveArtifacts maps active store artifacts to marketplace input values.
// It is ONE of three construction sites turning a store.Artifact into a
// marketplace.Artifact, not the single bridge this comment claimed until audit
// finding C7. The other two are internal/api/sync.go's Channel-2 conversions.
// That matters concretely: add a field, trust the old sentence, update only
// here, and the field reaches the marketplace and never reaches orbeat-sync,
// which is the memory_scope-shaped distribution hole.
func ActiveArtifacts(ctx context.Context, st *store.Store, tenantID string) ([]marketplace.Artifact, error) {
	rows, err := st.ListActiveOrgArtifacts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]marketplace.Artifact, 0, len(rows))
	for _, a := range rows {
		out = append(out, marketplace.Artifact{Type: a.Type, Name: a.Name, Content: a.Content, MemoryScope: a.MemoryScope})
	}
	return out, nil
}

// isSchemeByte reports whether c may appear in a URI scheme
// (RFC 3986: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." )).
func isSchemeByte(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// hasSchemeBefore reports whether the "://" at sep is preceded by a URI
// scheme: a non-empty run of scheme bytes containing at least one letter,
// since any letter in that run is itself a legal scheme start. That is exactly
// what the old `[a-zA-Z][a-zA-Z0-9+.-]*://` regex accepted, and it is what
// keeps a bare local path and an SCP-like "git@host:path" (no scheme at all)
// out of the redactor.
func hasSchemeBefore(msg string, sep int) bool {
	for i := sep; i > 0; i-- {
		c := msg[i-1]
		if !isSchemeByte(c) {
			return false
		}
		if isASCIILetter(c) {
			return true
		}
	}
	return false
}

// isURLBoundary reports whether c ends a URL token inside an error message.
// Whitespace only: it cannot appear in a URL go-git actually dialed, and it is
// what separates the URL from the prose wrapped around it.
func isURLBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// redactURLUserinfo strips embedded URL credentials (https://user:token@host)
// from an error message before it is persisted (audit G12): a misconfigured
// ORBEAT_MARKETPLACE_GIT_URL carrying a token, or any go-git error that echoes
// the remote URL verbatim, would otherwise leak the token into publish_state,
// the audit table, the portal UI that renders both, and cmd/api's structured
// log on stdout.
//
// The rule, stated in full because a comment that overclaims here is worse
// than none. A URL runs from its "scheme://" to the first whitespace. Within
// that token the userinfo is everything up to the LAST "@". That is the same
// rule net/url applies inside an authority, and it is what makes a password
// containing "@" (https://user:p@ss@host) redact whole instead of half-way.
//
// The deliberate over-approximation is that the scan does NOT stop at the
// first "/". RFC 3986 ends the authority there, so a strict parse reads
// https://user:b64/tok@host as a credential-free path and redacts nothing,
// while base64 material, base64(user:pass), and GitLab/Gitea deploy tokens
// routinely contain "/". A slash is not legal in userinfo, so the string alone
// cannot say which reading is right; the ambiguity is resolved toward
// redacting.
//
// What that costs, and it is a real cost: a URL whose PATH contains "@" and
// carries no credential is over-redacted, host included, so
// "https://github.com/org/repo@v1.git" becomes "https://***@v1.git". A
// legibility loss on a message that held no secret, traded for never emitting
// one. Two things it still does not touch, also deliberately: an SCP-like
// "git@host:path" (no scheme, a conventional public login, key-based auth),
// and any "user@host" in prose that no "scheme://" precedes.
//
// Idempotent: after one pass the token holds exactly one "@", the one in
// "***@", so the last-"@" rule selects it again and rewrites it unchanged.
func redactURLUserinfo(msg string) string {
	if !strings.Contains(msg, "://") {
		return msg
	}
	var b strings.Builder
	i := 0
	for {
		off := strings.Index(msg[i:], "://")
		if off < 0 {
			break
		}
		sep := i + off
		after := sep + len("://")
		if !hasSchemeBefore(msg, sep) {
			b.WriteString(msg[i:after])
			i = after
			continue
		}
		end := after
		for end < len(msg) && !isURLBoundary(msg[end]) {
			end++
		}
		// No "@" anywhere in the token means no userinfo in it, and none in
		// any nested URL either, since the token spans them all.
		at := strings.LastIndexByte(msg[after:end], '@')
		if at < 0 {
			b.WriteString(msg[i:end])
			i = end
			continue
		}
		b.WriteString(msg[i:after])
		b.WriteString("***@")
		i = after + at + 1
	}
	b.WriteString(msg[i:])
	return b.String()
}

// redactedError wraps a publish error so its message can never carry embedded
// URL credentials, no matter which consumer formats it (slog, RecordResult, a
// future caller). Unwrap is preserved so errors.Is/As discrimination against
// the wrapped chain (e.g. context.DeadlineExceeded) still works for callers
// that inspect the error rather than its text.
type redactedError struct{ err error }

func (e *redactedError) Error() string { return redactURLUserinfo(e.err.Error()) }
func (e *redactedError) Unwrap() error { return e.err }

// RecordResult persists the outcome of a publish run into publish_state and
// appends a best-effort audit event. Preservation of the last good publish on
// failure is enforced by the store's SQL, not here.
func RecordResult(ctx context.Context, st *store.Store, tenantID string, res Result, pubErr error) {
	now := time.Now().UTC()
	var redactedErr string
	if pubErr != nil {
		redactedErr = redactURLUserinfo(pubErr.Error())
		if err := st.RecordPublishFailure(ctx, tenantID, now, redactedErr); err != nil {
			slog.Warn("record publish failure", "err", err)
		}
	} else if err := st.RecordPublishSuccess(ctx, tenantID, now, res.Commit); err != nil {
		slog.Warn("record publish success", "err", err)
	}

	// Best-effort audit: log on failure, never crash the worker.
	ae := store.AuditEvent{
		TenantID: tenantID,
		Actor:    "system",
		Action:   "marketplace.publish",
	}
	if pubErr != nil {
		ae.Decision = "error"
		ae.Target = ""
		ae.Metadata = map[string]any{"error": redactedErr}
	} else {
		ae.Decision = "allow"
		ae.Target = res.Commit
		if res.Changed {
			ae.Metadata = map[string]any{}
		} else {
			ae.Metadata = map[string]any{"noop": true}
		}
	}
	// Recorded only past the first try, so the common case stays exactly the
	// shape it has always been and the presence of the key is itself the
	// signal. On a failure it says how long this has been going; on a success
	// it marks the run as a recovery, which is otherwise indistinguishable
	// from an ordinary publish.
	if res.Attempt > 1 {
		ae.Metadata["attempt"] = res.Attempt
	}
	if _, err := st.AppendAuditEvent(ctx, ae); err != nil {
		slog.Warn("audit publish", "err", err)
	}
}
