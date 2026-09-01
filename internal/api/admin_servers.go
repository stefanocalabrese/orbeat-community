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

// serverInput is the admin CREATE write DTO. It carries the secret REFERENCE
// (never a raw secret) and the endpoint/command, which the read-only catalog
// DTO omits. SecretRef/TLSCARef are plain strings here, deliberately NOT the
// *string tri-state serverUpdateInput below uses: a create has no existing
// value to preserve, so an omitted field and an explicit "" mean exactly the
// same thing ("no reference at all") — there is no third state to express.
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

// serverUpdateInput is the admin UPDATE write DTO (PUT /v1/admin/servers/{id}).
// It differs from serverInput ONLY in SecretRef/TLSCARef's type, and that
// difference is the whole fix for defect 1 (2026-09-01, BREAKING): before
// this, an update wrote plain strings straight into NULLIF of empty string, so an
// OMITTED key and an EXPLICIT "" were byte-identical on the wire and both
// wiped the stored reference — and since GetMCPServer/toAdminServerDTO never
// echo either ref back (hasSecret/hasTlsCa booleans only, by design), no
// read-modify-write caller could ever have resent the value it couldn't
// read, so every partial update silently destroyed both refs.
//
// *string makes the three states an update can express explicit at the type
// level rather than resting on a caller-side convention nothing enforces:
//   - nil (the key is absent from the JSON body): leave the stored value
//     exactly as it is. This is the new default, and it is what makes a
//     script that PATCHes only, say, `status` safe to write for the first
//     time.
//   - a pointer to "" (the key is present with an empty value): explicit
//     clear.
//   - a pointer to a non-empty string: replace.
//
// See UpdateMCPServer's own doc comment (internal/store/mcpserver.go) for how
// this reaches SQL, and validateServerWrite's doc comment for how a nil skips
// validation (there is no new value on this write to validate).
type serverUpdateInput struct {
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	Transport         string  `json:"transport"`
	EndpointOrCommand string  `json:"endpointOrCommand"`
	Version           string  `json:"version"`
	ProtocolVersion   string  `json:"protocolVersion"`
	SecretRef         *string `json:"secretRef"`
	TLSCARef          *string `json:"tlsCaRef"`
	Status            string  `json:"status"`
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
// secretRef and tlsCARef are *string so a single validator serves both
// serverInput (create, always non-nil — see its own doc comment) and
// serverUpdateInput (update, nil means "this write does not mention the
// field"). A nil is skipped entirely rather than treated as "": there is no
// new value on this write to check, and ValidateRef's own contract already
// treats "" as valid-and-meaningless (an explicit clear needs no scheme), so
// folding nil into "" would happen to produce the same answer today but for
// the wrong reason — a future ValidateRef change with a side effect for ""
// (a metric, a deprecation warning) would then fire on every omitted field
// on every update, which nothing about "the caller didn't mention this
// field" should ever trigger.
func validateServerWrite(resolver *secrets.Resolver, endpoint string, secretRef, tlsCARef *string) (msg string, ok bool) {
	if !validEndpoint(endpoint) {
		return "endpointOrCommand must be an absolute http(s) URL with a host and no embedded credentials", false
	}
	if secretRef != nil {
		if err := resolver.ValidateRef(*secretRef); err != nil {
			return err.Error(), false
		}
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
	if tlsCARef != nil {
		if err := resolver.ValidateRef(*tlsCARef); err != nil {
			return "tlsCaRef: " + err.Error(), false
		}
	}
	return "", true
}

// derefRefOrUnchanged renders a tri-state update field for the deny-audit
// path (handleUpdateServer's ErrVersionMismatch arm), which records the
// ATTEMPTED write since nothing was persisted. nil (the request never
// mentioned this field) is rendered as the literal "(unchanged)" rather than
// "" — the two are not the same attempt, and a deny row claiming an admin
// tried to clear a ref they never touched would misname a security-relevant
// event on the one surface that exists to describe it accurately (audit A4).
// A non-nil pointer, including one to "" (an explicit attempted clear), is
// rendered as its dereferenced value like every other field this metadata
// records.
func derefRefOrUnchanged(p *string) string {
	if p == nil {
		return "(unchanged)"
	}
	return *p
}

// serverWriteAuditMetadata is the audit metadata every mcp_server write
// records. It exists because "name" alone made the audit trail unable to
// distinguish adding a legitimate MCP server from pointing one at an
// attacker's host with a credential ref of the attacker's choosing (audit A4):
// the endpoint and the two refs ARE the security-relevant content of the
// write, and neither was recorded.
//
// Recording the refs is safe, and the reason is a precondition rather than a
// hope: every ALLOW-path caller runs validateServerWrite first, so by the
// time this is called each ref is either empty or a well-formed
// "<scheme>:<locator>" with a registered scheme and a locator that scheme
// accepts. A raw secret pasted into the secretRef field has already been
// refused with a 400 that does not echo it, so it can never reach this map.
// That is also why the values may be recorded verbatim while the 400
// messages next door may not name them.
//
// Empty refs are recorded as "" rather than omitted, so "this server carries no
// credential" is an assertion in the row instead of an absence a reader has to
// interpret. handleUpdateServer's ErrVersionMismatch (DENY) arm is the one
// caller that does NOT pass a validated ref straight through: it renders a
// tri-state *string first (derefRefOrUnchanged), so the value that reaches
// this map there can also be the literal "(unchanged)" — a nil the request
// never mentioned that field, not a well-formed ref. That value is still
// exactly what it claims to be (the field genuinely was not part of the
// attempted write), so it does not weaken the "safe to record verbatim"
// argument above; it is simply a third case the ALLOW-path callers never
// produce.
func serverWriteAuditMetadata(name, endpoint, secretRef, tlsCARef string) map[string]any {
	return map[string]any{
		"name":              name,
		"endpointOrCommand": endpoint,
		"secretRef":         secretRef,
		"tlsCaRef":          tlsCARef,
	}
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
	if msg, ok := validateServerWrite(s.secrets, in.EndpointOrCommand, &in.SecretRef, &in.TLSCARef); !ok {
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
			Metadata: serverWriteAuditMetadata(created.Name, created.EndpointOrCommand,
				created.SecretRef, created.TLSCARef),
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
//
// ?q= searches name, the same column this list sorts on, IN SQL, see
// handleListRoles' comment on why (identical reasoning, applies to every
// search-supporting list).
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
	desc, err := sortOrderParams(r, mcpServerSortName)
	if err != nil {
		fail(w, err)
		return
	}
	servers, err := s.store.ListMCPServersPage(r.Context(), rc.TenantID, cursor, limit, desc, searchParam(r))
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
		next = encodeListCursor(store.MCPServerCursor(servers[len(servers)-1], desc))
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
	var in serverUpdateInput
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
		// SecretRef/TLSCARef are deliberately NOT set here: UpdateMCPServer
		// ignores them on m and takes the tri-state in.SecretRef/in.TLSCARef as
		// explicit parameters instead (see that function's own doc comment) —
		// setting them on m as well would be dead code inviting a future editor
		// to "fix" this call by wiring them back in, silently reviving defect 1.
		ID: id, TenantID: rc.TenantID, Name: in.Name, Description: in.Description,
		Transport: in.Transport, EndpointOrCommand: in.EndpointOrCommand,
		Version: in.Version, ProtocolVersion: in.ProtocolVersion, Status: in.Status,
		// RowVersion carries the CLIENT's expected version (from If-Match) into
		// UpdateMCPServer's `... AND row_version=$n` guard. The CTE (§6.2)
		// distinguishes "doesn't exist" from "exists but stale" in one
		// statement, so no precedent fetch is needed here.
		RowVersion: expected,
	}
	var updated store.MCPServer
	err = s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		updated, e = tx.UpdateMCPServer(r.Context(), m, in.SecretRef, in.TLSCARef)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "server.update",
			Target: updated.ID, Decision: "allow",
			Metadata: serverWriteAuditMetadata(updated.Name, updated.EndpointOrCommand,
				updated.SecretRef, updated.TLSCARef),
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
			// The ATTEMPTED values, not the stored ones: nothing was written.
			// A refused write is the row an investigator most wants to see for
			// an exfiltration attempt (audit A4), and "which endpoint and which
			// ref did they try to point this server at" is the whole content of
			// that answer; "name + version_mismatch" alone cannot distinguish
			// it from a benign concurrent edit. derefRefOrUnchanged renders a
			// nil ref (the request never mentioned it) as "(unchanged)" rather
			// than "" — the two are different attempts (see its own comment).
			denyMeta := serverWriteAuditMetadata(in.Name, in.EndpointOrCommand,
				derefRefOrUnchanged(in.SecretRef), derefRefOrUnchanged(in.TLSCARef))
			denyMeta["reason"] = "version_mismatch"
			if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
				TenantID: rc.TenantID, Actor: p.Subject, Action: "server.update",
				Target: id, Decision: "deny",
				Metadata: denyMeta,
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
