package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
)

func TestAuditDualEmitOnCommit(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	var buf bytes.Buffer
	srv.logger = logging.New(&buf, "json", "info") // same-package field override

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPost, "/v1/admin/servers",
		map[string]any{"name": "audit-emit-srv", "transport": "http",
			"endpointOrCommand": "http://x:1/mcp", "status": "active"}, tn)
	srv.handleCreateServer(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body %s", rec.Code, rec.Body)
	}
	var sawAudit bool
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["event"] == "audit" && m["action"] == "server.create" {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Fatalf("expected an event=audit log line for server.create; got %q", buf.String())
	}
}
