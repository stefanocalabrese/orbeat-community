// Command marketplacegen renders the orbeat Claude Code plugin marketplace into a
// directory. The committed dev-default tree (marketplace/) is produced with the
// default flags; operators regenerate with their gateway URL for production.
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
)

func main() {
	slog.SetDefault(logging.New(os.Stdout, "text", "info"))

	out := flag.String("out", "marketplace", "output directory for the marketplace tree")
	gatewayURL := flag.String("gateway-url", "http://localhost:8090", "gateway public base URL (no /mcp)")
	flag.Parse()

	if err := marketplace.Generate(*out, marketplace.Options{GatewayURL: *gatewayURL}); err != nil {
		slog.Error("generate marketplace", "err", err)
		os.Exit(1)
	}
	slog.Info("wrote marketplace", "out", *out, "gateway", marketplace.MCPURL(*gatewayURL))
}
