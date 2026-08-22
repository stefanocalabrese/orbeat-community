// Command smokeclient exercises a tool through the orbeat-gateway over MCP.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "smokeclient: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	url := os.Getenv("GATEWAY_MCP_URL")
	token := os.Getenv("ACCESS_TOKEN")
	wantTool := os.Getenv("WANT_TOOL")
	if url == "" || token == "" || wantTool == "" {
		fail("GATEWAY_MCP_URL, ACCESS_TOKEN, WANT_TOOL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpClient := &http.Client{Transport: bearerRT{token: token}}
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "orbeat-smokeclient", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		fail("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		fail("listtools: %v", err)
	}
	var found string
	for _, t := range tools.Tools {
		fmt.Println("  visible tool:", t.Name)
		if t.Name == wantTool {
			found = t.Name
		}
	}
	if found == "" {
		fail("expected tool %q not visible in gateway catalog", wantTool)
	}

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: found, Arguments: json.RawMessage(`{"text":"smoke-hello"}`)})
	if err != nil {
		fail("calltool %s: %v", found, err)
	}
	if len(out.Content) == 0 {
		fail("empty tool result")
	}
	tc, ok := out.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "smoke-hello" {
		fail("unexpected tool result: %+v", out.Content)
	}
	fmt.Printf("  OK: called %s through gateway, got %q\n", found, tc.Text)
}
