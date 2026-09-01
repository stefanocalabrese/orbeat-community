package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// listAudit drives the real handler and returns the decoded envelope.
func listAudit(t *testing.T, srv *Server, tn store.Tenant, query string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodGet, "/v1/admin/audit"+query, nil, tn)
	srv.handleListAudit(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v (body %s)", query, err, rec.Body)
		}
	}
	return rec.Code, out
}

func auditActors(t *testing.T, env map[string]any) []string {
	t.Helper()
	events, ok := env["events"].([]any)
	if !ok {
		t.Fatalf("envelope has no events array: %#v", env)
	}
	var out []string
	for _, e := range events {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("event is not an object: %#v", e)
		}
		out = append(out, fmt.Sprintf("%v/%v/%v", m["actor"], m["action"], m["decision"]))
	}
	return out
}

// TestListAuditQueryFilters drives the three query parameters end to end
// through the handler, asserting the COUNT each returns. A count is the
// assertion that discriminates: an "every returned row matches" check passes
// unchanged on a handler that drops the filter and returns one matching row
// among many, which is the shape a parameter parsed but never passed on
// produces.
func TestListAuditQueryFilters(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	for _, e := range []store.AuditEvent{
		{Actor: "alice", Action: "role.delete", Decision: "allow"},
		{Actor: "alice", Action: "artifact.approve", Decision: "deny"},
		{Actor: "bob", Action: "role.delete", Decision: "deny"},
		{Actor: "bob", Action: "artifact.approve", Decision: "error"},
	} {
		e.TenantID = tn.ID
		if _, err := st.AppendAuditEvent(ctx, e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 4},
		{"?actor=alice", 2},
		{"?action=role.delete", 2},
		{"?decision=deny", 2},
		{"?decision=error", 1},
		{"?actor=alice&decision=deny", 1},
		{"?actor=alice&action=role.delete&decision=allow", 1},
		{"?actor=alice&action=role.delete&decision=deny", 0},
		{"?actor=nobody", 0},
		{"?actor=", 4},
	} {
		t.Run("q"+tc.query, func(t *testing.T) {
			code, env := listAudit(t, srv, tn, tc.query)
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			got := auditActors(t, env)
			if len(got) != tc.want {
				t.Fatalf("%q returned %d events, want %d: %v", tc.query, len(got), tc.want, got)
			}
		})
	}
}

// TestListAuditRejectsUnknownDecision pins that a decision outside the column's
// CHECK is a 400 and not an empty page. The domain belongs to the schema, not
// to this handler, so refusing is honest rather than presumptuous: no row can
// ever carry the value, and an empty 200 would read as "nothing happened".
func TestListAuditRejectsUnknownDecision(t *testing.T) {
	srv, _, tn := newAdminServer(t)
	// "allow%20" is percent-encoded on purpose: a literal trailing space in a
	// request target is eaten by the request-line parser, so the unencoded form
	// would test the transport rather than the handler.
	for _, bad := range []string{"maybe", "ALLOW", "allow%20", "deny,error", "allow'"} {
		t.Run(bad, func(t *testing.T) {
			code, _ := listAudit(t, srv, tn, "?decision="+bad)
			if code != http.StatusBadRequest {
				t.Fatalf("decision=%q returned %d, want 400", bad, code)
			}
		})
	}
}

// TestListAuditFilterPaginates walks a filtered result set through the
// handler's own nextCursor, which is where a filter dropped on the second
// request would show up: the cursor encodes a position, not a query, so the
// client resends the filters and the server has to apply them again.
func TestListAuditFilterPaginates(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	for i := 0; i < 3; i++ {
		for _, e := range []store.AuditEvent{
			{TenantID: tn.ID, Actor: "alice", Action: "role.delete", Decision: "deny"},
			{TenantID: tn.ID, Actor: "bob", Action: "artifact.approve", Decision: "allow"},
		} {
			if _, err := st.AppendAuditEvent(ctx, e); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	seen := map[string]bool{}
	query := "?actor=alice&decision=deny&limit=1"
	for page := 0; page < 5; page++ {
		code, env := listAudit(t, srv, tn, query)
		if code != http.StatusOK {
			t.Fatalf("page %d status %d", page, code)
		}
		events, _ := env["events"].([]any)
		if len(events) == 0 {
			break
		}
		m := events[0].(map[string]any)
		if m["actor"] != "alice" || m["decision"] != "deny" {
			t.Fatalf("page %d leaked %v/%v", page, m["actor"], m["decision"])
		}
		id := m["id"].(string)
		if seen[id] {
			t.Fatalf("page %d repeated %s", page, id)
		}
		seen[id] = true
		next, _ := env["nextCursor"].(string)
		if next == "" {
			break
		}
		query = "?actor=alice&decision=deny&limit=1&cursor=" + next
	}
	if len(seen) != 3 {
		t.Fatalf("walked %d filtered events, want 3", len(seen))
	}
}

// TestOpenAPIDecisionEnumMatchesTheDomain closes the third copy of the decision
// domain. The database's CHECK constraint and store.AuditDecisions() are
// already pinned to each other (store.TestAuditDecisionsMatchTheSchemaCheck);
// documenting the enum in openapi.yaml added a third place to be wrong, and a
// spec that lists a value the server rejects is worse than a spec that says
// nothing, because a client trusts it.
func TestOpenAPIDecisionEnumMatchesTheDomain(t *testing.T) {
	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*enum: \[(allow[^\]]*)\]`).FindSubmatch(spec)
	if m == nil {
		t.Fatal("no decision enum found in openapi.yaml — this test cannot fail as written")
	}
	var fromSpec []string
	for _, v := range strings.Split(string(m[1]), ",") {
		fromSpec = append(fromSpec, strings.TrimSpace(v))
	}
	want := store.AuditDecisions()
	sort.Strings(want)
	sort.Strings(fromSpec)
	if !slices.Equal(fromSpec, want) {
		t.Fatalf("openapi.yaml documents decision values %v, store.AuditDecisions() = %v", fromSpec, want)
	}
}
