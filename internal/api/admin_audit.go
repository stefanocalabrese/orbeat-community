package api

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// uuidRe matches Postgres' uuid text representation (RFC 4122 form). decodeCursor
// uses it to reject a non-UUID cursor id before it reaches the uuid cast in
// store/audit.go's ListAuditEventsPage — otherwise a malformed-but-well-shaped
// cursor id (valid base64, valid "<nanos>:<id>" split) throws Postgres 22P02
// (invalid_text_representation), which fail()'s default case surfaces as a 500
// instead of the intended 400 (audit B2a).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

type auditDTO struct {
	ID       string         `json:"id"`
	TS       time.Time      `json:"ts"`
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Target   string         `json:"target"`
	Decision string         `json:"decision"`
	Metadata map[string]any `json:"metadata"`
}

func toAuditDTO(e store.AuditEvent) auditDTO {
	return auditDTO{
		ID: e.ID, TS: e.TS, Actor: e.Actor, Action: e.Action,
		Target: e.Target, Decision: e.Decision, Metadata: e.Metadata,
	}
}

// encodeCursor serializes a keyset position as base64url("<unixNano>:<id>").
func encodeCursor(ts time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ts.UnixNano(), 10) + ":" + id))
}

// decodeCursor parses an opaque cursor back into a keyset position, returning a
// validationError (→ 400) for any malformed input.
func decodeCursor(s string) (*store.AuditCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, validationError{"invalid cursor"}
	}
	nanos, id, found := strings.Cut(string(raw), ":")
	if !found || id == "" {
		return nil, validationError{"invalid cursor"}
	}
	if !uuidRe.MatchString(id) {
		return nil, validationError{"invalid cursor"}
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, validationError{"invalid cursor"}
	}
	return &store.AuditCursor{TS: time.Unix(0, n).UTC(), ID: id}, nil
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit := defaultAuditLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	var cursor *store.AuditCursor
	if c := r.URL.Query().Get("cursor"); c != "" {
		parsed, err := decodeCursor(c)
		if err != nil {
			fail(w, err)
			return
		}
		cursor = parsed
	}
	events, err := s.store.ListAuditEventsPage(r.Context(), rc.TenantID, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]auditDTO, 0, len(events))
	for _, e := range events {
		out = append(out, toAuditDTO(e))
	}
	next := ""
	if len(events) == limit && limit > 0 {
		last := events[len(events)-1]
		next = encodeCursor(last.TS, last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "limit": limit, "nextCursor": next})
}
