// Package govern holds orbeat's artifact governance checks. The Scanner runs at
// submit-for-review time; its findings gate submission (block) or inform the
// human approver (warn/info). The default scanner is rule-based and in-process;
// an LLM-based scanner can drop in behind the same interface later.
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

// MaxContentBytes and MaxSeedBytes are the size ceilings for an artifact's
// content and memory seed. The scanner WARNS at these sizes (informing the human
// approver); the api layer HARD-REJECTS above them at create/update time
// (validateArtifact) — the two must agree, so both read these constants.
const (
	MaxContentBytes = 64 * 1024
	MaxSeedBytes    = 16 * 1024
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
	if len(p.Content) > MaxContentBytes {
		f = append(f, Finding{Rule: "size", Severity: "warn", Message: "content exceeds 64KiB"})
	}
	if len(p.MemorySeed) > MaxSeedBytes {
		f = append(f, Finding{Rule: "size", Severity: "warn", Message: "seed exceeds 16KiB"})
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

func scanReserved(where, text string) []Finding {
	if reservedMarkerRe.MatchString(text) {
		return []Finding{{Rule: "reserved-marker", Severity: "block",
			Message: "reserved orbeat managed-block sentinel marker in " + where}}
	}
	return nil
}
