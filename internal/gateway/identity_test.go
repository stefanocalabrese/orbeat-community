package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// syncBuffer wraps a bytes.Buffer with a mutex: a real httptest.Server's
// request-handling goroutines write log lines concurrently with the test
// body reading them back (a live Streamable HTTP connection can still be
// finishing a request when the client call that triggered it returns), and
// bytes.Buffer itself is not safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestRequestLogCarriesIdentity is the fable-audit §7 #14 gate: a REAL request
// driven through the compiled Handler() must produce an "http_request" log
// line carrying the caller's subject AND tenant — not a hand-fed identity
// closure (that would only prove logging.Requests' own plumbing, already
// covered by internal/logging's tests), and not a value read from source.
//
// This is deliberately an end-to-end drive (real store, real Handler(), real
// HTTP+MCP round trip) rather than a unit test of gatewayIdentity/withSession
// in isolation: the defect this closes was a WIRING gap (Handler() passed
// nil), and a unit test of the helper functions alone cannot prove they were
// ever actually reached from Handler()'s real composition.
func TestRequestLogCarriesIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("identity-log-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	verifier := stubVerifier(map[string]auth.Principal{
		"ivy-tok": {Subject: "kc-ivy-identity", Email: "ivy@x.io"},
	})

	buf := &syncBuffer{}
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	gw.logger = logging.New(buf, "json", "info") // capture, in place of New()'s slog.Default()
	t.Cleanup(gw.Close)
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "ivy-tok")
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var sawSubject, sawTenant bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		if fields["msg"] != "http_request" {
			continue
		}
		if fields["subject"] == "kc-ivy-identity" {
			sawSubject = true
		}
		if fields["tenant"] == tn.ID {
			sawTenant = true
		}
	}
	if !sawSubject {
		t.Fatalf("no http_request log line carried subject=%q; log:\n%s", "kc-ivy-identity", buf.String())
	}
	if !sawTenant {
		t.Fatalf("no http_request log line carried tenant=%q; log:\n%s", tn.ID, buf.String())
	}
}

// TestRequestLogOmitsIdentityOnRejectedToken pins the other half of the
// contract (fable-audit §7 #14): a request that never carries a valid
// principal must NOT fabricate one. It also proves withIdentityCarrier didn't
// regress the pre-existing "every request is logged exactly once" coverage —
// TestGatewayRejectsBadToken already pins the 401 status; this pins that the
// SAME request still produces an http_request line, just without identity.
func TestRequestLogOmitsIdentityOnRejectedToken(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	buf := &syncBuffer{}
	gw := New(st, authz.NewResolver(st, "default"), stubVerifier(map[string]auth.Principal{}), secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	gw.logger = logging.New(buf, "json", "info")
	t.Cleanup(gw.Close)
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	resp, err := httpSrv.Client().Get(httpSrv.URL + "/mcp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	var lines int
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		if fields["msg"] != "http_request" {
			continue
		}
		lines++
		if _, ok := fields["subject"]; ok {
			t.Fatalf("rejected-token request must not carry a subject field, got: %s", line)
		}
		if _, ok := fields["tenant"]; ok {
			t.Fatalf("rejected-token request must not carry a tenant field, got: %s", line)
		}
	}
	if lines != 1 {
		t.Fatalf("want exactly 1 http_request log line for the rejected request, got %d", lines)
	}
}
