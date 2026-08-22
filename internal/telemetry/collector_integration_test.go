package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestCollectorExportsSpanToJaeger drives the repo's own Setup — not a
// hand-rolled OTLP exporter — against the real, unmodified deploy/otel-collector.yaml,
// forwarding through a real otel/opentelemetry-collector container into a real
// Jaeger container, then confirms the span is actually queryable. compose_test.go
// (Task 3) already pins that the config *says* the right thing; this proves it
// *works*, and it is the only test that exercises the exporter-option branches
// in Setup (telemetry.go:118-130) against a live collector.
//
// Networking: Jaeger runs on a dedicated testcontainers network, aliased as
// "jaeger" — the exact hostname deploy/otel-collector.yaml hardcodes for its
// otlp/jaeger exporter. This is deliberate: a live probe of the shipped
// collector image (run by hand against this exact config file) shows its gRPC
// exporter eagerly resolving "jaeger" via DNS within milliseconds of startup,
// logging "no such host" until it succeeds — so a network + alias is not just
// the "more faithful" option, it is the only one under which the collector's
// export actually starts working, and it lets the test mount deploy/otel-collector.yaml
// verbatim rather than a templated copy that no longer proves the shipped file.
func TestCollectorExportsSpanToJaeger(t *testing.T) {
	ctx := context.Background()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() {
		termCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := net.Remove(termCtx); err != nil {
			t.Logf("remove network: %v", err)
		}
	})

	jaeger, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:           "orbeat-telemetry-itest-jaeger-" + randomSuffix(t),
			Image:          "jaegertracing/jaeger:2.11.0",
			ExposedPorts:   []string{"4317/tcp", "16686/tcp"},
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {"jaeger"}},
			// Jaeger v2 is built on the same otelcol service framework as the
			// collector image below and prints this exact line, once, after
			// every receiver/extension (including the OTLP gRPC receiver on
			// 4317) is up — verified live by reading its startup log. Unlike
			// Postgres, Jaeger's in-memory all-in-one mode does not restart
			// mid-init, so a single occurrence is correct; per CLAUDE.md's
			// container-readiness gotcha we still gate on the log line, not
			// the port alone.
			WaitingFor: wait.ForAll(
				wait.ForLog("Everything is ready. Begin running and processing data.").
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("4317/tcp").WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("16686/tcp").WithStartupTimeout(60*time.Second),
			),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start jaeger: %v", err)
	}
	t.Cleanup(func() { terminate(t, jaeger, "jaeger") })

	collector, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Name:         "orbeat-telemetry-itest-collector-" + randomSuffix(t),
			Image:        "otel/opentelemetry-collector:0.132.0",
			Cmd:          []string{"--config=/etc/otel-collector.yaml"},
			ExposedPorts: []string{"4317/tcp"},
			Networks:     []string{net.Name},
			Files: []testcontainers.ContainerFile{
				{
					HostFilePath:      collectorConfigPath(t),
					ContainerFilePath: "/etc/otel-collector.yaml",
					FileMode:          0o644,
				},
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("Everything is ready. Begin running and processing data.").
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("4317/tcp").WithStartupTimeout(60*time.Second),
			),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start collector: %v", err)
	}
	t.Cleanup(func() { terminate(t, collector, "collector") })

	collectorHost, err := collector.Host(ctx)
	if err != nil {
		t.Fatalf("collector host: %v", err)
	}
	collectorPort, err := collector.MappedPort(ctx, "4317/tcp")
	if err != nil {
		t.Fatalf("collector mapped port: %v", err)
	}
	endpoint := fmt.Sprintf("%s:%s", collectorHost, collectorPort.Port())

	const serviceName = "orbeat-collector-itest"
	spanName := fmt.Sprintf("orbeat-itest-span-%d", time.Now().UnixNano())

	providers, shutdown, err := Setup(ctx, Config{
		Endpoint:       endpoint,
		ServiceName:    serviceName,
		ServiceVersion: "itest",
		Insecure:       "true",
	})
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}

	_, span := providers.Tracer("orbeat/telemetry-itest").Start(ctx, spanName)
	if !span.SpanContext().IsValid() {
		t.Fatalf("expected a valid recording span context against a live collector endpoint")
	}
	span.End()

	// Shutdown flushes the batch span processor before tearing the exporter
	// down (SDK contract: Shutdown includes the effects of ForceFlush) — this
	// is what pushes the span onto the wire rather than leaving it sitting in
	// the batcher until its export interval. Skipping this is exactly the
	// mutation the red-proof below exercises.
	shutdownCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("telemetry shutdown (flush): %v", err)
	}

	queryHost, err := jaeger.Host(ctx)
	if err != nil {
		t.Fatalf("jaeger host: %v", err)
	}
	queryPort, err := jaeger.MappedPort(ctx, "16686/tcp")
	if err != nil {
		t.Fatalf("jaeger mapped port: %v", err)
	}
	queryBase := fmt.Sprintf("http://%s:%s", queryHost, queryPort.Port())

	found, diag := pollForSpan(queryBase, serviceName, spanName, 30*time.Second)
	if !found {
		t.Fatalf("span %q for service %q not found in Jaeger within 30s.\n%s\ncollector logs:\n%s",
			spanName, serviceName, diag, containerLogs(ctx, collector))
	}
}

// jaegerServicesResponse mirrors the Jaeger Query API's GET /api/services
// response shape (only the field this test needs).
type jaegerServicesResponse struct {
	Data []string `json:"data"`
}

// jaegerTracesResponse mirrors GET /api/traces?service=... (only the field
// this test needs — the operationName of each span is enough to confirm the
// exact span this test emitted made it all the way through the collector).
type jaegerTracesResponse struct {
	Data []struct {
		Spans []struct {
			OperationName string `json:"operationName"`
		} `json:"spans"`
	} `json:"data"`
}

// pollForSpan polls Jaeger's query API until a span named spanName is found
// under serviceName, or timeout expires. It returns a diagnostic string
// summarizing the last observation on failure, so a red run names exactly
// what WAS seen instead of a bare "not found after 30s".
func pollForSpan(queryBase, serviceName, spanName string, timeout time.Duration) (found bool, diag string) {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastServices, lastOps []string

	for {
		lastServices = fetchServices(client, queryBase)
		lastOps = nil
		for _, svc := range lastServices {
			if svc != serviceName {
				continue
			}
			lastOps = fetchOperationNames(client, queryBase, serviceName)
			for _, op := range lastOps {
				if op == spanName {
					return true, ""
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	return false, fmt.Sprintf("last GET %s/api/services: %v\nlast operationNames for %q: %v",
		queryBase, lastServices, serviceName, lastOps)
}

func fetchServices(client *http.Client, queryBase string) []string {
	resp, err := client.Get(queryBase + "/api/services") //nolint:noctx // polling helper, per-request timeout via client
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out jaegerServicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out.Data
}

func fetchOperationNames(client *http.Client, queryBase, serviceName string) []string {
	reqURL := fmt.Sprintf("%s/api/traces?service=%s", queryBase, url.QueryEscape(serviceName))
	resp, err := client.Get(reqURL) //nolint:noctx // polling helper, per-request timeout via client
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out jaegerTracesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	var ops []string
	for _, tr := range out.Data {
		for _, sp := range tr.Spans {
			ops = append(ops, sp.OperationName)
		}
	}
	return ops
}

// containerLogs reads a container's full log output, for attaching to a
// failure message — a bare "not found after 30s" is an unattributable
// failure that wastes the next person's afternoon.
func containerLogs(ctx context.Context, c testcontainers.Container) string {
	rc, err := c.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("(failed to fetch logs: %v)", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Sprintf("(failed to read logs: %v)", err)
	}
	return string(b)
}

// terminate tears a container down on a context independent of the test's,
// mirroring internal/testkc.StartKeycloak's rationale: t.Cleanup runs after
// the test function returns, by which point a per-test ctx the caller derived
// may already be Done, which would fail Terminate immediately and leak the
// container.
func terminate(t *testing.T, c testcontainers.Container, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Terminate(ctx); err != nil {
		t.Logf("terminate %s: %v", label, err)
	}
}

// collectorConfigPath resolves deploy/otel-collector.yaml relative to this
// source file, walking up to the repo root — robust regardless of the
// working directory `go test` runs from. internal/telemetry is two
// directories under the repo root, same depth as internal/testkc.
func collectorConfigPath(t testing.TB) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Clean(filepath.Join(repoRoot, "deploy", "otel-collector.yaml"))
}

// randomSuffix gives container names a per-run-unique suffix, mirroring
// internal/testkc's rationale: a crashed prior run's container (Ryuk not yet
// reaped) can otherwise collide on a deterministic name and Docker refuses to
// create the duplicate.
func randomSuffix(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
