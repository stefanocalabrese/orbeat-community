// Command upstream is a tiny example MCP server for orbeat smoke tests.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoArgs struct {
	Text string `json:"text"`
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "example-upstream", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes text"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	log.Println("example-upstream: listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", mux))
}
