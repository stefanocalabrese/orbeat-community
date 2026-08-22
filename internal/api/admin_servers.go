package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stefanocalabrese/orbeat-community/internal/naming"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// serverInput is the admin write DTO. It carries the secret REFERENCE (never a
// raw secret) and the endpoint/command, which the read-only catalog DTO omits.
type serverInput struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	Transport         string `json:"transport"`
	EndpointOrCommand string `json:"endpointOrCommand"`
	Version           string `json:"version"`
	ProtocolVersion   string `json:"protocolVersion"`
	SecretRef         string `json:"secretRef"`
	TLSCARef          string `json:"tlsCaRef"`
	Status            string `json:"status"`
}

// adminServerDTO is the admin read projection: like the catalog DTO plus the
// endpoint and hasSecret/hasTlsCa flags, but it NEVER echoes the secret_ref or
// tls_ca_ref values.
type adminServerDTO struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Transport         string `json:"transport"`
	EndpointOrCommand string `json:"endpointOrCommand"`
	Version           string `json:"version"`
	ProtocolVersion   string `json:"protocolVersion"`
	Status            string `json:"status"`
	HasSecret         bool   `json:"hasSecret"`
	HasTLSCA          bool   `json:"hasTlsCa"`
	// RowVersion is the optimistic-concurrency token (spec §4): the value a
	// client must echo back in If-Match to update this row. Shared by
	// handleListServers, handleGetServer and handleUpdateServer, since all
	// three go through toAdminServerDTO.
	RowVersion int64 `json:"rowVersion"`
}

func toAdminServerDTO(m store.MCPServer) adminServerDTO {
	return adminServerDTO{
		ID: m.ID, Name: m.Name, Description: m.Description, Transport: m.Transport,
		EndpointOrCommand: m.EndpointOrCommand, Version: m.Version,
		ProtocolVersion: m.ProtocolVersion, Status: m.Status, HasSecret: m.SecretRef != "",
		HasTLSCA:   m.TLSCARef != "",
		RowVersion: m.RowVersion,
	}
}

// decodeJSON strictly decodes the request body into v: it rejects unknown
// fields AND any trailing data after the single JSON value (dec.More()), so a
// body like `{...}{...}` or `{...} junk` is a 400, not a silently-accepted
// first object. An empty body still returns io.EOF, which decodeJSONOrFail maps
// to 400 "invalid JSON body" — that is what makes an absent body a client error
// on every handler that decodes one (no caller exempts it).
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing data after JSON value")
	}
	return nil
}

// decodeJSONOrFail decodes the request body into v, writing the appropriate
// error response and returning false on failure so the caller can just
// `return`. maxBytesMiddleware (api.go) wraps every mutating route's body in
// http.MaxBytesReader; when that cap is exceeded, decodeJSON's underlying read
// fails with *http.MaxBytesError, which must map to 413 — not the generic 400
// "invalid JSON body" every other decode failure gets (audit B3).
func decodeJSONOrFail(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(r, v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}

// validTransport reports whether t is a transport a NEW catalog entry may use.
//
// "stdio" is deliberately absent even though the mcp_server CHECK constraint
// still permits it. orbeat-gateway is a REMOTE broker: its transport switch
// (internal/gateway/broker.go) builds a Streamable-HTTP or SSE client and
// returns "unsupported upstream transport" for anything else, and no other
// channel consumes catalog servers (the marketplace ships the gateway plugin
// plus artifacts; orbeat-sync ships artifacts and the gateway's own MCP entry;
// the portal Connect page hardcodes the gateway's own http transport). A stdio
// row was therefore accepted, listed in the catalog, and silently yielded zero
// tools — the exact failure this validation exists to prevent.
//
// The DB constraint is left alone on purpose: pre-existing stdio rows must stay
// READABLE (constraining it would need a lossy migration — a command line cannot
// become a URL). They keep being skipped by the gateway exactly as before; they
// simply can no longer be created or updated. Delete them.
func validTransport(t string) bool {
	switch t {
	case "http", "sse":
		return true
	default:
		return false
	}
}

// validServerStatus reports whether s is one of the catalog's allowed
// lifecycle states (mirrors the mcp_server_status_check DB constraint).
func validServerStatus(s string) bool {
	switch s {
	case "active", "disabled":
		return true
	default:
		return false
	}
}

// validEndpoint reports whether endpoint is usable as an upstream MCP URL.
//
// PRECONDITION: the caller has already run validTransport, so transport is
// http or sse and the value is always a URL — there is no command form to
// exempt. (stdio was removed from validTransport; see its doc comment.)
//
// It must be an absolute http(s) URL with a real host and no embedded
// credentials.
//
// It checks u.Hostname(), NOT u.Host: Host includes the port, so "http://:8080"
// has a NON-EMPTY Host and an empty Hostname — a hostless URL that a Host check
// would accept and that nothing can ever dial.
//
// url.IsAbs() is deliberately not checked: it is equivalent to Scheme != "",
// which requiring http/https already implies.
//
// Userinfo is rejected because it is a raw credential in a DB column that the
// admin DTO echoes back, against a system whose posture is
// references-never-values.
func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "" {
		return false
	}
	// A port outside 1-65535 parses fine and passes the scheme+hostname checks,
	// but nothing can ever dial it — url.Parse only rejects a non-numeric or
	// negative port, so ":0", ":65536" and ":99999" would otherwise be stored as
	// a server that yields zero tools with no explanation. u.Port() is "" when no
	// port is present, which is valid (the scheme's default applies).
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return u.User == nil
}

// validateServerWrite applies the write-time rules shared by create and update.
// It returns the client-facing 400 message and false when the input is invalid.
// Pure: a stateless check needs the resolver, not the whole Server.
//
// The message names the FIELD and the RULE and never the submitted value: a
// secretRef may be a raw secret pasted by mistake, and an endpoint may carry
// userinfo (ftp://user:pw@h/x is rejected for its SCHEME, so any "echo unless
// the problem is userinfo" carve-out would leak the password).
//
// Surfacing resolver.ValidateRef's error text directly is deliberate and safe:
// every error it can return is a STATIC literal (the only operator-input
// interpolation in internal/secrets lives in Resolve, not ValidateRef), and both
// packages have tests asserting no validation error echoes the submitted value.
// It is also strictly more useful than a blanket message — it distinguishes an
// unregistered scheme from a vault locator missing its #field. Spec §6 permits
// granularity; the invariant is the absence of the value, not one message.
// TestServerWriteSurfacesSpecificRefReason pins the granularity so a
// "simplification" back to one static string cannot pass silently.
func validateServerWrite(resolver *secrets.Resolver, endpoint, secretRef, tlsCARef string) (msg string, ok bool) {
	if !validEndpoint(endpoint) {
		return "endpointOrCommand must be an absolute http(s) URL with a host and no embedded credentials", false
	}
	if err := resolver.ValidateRef(secretRef); err != nil {
		return err.Error(), false
	}
	// Structural only: ValidateRef checks the scheme is registered and the
	// locator is usable. It deliberately does NOT resolve the ref or check that
	// the bytes parse as a certificate — that needs I/O, the line v1.21.0 drew.
	// A syntactically valid ref pointing at garbage fails at DIAL time and skips
	// that upstream (spec §8), not at write time.
	//
	// The "tlsCaRef: " prefix disambiguates which of the two ref fields failed:
	// both now share the same validator and the same error text, and an admin
	// who typo'd tlsCaRef's scheme needs to know that, not "secretRef" by
	// omission.
	if err := resolver.ValidateRef(tlsCARef); err != nil {
		return "tlsCaRef: " + err.Error(), false
	}
	return "", true
}

// checkServerSlugCollision rejects a create/update whose name would collide
// with another server's after slugification. The DB is unique on the RAW name
// only, but the gateway routes tool calls by naming.Slugify(name) — which is
// lossy ("My Server", "my-server" → "my-server") — so admitting two servers
// with the same slug would silently misroute per-call RBAC (audit G3).
// excludeID skips the server being updated (renaming onto its own slug is fine).
//
// This check is advisory-pre-tx: it reads outside the write transaction, so a
// racing create can still slip a colliding pair through. The gateway's
// build-time slug guard (first-registered wins, collider skipped + audited) is
// the fail-safe backstop that keeps routing correct regardless.
func (s *Server) checkServerSlugCollision(w http.ResponseWriter, r *http.Request, tenantID, name, excludeID string) (ok bool) {
	slug := naming.Slugify(name)
	servers, err := s.store.ListMCPServersByTenant(r.Context(), tenantID)
	if err != nil {
		fail(w, err)
		return false
	}
	for _, other := range servers {
		if other.ID == excludeID {
			continue
		}
		if naming.Slugify(other.Name) == slug {
			fail(w, conflictError{fmt.Sprintf("server name collides with %q after slugification", other.Name)})
			return false
		}
	}
	return true
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in serverInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.Name == "" || in.Transport == "" || in.EndpointOrCommand == "" || in.Status == "" {
		writeError(w, http.StatusBadRequest, "name, transport, endpointOrCommand, status are required")
		return
	}
	if !validTransport(in.Transport) {
		writeError(w, http.StatusBadRequest, "transport must be one of http, sse (stdio servers cannot be brokered by the remote gateway)")
		return
	}
	if !validServerStatus(in.Status) {
		writeError(w, http.StatusBadRequest, "status must be one of active, disabled")
		return
	}
	if msg, ok := validateServerWrite(s.secrets, in.EndpointOrCommand, in.SecretRef, in.TLSCARef); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !s.checkServerSlugCollision(w, r, rc.TenantID, in.Name, "") {
		return
	}
	if err := s.checkServerActiveCap(r.Context(), rc.TenantID, "", in.Status); err != nil {
		fail(w, err)
		return
	}
	m := store.MCPServer{
		TenantID: rc.TenantID, Name: in.Name, Description: in.Description,
		Transport: in.Transport, EndpointOrCommand: in.EndpointOrCommand,
		Version: in.Version, ProtocolVersion: in.ProtocolVersion,
		SecretRef: in.SecretRef, TLSCARef: in.TLSCARef, Status: in.Status,
	}
	var created store.MCPServer
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		created, e = tx.CreateMCPServer(r.Context(), m)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "server.create",
			Target: created.ID, Decision: "allow",
			Metadata: map[string]any{"name": created.Name},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminServerDTO(created))
}

// handleListServers is keyset-paginated (?limit, ?cursor; see paging.go). The
// nextCursor heuristic (len(rows)==limit means "possibly more"; an exact
// multiple of limit costs one extra empty page) is documented once, on
// handleListRoles above — it applies identically here.
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText})
	if err != nil {
		fail(w, err)
		return
	}
	servers, err := s.store.ListMCPServersPage(r.Context(), rc.TenantID, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]adminServerDTO, 0, len(servers))
	for _, m := range servers {
		out = append(out, toAdminServerDTO(m))
	}
	next := ""
	if len(servers) == limit && limit > 0 {
		next = encodeListCursor(store.MCPServerCursor(servers[len(servers)-1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out, "limit": limit, "nextCursor": next})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	m, err := s.store.GetMCPServer(r.Context(), rc.TenantID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	// The by-id read is where a client obtains the ETag it will later echo
	// back as If-Match (spec §4: this route reads no query params, so it has
	// exactly one representation and a strong ETag is safe here).
	w.Header().Set("ETag", etag(m.RowVersion))
	writeJSON(w, http.StatusOK, toAdminServerDTO(m))
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	// Parsed before touching the store: a missing/malformed/refused
	// precondition must reject the request without any read or write
	// (spec §5). expected is the row_version the CLIENT last saw.
	expected, err := ifMatch(r)
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	var in serverInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.Name == "" || in.Transport == "" || in.EndpointOrCommand == "" || in.Status == "" {
		writeError(w, http.StatusBadRequest, "name, transport, endpointOrCommand, status are required")
		return
	}
	if !validTransport(in.Transport) {
		writeError(w, http.StatusBadRequest, "transport must be one of http, sse (stdio servers cannot be brokered by the remote gateway)")
		return
	}
	if !validServerStatus(in.Status) {
		writeError(w, http.StatusBadRequest, "status must be one of active, disabled")
		return
	}
	if msg, ok := validateServerWrite(s.secrets, in.EndpointOrCommand, in.SecretRef, in.TLSCARef); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !s.checkServerSlugCollision(w, r, rc.TenantID, in.Name, id) {
		return
	}
	if err := s.checkServerActiveCap(r.Context(), rc.TenantID, id, in.Status); err != nil {
		fail(w, err)
		return
	}
	m := store.MCPServer{
		ID: id, TenantID: rc.TenantID, Name: in.Name, Description: in.Description,
		Transport: in.Transport, EndpointOrCommand: in.EndpointOrCommand,
		Version: in.Version, ProtocolVersion: in.ProtocolVersion,
		SecretRef: in.SecretRef, TLSCARef: in.TLSCARef, Status: in.Status,
		// RowVersion carries the CLIENT's expected version (from If-Match) into
		// UpdateMCPServer's `... AND row_version=$n` guard. The CTE (§6.2)
		// distinguishes "doesn't exist" from "exists but stale" in one
		// statement, so no precedent fetch is needed here.
		RowVersion: expected,
	}
	var updated store.MCPServer
	err = s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		updated, e = tx.UpdateMCPServer(r.Context(), m)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "server.update",
			Target: updated.ID, Decision: "allow", Metadata: map[string]any{"name": updated.Name},
		}, nil
	})
	if err != nil {
		// A stale If-Match is a rejected mutation and, under the fail-closed
		// audit invariant (v1.17.0 finding B1: "deny decisions were never
		// audited"), must leave a durable trace before the client sees the
		// 412 — mirroring admin_artifact_review.go's transitionHandler
		// forbiddenError arm exactly, including its precedent: if the audit
		// write itself fails, the caller gets 500, not a silent 412. A 428
		// (missing/refused If-Match) is a client bug, not a security event,
		// and is deliberately NOT audited (spec §9).
		if errors.Is(err, store.ErrVersionMismatch) {
			if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
				TenantID: rc.TenantID, Actor: p.Subject, Action: "server.update",
				Target: id, Decision: "deny",
				Metadata: map[string]any{"name": in.Name, "reason": "version_mismatch"},
			}); aerr != nil {
				fail(w, aerr)
				return
			}
		}
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(updated.RowVersion))
	writeJSON(w, http.StatusOK, toAdminServerDTO(updated))
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		if e := tx.DeleteMCPServer(r.Context(), rc.TenantID, id); e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "server.delete",
			Target: id, Decision: "allow",
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
