package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// TestRBACDecisionMetricOnlyCountsPerCallDecisions pins audit G12: the
// orbeat.gateway.rbac_decisions counter must reflect ONLY the per-call tool
// allow/deny outcomes decided by rbacMiddleware — not every audit event the
// gateway writes (e.g. gateway.upstream.connect during session build), which
// used to inflate/skew deny-rate dashboards by counting connect events too.
func TestRBACDecisionMetricOnlyCountsPerCallDecisions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("rbac-metric-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	metrics := telemetry.NewMetrics(mp.Meter("gateway-metric-test"))

	srv := &Server{
		store:    st,
		resolver: authz.NewResolver(st, tn.Name),
		secrets:  secrets.NewResolver(),
		logger:   logging.New(io.Discard, "json", "info"),
		metrics:  metrics,
	}

	sess := &session{
		subject: "kc-metrictest", tenantID: tn.ID, actor: "kc-metrictest",
		entitlements: []store.Entitlement{{MCPServerID: "srv-1", AllowedTools: nil}},
		slugToServer: map[string]string{"alpha": "srv-1"},
	}

	// A non-tool-call audit event (simulating gateway.upstream.connect) must
	// NOT touch the RBAC-decision counter.
	srv.audit(ctx, sess, "gateway.upstream.connect", "srv-1", "allow")

	handler := srv.rbacMiddleware(sess)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, nil
	})

	allowReq := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "alpha__anything"}}
	if _, err := handler(ctx, "tools/call", allowReq); err != nil {
		t.Fatalf("allow call: %v", err)
	}
	denyReq := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "unknown__x"}}
	if _, err := handler(ctx, "tools/call", denyReq); err == nil {
		t.Fatal("expected the deny call to error")
	}

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.rbac_decisions" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T for %s", md.Data, md.Name)
			}
			for _, dp := range sum.DataPoints {
				dec, _ := dp.Attributes.Value(attribute.Key("decision"))
				got[dec.AsString()] += dp.Value
			}
		}
	}
	if got["allow"] != 1 {
		t.Errorf("allow decisions = %d, want 1 (upstream.connect must not count)", got["allow"])
	}
	if got["deny"] != 1 {
		t.Errorf("deny decisions = %d, want 1", got["deny"])
	}
	if total := got["allow"] + got["deny"]; total != 2 {
		t.Errorf("total rbac_decisions = %d, want exactly 2 (the two tool calls only)", total)
	}
}

// TestTruncateAuditTarget pins the 256-rune cap on client-supplied tool names
// flowing into the audit target (audit G12): an unbounded name is attacker-
// controlled data that would otherwise blow up audit storage/log volume, and
// a naive byte-slice truncation could split a multi-byte rune and produce
// invalid UTF-8 that Postgres' text column rejects.
func TestTruncateAuditTarget(t *testing.T) {
	short := "alpha__tool"
	if got := truncateAuditTarget(short); got != short {
		t.Errorf("short name should pass through unchanged, got %q", got)
	}

	long := strings.Repeat("a", 500)
	got := truncateAuditTarget(long)
	if len([]rune(got)) != maxAuditTargetRunes {
		t.Errorf("truncated length = %d, want %d", len([]rune(got)), maxAuditTargetRunes)
	}

	// A multi-byte rune sitting exactly on the cut boundary must not be split.
	multiByte := strings.Repeat("a", maxAuditTargetRunes-1) + "€€€"
	gotMB := truncateAuditTarget(multiByte)
	if !utf8.ValidString(gotMB) {
		t.Errorf("truncation produced invalid UTF-8: %q", gotMB)
	}
	if len([]rune(gotMB)) != maxAuditTargetRunes {
		t.Errorf("multi-byte truncated rune length = %d, want %d", len([]rune(gotMB)), maxAuditTargetRunes)
	}
}

// connectDurationPoint is one orbeat.gateway.upstream.connect.duration data
// point, reduced to the fields the two tests below assert on: which server it
// was dialed for, what outcome it landed under, and how many measurements
// share that (server, outcome) pair.
type connectDurationPoint struct {
	server, outcome string
	count           uint64
}

// collectUpstreamConnectPoints reads every orbeat.gateway.upstream.connect.duration
// data point currently in rdr. Reducing to connectDurationPoint keeps the two
// tests below asserting on server/outcome/count — not merely that a data
// point exists, since a measurement recorded under the wrong outcome would
// otherwise still pass a "some data point exists" check.
func collectUpstreamConnectPoints(t *testing.T, rdr *sdkmetric.ManualReader) []connectDurationPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var points []connectDurationPoint
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.upstream.connect.duration" {
				continue
			}
			hist, ok := md.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("unexpected data type %T for %s", md.Data, md.Name)
			}
			for _, dp := range hist.DataPoints {
				server, _ := dp.Attributes.Value(attribute.Key("server"))
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				points = append(points, connectDurationPoint{
					server: server.AsString(), outcome: outcome.AsString(), count: dp.Count,
				})
			}
		}
	}
	return points
}

// TestBuildSessionRecordsSuccessfulConnectDuration pins design
// 2026-08-18-orbeat-gateway-connect-metrics-design.md §4: a successful dial
// inside buildSession records exactly one orbeat.gateway.upstream.connect.duration
// measurement, attributed with the dialed server's name and outcome=ok. This
// asserts the attributes, not just that some data point landed, so a
// measurement recorded under outcome=error would fail this test even though
// the histogram is non-empty.
func TestBuildSessionRecordsSuccessfulConnectDuration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("connect-metric-ok-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	role, err := st.CreateRole(ctx, tn.ID, "orbeat-user")
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	good := newUpstreamFixture(t)
	goodSrv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "good-connect", Transport: "http", EndpointOrCommand: good.URL, Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: goodSrv.ID, AllowedTools: []string{"echo"},
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	metrics := telemetry.NewMetrics(mp.Meter("connect-metric-ok-test"))

	gw := &Server{
		store:    st,
		resolver: authz.NewResolver(st, tn.Name),
		secrets:  secrets.NewResolver(),
		logger:   logging.New(io.Discard, "json", "info"),
		metrics:  metrics,
	}

	sess, err := gw.buildSession(ctx, auth.Principal{Subject: "kc-connect-metric-ok", Roles: []string{"orbeat-user"}})
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	t.Cleanup(sess.close)

	got := collectUpstreamConnectPoints(t, rdr)
	want := []connectDurationPoint{{server: goodSrv.Name, outcome: "ok", count: 1}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("connect duration points = %+v, want %+v", got, want)
	}
}

// TestBuildSessionRecordsFailedConnectDuration pins the other half of design
// §4: "record on BOTH paths" — a dial that fails to connect must still land a
// measurement, under outcome=error, not silence. The endpoint is a bare
// connection-refused address (port 1), the same literal
// TestGatewayDegradesOnBadUpstreams already uses to force connectUpstream to
// fail synchronously — upstreamfixture_test.go has no "fails to dial" fixture
// (every helper there starts a real, reachable httptest.Server), and this
// package already has a working, precedented way to get one without adding a
// new fixture function.
func TestBuildSessionRecordsFailedConnectDuration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("connect-metric-err-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	role, err := st.CreateRole(ctx, tn.ID, "orbeat-user")
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	deadSrv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "dead-connect", Transport: "http", EndpointOrCommand: "http://127.0.0.1:1/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: deadSrv.ID, AllowedTools: []string{"echo"},
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	metrics := telemetry.NewMetrics(mp.Meter("connect-metric-err-test"))

	gw := &Server{
		store:    st,
		resolver: authz.NewResolver(st, tn.Name),
		secrets:  secrets.NewResolver(),
		logger:   logging.New(io.Discard, "json", "info"),
		metrics:  metrics,
	}

	// buildSession itself does not error: a dead upstream is skipped and
	// audited, not a session-build failure (see TestGatewayDegradesOnBadUpstreams).
	sess, err := gw.buildSession(ctx, auth.Principal{Subject: "kc-connect-metric-err", Roles: []string{"orbeat-user"}})
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	t.Cleanup(sess.close)

	got := collectUpstreamConnectPoints(t, rdr)
	want := []connectDurationPoint{{server: deadSrv.Name, outcome: "error", count: 1}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("connect duration points = %+v, want %+v", got, want)
	}
}
