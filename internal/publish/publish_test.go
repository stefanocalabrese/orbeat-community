package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
)

type fakeSecrets struct{}

func (fakeSecrets) Resolve(_ context.Context, _ string) (string, error) { return "", nil }

func snapshot(artifacts []marketplace.Artifact) func(context.Context) ([]marketplace.Artifact, error) {
	return func(context.Context) ([]marketplace.Artifact, error) { return artifacts, nil }
}

func TestPublishLocalCommitsInPlace(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{GitURL: dir, GatewayURL: "http://localhost:8090"}, fakeSecrets{},
		snapshot([]marketplace.Artifact{{Type: "skill", Name: "fmt", Content: "---\nname: fmt\ndescription: d\n---\nx"}}))
	res, err := p.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Commit == "" {
		t.Fatal("empty commit hash")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "orbeat-artifacts", "skills", "fmt", "SKILL.md")); err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if _, err := git.PlainOpen(dir); err != nil {
		t.Fatalf("target is not a git repo: %v", err)
	}
}

// TestPublishDeletionPropagates verifies that when an artifact present in commit
// N is absent from the next publish, the file is removed from the git tree —
// i.e. wt.Add(".") stages tracked-file deletions correctly.
func TestPublishDeletionPropagates(t *testing.T) {
	dir := t.TempDir()
	artA := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}
	artB := marketplace.Artifact{Type: "skill", Name: "beta", Content: "---\nname: beta\ndescription: d\n---\nbody"}
	bPath := filepath.Join(dir, "plugins", "orbeat-artifacts", "skills", "beta", "SKILL.md")

	// Publish [A, B].
	p := New(Config{GitURL: dir, GatewayURL: "http://localhost:8090"}, fakeSecrets{}, snapshot([]marketplace.Artifact{artA, artB}))
	if _, err := p.PublishOnce(context.Background()); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, err := os.Stat(bPath); err != nil {
		t.Fatalf("beta not written after first publish: %v", err)
	}

	// Publish [A] only — beta must be gone.
	p2 := New(Config{GitURL: dir, GatewayURL: "http://localhost:8090"}, fakeSecrets{}, snapshot([]marketplace.Artifact{artA}))
	res, err := p2.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if res.Commit == "" {
		t.Fatal("expected a new commit (beta deletion), got empty hash")
	}

	// Assert the file is gone from disk.
	if _, err := os.Stat(bPath); !os.IsNotExist(err) {
		t.Fatalf("beta still present on disk after second publish (err=%v)", err)
	}

	// Assert the deletion is recorded in the committed git tree.
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	ref, err := repo.Head()
	if err != nil {
		t.Fatalf("repo head: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("commit tree: %v", err)
	}
	_, err = tree.File("plugins/orbeat-artifacts/skills/beta/SKILL.md")
	if err != object.ErrFileNotFound {
		t.Fatalf("beta/SKILL.md is still in the committed tree (err=%v) — deletion not staged", err)
	}
}

func TestPublishRemotePushesToBareRepo(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	p := New(Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}, fakeSecrets{},
		snapshot([]marketplace.Artifact{{Type: "subagent", Name: "rev", Content: "---\nname: rev\ndescription: d\n---\nx", MemoryScope: "project"}}))
	if _, err := p.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publish remote: %v", err)
	}
	// clone the bare repo fresh and assert the pushed tree
	check := t.TempDir()
	if _, err := git.PlainClone(check, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone check: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(check, "plugins", "orbeat-artifacts", "agents", "rev.md"))
	if err != nil {
		t.Fatalf("agent not pushed: %v", err)
	}
	if !strings.Contains(string(b), "memory: project") {
		t.Fatalf("memory not in pushed agent: %s", b)
	}
}

// TestPublishRemoteSecondPushWorkDirReuse verifies that a second PublishOnce
// call to the same remote bare repo succeeds: the work-dir is reused via
// PlainOpen + pull, a new commit is created, and the push succeeds without a
// non-fast-forward error.
func TestPublishRemoteSecondPushWorkDirReuse(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	artA := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}
	artB := marketplace.Artifact{Type: "skill", Name: "beta", Content: "---\nname: beta\ndescription: d\n---\nbody"}

	// First publish.
	p1 := New(Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}, fakeSecrets{}, snapshot([]marketplace.Artifact{artA}))
	r1, err := p1.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("first remote publish: %v", err)
	}
	if r1.Commit == "" {
		t.Fatal("first publish: expected non-empty hash")
	}

	// Second publish with different artifacts — work-dir must be reused.
	p2 := New(Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}, fakeSecrets{}, snapshot([]marketplace.Artifact{artA, artB}))
	r2, err := p2.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("second remote publish: %v", err)
	}
	if r2.Commit == "" {
		t.Fatal("second publish: expected non-empty commit hash")
	}
	if r1.Commit == r2.Commit {
		t.Fatal("second publish produced the same hash as the first — no new commit")
	}

	// Verify that the second push is visible in a fresh clone.
	check := t.TempDir()
	if _, err := git.PlainClone(check, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(check, "plugins", "orbeat-artifacts", "skills", "beta", "SKILL.md")); err != nil {
		t.Fatalf("beta not present after second push: %v", err)
	}
}

// TestPublishNoOpStaysNoOp pins the Result contract on the no-op path: an
// unchanged catalog must report Changed=false and leave the commit where it was,
// while Commit is still populated (from repo.Head()).
func TestPublishNoOpStaysNoOp(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	cfg := Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}
	art := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}

	p1 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	r1, err := p1.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if !r1.Changed {
		t.Fatal("first publish must report Changed")
	}

	p2 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	r2, err := p2.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if r2.Changed {
		t.Fatal("an unchanged catalog must not report Changed")
	}
	if r2.Commit != r1.Commit {
		t.Fatalf("no-op moved the commit: %s -> %s", r1.Commit, r2.Commit)
	}
	if r2.Commit == "" {
		t.Fatal("Commit must be populated even on a no-op")
	}
}

// TestPublishRetryAfterFailedPush is the reported defect: a push that fails
// leaves a commit in the persistent work-dir; the retry re-renders identical
// bytes, so IsClean is true and the push is never reached. The remote stays
// stale forever while the banner goes green.
func TestPublishRetryAfterFailedPush(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	cfg := Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}
	artA := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}
	artB := marketplace.Artifact{Type: "skill", Name: "beta", Content: "---\nname: beta\ndescription: d\n---\nbody"}

	// Seed: one good publish so the remote has a tip and a tracking ref.
	p0 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{artA}))
	if _, err := p0.PublishOnce(context.Background()); err != nil {
		t.Fatalf("seed publish: %v", err)
	}

	// Break the remote, then publish new content: the commit lands locally, the push fails.
	moved := bare + "-moved"
	if err := os.Rename(bare, moved); err != nil {
		t.Fatalf("rename away: %v", err)
	}
	p1 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{artA, artB}))
	if _, err := p1.PublishOnce(context.Background()); err == nil {
		t.Fatal("expected publish to fail while the remote is unreachable")
	}
	if err := os.Rename(moved, bare); err != nil {
		t.Fatalf("rename back: %v", err)
	}

	// The retry must actually publish beta.
	p2 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{artA, artB}))
	res, err := p2.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("retry publish: %v", err)
	}
	if res.Commit == "" {
		t.Fatal("retry: expected a commit hash")
	}

	check := t.TempDir()
	if _, err := git.PlainClone(check, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(check, "plugins", "orbeat-artifacts", "skills", "beta", "SKILL.md")); err != nil {
		t.Fatalf("beta never reached the remote after the retry: %v", err)
	}
}

// TestPublishRetryAfterFailedFirstPushToEmptyRemote covers first-publish-to-an-
// empty-remote: with no successful push yet there is no tracking ref, so only the
// unconditional push (not any work-dir reset) can get the content onto the remote.
func TestPublishRetryAfterFailedFirstPushToEmptyRemote(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	cfg := Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}
	art := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}

	// Break the remote BEFORE the first publish ever succeeds.
	moved := bare + "-moved"
	if err := os.Rename(bare, moved); err != nil {
		t.Fatalf("rename away: %v", err)
	}
	p1 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	if _, err := p1.PublishOnce(context.Background()); err == nil {
		t.Fatal("expected the first publish to fail with the remote unreachable")
	}
	if err := os.Rename(moved, bare); err != nil {
		t.Fatalf("rename back: %v", err)
	}

	p2 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	if _, err := p2.PublishOnce(context.Background()); err != nil {
		t.Fatalf("retry publish to empty remote: %v", err)
	}

	check := t.TempDir()
	if _, err := git.PlainClone(check, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(check, "plugins", "orbeat-artifacts", "skills", "alpha", "SKILL.md")); err != nil {
		t.Fatalf("alpha never reached the empty remote after the retry: %v", err)
	}
}

// TestPublishRemoteCloneFailureSurfacesAsCloneError pins audit G10: a clone
// failure that is NOT "empty remote" (unreachable host, auth failure, DNS)
// must be reported as a clone error — not silently treated as a fresh remote,
// which proceeds to init+commit and then fails later with a misleading push
// error (or worse, a non-fast-forward against a remote that was merely
// unreachable for a moment).
func TestPublishRemoteCloneFailureSurfacesAsCloneError(t *testing.T) {
	work := t.TempDir()
	// Connection-refused endpoint: a clone failure that is not ErrEmptyRemoteRepository.
	p := New(Config{GitURL: "http://127.0.0.1:1/nope.git", WorkDir: work, GatewayURL: "http://localhost:8090"},
		fakeSecrets{}, snapshot([]marketplace.Artifact{{Type: "skill", Name: "a", Content: "---\nname: a\ndescription: d\n---\nx"}}))
	_, err := p.PublishOnce(context.Background())
	if err == nil {
		t.Fatal("expected publish against an unreachable remote to fail")
	}
	if !strings.Contains(err.Error(), "publish: clone") {
		t.Fatalf("the failure must be diagnosed as a CLONE error, got: %v", err)
	}
}

// TestWorkerDebounceAndOnResult verifies that a single Enqueue() triggers
// PublishOnce after the debounce window and that onResult is called with
// a non-empty commit hash. Uses a done channel — no sleeps.
func TestWorkerDebounceAndOnResult(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{GitURL: dir, GatewayURL: "http://localhost:8090"}, fakeSecrets{},
		snapshot([]marketplace.Artifact{
			{Type: "skill", Name: "worker-skill", Content: "---\nname: worker-skill\ndescription: d\n---\nx"},
		}))

	done := make(chan string, 1) // receives the commit hash from onResult
	worker := NewWorker(p, 10*time.Millisecond, func(_ context.Context, res Result, err error) {
		if err != nil {
			t.Errorf("onResult error: %v", err)
		}
		done <- res.Commit
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go worker.Start(ctx)

	worker.Enqueue()

	select {
	case commit := <-done:
		if commit == "" {
			t.Fatal("onResult received empty commit hash; expected a real commit")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for onResult to be called after Enqueue()")
	}
}

// TestPublishResetsDivergentWorkDir pins the invariant that the work-dir mirrors
// the remote tip before rendering: a local commit the remote never saw must be
// discarded by the reset, not carried into the next publish.
func TestPublishResetsDivergentWorkDir(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	cfg := Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090"}
	art := marketplace.Artifact{Type: "skill", Name: "alpha", Content: "---\nname: alpha\ndescription: d\n---\nbody"}

	p1 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	r1, err := p1.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Hand-create a divergent commit in the work-dir that the remote never saw.
	repo, err := git.PlainOpen(work)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "junk.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatalf("add junk: %v", err)
	}
	junk, err := wt.Commit("junk", &git.CommitOptions{Author: &object.Signature{Name: "x", Email: "x@x", When: time.Now()}})
	if err != nil {
		t.Fatalf("commit junk: %v", err)
	}

	// Publish the same artifacts again: the reset must discard the junk commit.
	p2 := New(cfg, fakeSecrets{}, snapshot([]marketplace.Artifact{art}))
	r2, err := p2.PublishOnce(context.Background())
	if err != nil {
		t.Fatalf("publish after divergence: %v", err)
	}
	if r2.Commit == junk.String() {
		t.Fatal("the junk commit survived — the work-dir was not reset to the remote tip")
	}
	if r2.Commit != r1.Commit {
		t.Fatalf("expected reset back to the remote tip %s, got %s", r1.Commit, r2.Commit)
	}

	check := t.TempDir()
	if _, err := git.PlainClone(check, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(check, "junk.txt")); err == nil {
		t.Fatal("junk.txt reached the remote — the work-dir was not reset")
	}
}

// TestAuthNilInterfaceWithoutCredential pins the auth() footgun fix: with no
// CredentialRef, auth must yield a genuine nil transport.AuthMethod, NOT a
// typed-nil *githttp.BasicAuth. A typed-nil pointer assigned to the interface
// is non-nil and makes go-git's non-HTTP transports (git://, ssh) reject the
// request with "invalid auth method" — the exact failure the remote smoke hit.
func TestAuthNilInterfaceWithoutCredential(t *testing.T) {
	p := New(Config{GitURL: "git://example.test/x.git"}, fakeSecrets{}, snapshot(nil))
	var am transport.AuthMethod
	am, err := p.auth(context.Background())
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if am != nil {
		t.Fatalf("auth without a credential must be a nil interface; got non-nil %T (typed-nil breaks git://)", am)
	}
}

// TestRedactURLUserinfo pins audit G12: a git URL's embedded credentials
// (https://user:token@host) must never survive into a persisted error
// string — RecordResult writes this into publish_state.last_error and the
// audit table's metadata, both readable from the portal UI.
func TestRedactURLUserinfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"basic userinfo with password",
			"https://user:token@example.test/repo.git",
			"https://***@example.test/repo.git",
		},
		{
			"wrapped clone error (realistic go-git message)",
			"publish: clone: https://x-access-token:ghp_abc123XYZ@github.com/org/repo.git: dial tcp: connection refused",
			"publish: clone: https://***@github.com/org/repo.git: dial tcp: connection refused",
		},
		{
			"username only, no password",
			"publish: push: https://ghp_abc123XYZ@github.com/org/repo.git: authentication failed",
			"publish: push: https://***@github.com/org/repo.git: authentication failed",
		},
		{
			"SCP-like syntax is untouched (no scheme, key-based auth)",
			"publish: push: git@github.com:org/repo.git: permission denied (publickey)",
			"publish: push: git@github.com:org/repo.git: permission denied (publickey)",
		},
		{
			"no URL present",
			"publish: status: context deadline exceeded",
			"publish: status: context deadline exceeded",
		},
		{
			"local filesystem path is untouched",
			"publish: init local repo: /var/lib/orbeat/marketplace: permission denied",
			"publish: init local repo: /var/lib/orbeat/marketplace: permission denied",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactURLUserinfo(c.in); got != c.want {
				t.Errorf("redactURLUserinfo(%q) =\n got:  %q\n want: %q", c.in, got, c.want)
			}
		})
	}
}

// TestRedactedErrorRedactsBothPaths pins the redactedError shape directly
// (not the go-git integration): it proves the test CAN fail by asserting the
// unwrapped inner error still carries the secret (a leak is possible on this
// path), then asserts the wrapped Error() text does not.
func TestRedactedErrorRedactsBothPaths(t *testing.T) {
	inner := errors.New("publish: clone: https://x-access-token:ghp_SUPERSECRET99@github.com/org/repo.git: dial tcp: connection refused")
	wrapped := &redactedError{err: inner}

	unwrapped := errors.Unwrap(error(wrapped))
	if unwrapped == nil || !strings.Contains(unwrapped.Error(), "ghp_SUPERSECRET99") {
		t.Fatalf("test is vacuous: unwrapped error must still carry the secret, got: %v", unwrapped)
	}

	got := wrapped.Error()
	if strings.Contains(got, "ghp_SUPERSECRET99") {
		t.Fatalf("wrapped error leaked the secret: %q", got)
	}
	if !strings.Contains(got, "***@") {
		t.Fatalf("wrapped error missing the redaction marker: %q", got)
	}
}

// TestRedactedErrorPreservesErrorsIs pins that wrapping does not break
// errors.Is discrimination against the underlying chain — PublishOnce's own
// timeout test relies on errors.Is(err, context.DeadlineExceeded) still
// working through the wrapper.
func TestRedactedErrorPreservesErrorsIs(t *testing.T) {
	inner := fmt.Errorf("publish: head: %w", context.DeadlineExceeded)
	wrapped := error(&redactedError{err: inner})
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatalf("errors.Is did not see through redactedError to %v", context.DeadlineExceeded)
	}
}

// TestPublishOnceRedactsEmbeddedCredentialOnRealCloneFailure drives an ACTUAL
// go-git clone failure — no synthetic/fake error — through the real
// PublishOnce production path, using a GitURL that embeds credentials (the
// shape audit G12 exists for) and an invalid host character so the failure
// surfaces from url.Parse rather than net/http's own dial-error redaction
// (net/http.stripPassword already scrubs *url.Error from Client.Do, so a
// plain connection-refused case would pass vacuously here). PublishOnce must
// return a *redactedError, and its Error() must not leak the token while its
// Unwrap()'d cause still does — proving this is a real, exploitable path and
// not a manufactured one.
func TestPublishOnceRedactsEmbeddedCredentialOnRealCloneFailure(t *testing.T) {
	work := t.TempDir()
	const token = "ghp_REALCLONELEAK42"
	p := New(Config{
		GitURL:     "https://x-access-token:" + token + "@bad host.invalid/nope.git",
		WorkDir:    work,
		GatewayURL: "http://localhost:8090",
	}, fakeSecrets{}, snapshot([]marketplace.Artifact{{Type: "skill", Name: "a", Content: "---\nname: a\ndescription: d\n---\nx"}}))

	_, err := p.PublishOnce(context.Background())
	if err == nil {
		t.Fatal("expected publish against a malformed credentialed URL to fail")
	}

	var re *redactedError
	if !errors.As(err, &re) {
		t.Fatalf("PublishOnce did not return a *redactedError, got %T: %v", err, err)
	}

	inner := errors.Unwrap(err)
	if inner == nil || !strings.Contains(inner.Error(), token) {
		t.Fatalf("test is vacuous: the real go-git clone error must still carry the token before redaction, got: %v", inner)
	}

	got := err.Error()
	if strings.Contains(got, token) {
		t.Fatalf("PublishOnce leaked the embedded credential: %q", got)
	}
	if !strings.Contains(got, "***@") {
		t.Fatalf("PublishOnce error missing the redaction marker: %q", got)
	}
}

// TestPublishOnceHonorsTimeout pins that a publish run is bounded: with a tiny
// timeout and an already-past deadline, PublishOnce returns a (context) error
// rather than proceeding/hanging. Guards the fix for unbounded git ops.
func TestPublishOnceHonorsTimeout(t *testing.T) {
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	// A 1ns timeout expires before any git op can run.
	p := New(Config{GitURL: bare, WorkDir: work, GatewayURL: "http://localhost:8090", Timeout: time.Nanosecond},
		fakeSecrets{}, snapshot([]marketplace.Artifact{{Type: "skill", Name: "a", Content: "---\nname: a\ndescription: d\n---\nx"}}))
	_, err := p.PublishOnce(context.Background())
	if err == nil {
		t.Fatal("expected PublishOnce to fail under an expired timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a context.DeadlineExceeded error, got: %v", err)
	}
}
