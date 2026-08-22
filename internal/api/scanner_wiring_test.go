package api

import (
	"context"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/govern"
)

type stubScanner struct{}

func (stubScanner) Scan(ctx context.Context, p govern.ArtifactPayload) ([]govern.Finding, error) {
	return nil, nil
}

func TestSetScannerOverridesAndIgnoresNil(t *testing.T) {
	// New with nil deps is fine: no handler is invoked, no DB touched.
	srv := New(nil, nil, nil, nil, nil)
	if srv.scanner == nil {
		t.Fatal("New should install a default scanner")
	}
	srv.SetScanner(stubScanner{})
	if _, ok := srv.scanner.(stubScanner); !ok {
		t.Fatal("SetScanner should override the default")
	}
	srv.SetScanner(nil) // nil must be ignored, not wipe the scanner
	if _, ok := srv.scanner.(stubScanner); !ok {
		t.Fatal("SetScanner(nil) should be ignored")
	}
}
