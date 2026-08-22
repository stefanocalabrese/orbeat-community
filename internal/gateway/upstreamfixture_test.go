package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoArgs is the upstream echo tool's typed input.
type echoArgs struct {
	Text string `json:"text"`
}

// upstreamFixture is an in-process Streamable-HTTP MCP server exposing one tool.
type upstreamFixture struct {
	URL      string
	httpSrv  *httptest.Server
	mu       sync.Mutex
	lastAuth string // last Authorization header seen
}

// newUpstreamFixture starts an MCP upstream serving an `echo` tool over real HTTP.
func newUpstreamFixture(t *testing.T) *upstreamFixture {
	t.Helper()
	return newUpstreamFixtureWithTools(t, "echo")
}

// newUpstreamFixtureWithTools starts an MCP upstream serving one echo-style
// tool per name over real HTTP. Distinct tool names let tests tell two
// upstreams apart (e.g. the slug-collision test, where both servers share a
// gateway slug but must NOT share a tool registry).
func newUpstreamFixtureWithTools(t *testing.T, toolNames ...string) *upstreamFixture {
	t.Helper()
	return newUpstreamFixtureWithOptions(t, nil, toolNames...)
}

// newUpstreamFixtureWithOptions is newUpstreamFixtureWithTools with explicit
// SDK ServerOptions (e.g. a small PageSize to exercise tool-list pagination).
func newUpstreamFixtureWithOptions(t *testing.T, opts *mcp.ServerOptions, toolNames ...string) *upstreamFixture {
	t.Helper()
	f := &upstreamFixture{}

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "0.0.1"}, opts)
	for _, name := range toolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "echoes text"},
			func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
			})
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		mcpHandler.ServeHTTP(w, r)
	})
	f.httpSrv = httptest.NewServer(wrapped)
	f.URL = f.httpSrv.URL
	t.Cleanup(f.httpSrv.Close)
	return f
}

func (f *upstreamFixture) lastAuthHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

// newSlowUpstreamFixture starts a Streamable-HTTP MCP upstream whose single
// `slow_echo` tool sleeps for delay before replying — the SDK's server sends
// tool results as the first SSE event, so response headers are withheld for
// the whole delay. Used to pin that no header/deadline timeout caps the
// gateway's tool-call path.
func newSlowUpstreamFixture(t *testing.T, delay time.Duration) *upstreamFixture {
	t.Helper()
	f := &upstreamFixture{}

	srv := mcp.NewServer(&mcp.Implementation{Name: "slow-upstream", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "slow_echo", Description: "echoes text, slowly"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})

	f.httpSrv = httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	f.URL = f.httpSrv.URL
	t.Cleanup(f.httpSrv.Close)
	return f
}

// sseUpstreamFixture is an in-process SSE MCP server exposing the same `echo`
// tool as upstreamFixture, over the SDK's SSE transport (hanging GET stream).
type sseUpstreamFixture struct {
	URL     string
	httpSrv *httptest.Server
}

// newSSEUpstreamFixture starts an SSE MCP upstream serving an `echo` tool.
func newSSEUpstreamFixture(t *testing.T) *sseUpstreamFixture {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "sse-upstream", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes text"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})

	handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	f := &sseUpstreamFixture{httpSrv: httptest.NewServer(handler)}
	f.URL = f.httpSrv.URL
	t.Cleanup(f.httpSrv.Close)
	return f
}

// CloseClientConnections severs every live client connection (including the
// SSE hanging GET) while keeping the fixture listening, simulating an upstream
// that dropped its connections but is still up.
func (f *sseUpstreamFixture) CloseClientConnections() {
	f.httpSrv.CloseClientConnections()
}
