package govern

import (
	"context"
	"testing"
)

type stubScanner struct {
	findings []Finding
	err      error
}

func (s stubScanner) Scan(ctx context.Context, p ArtifactPayload) ([]Finding, error) {
	return s.findings, s.err
}

func TestCompositeConcatenatesInOrder(t *testing.T) {
	a := stubScanner{findings: []Finding{{Rule: "a", Severity: "block"}}}
	b := stubScanner{findings: []Finding{{Rule: "llm-b", Severity: "warn"}}}
	f, err := NewCompositeScanner(a, b).Scan(context.Background(), ArtifactPayload{})
	if err != nil {
		t.Fatalf("composite err: %v", err)
	}
	if len(f) != 2 || f[0].Rule != "a" || f[1].Rule != "llm-b" {
		t.Fatalf("order/contents wrong: %+v", f)
	}
}

func TestCompositeSkipsNilScanners(t *testing.T) {
	f, err := NewCompositeScanner(nil, stubScanner{findings: []Finding{{Rule: "x"}}}, nil).
		Scan(context.Background(), ArtifactPayload{})
	if err != nil || len(f) != 1 {
		t.Fatalf("nil-skip failed: %v %+v", err, f)
	}
}

// The governing invariant: rules keep block authority; an LLM warn is advisory.
func TestCompositeBlockAuthorityInvariant(t *testing.T) {
	rules := NewDefaultScanner()
	llmWarn := stubScanner{findings: []Finding{{Rule: "llm-x", Severity: "warn", Message: "advisory"}}}
	comp := NewCompositeScanner(rules, llmWarn)

	// Clean content + LLM warn → surfaced, but not blocking.
	f, _ := comp.Scan(context.Background(), ArtifactPayload{Type: "skill", Name: "n", Content: "hello world"})
	if HasBlocking(f) {
		t.Fatalf("LLM warn must not block: %+v", f)
	}
	if len(f) != 1 || f[0].Severity != "warn" {
		t.Fatalf("expected the LLM warn to surface: %+v", f)
	}

	// A rule-detected secret still blocks even with the LLM layer present.
	f2, _ := comp.Scan(context.Background(), ArtifactPayload{Type: "skill", Name: "n",
		Content: "key AKIAIOSFODNN7EXAMPLE"})
	if !HasBlocking(f2) {
		t.Fatalf("rule secret must still block: %+v", f2)
	}
}
