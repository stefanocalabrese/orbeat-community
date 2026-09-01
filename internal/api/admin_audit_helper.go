package api

import (
	"context"
	"log/slog"
	"strings"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// auditedTx runs mutate and writes an audit row in the SAME transaction
// (fail-closed auditing). After the tx COMMITS, it dual-emits the audit event
// as a structured log line for stream/SIEM ingestion — never before commit, so
// a rolled-back mutation emits nothing.
// nudgesGatewaySessions reports whether an audit action changed something a
// gateway session SNAPSHOTS at build time: its entitlements, the roles they
// hang off, or the servers it dials.
//
// Matched by PREFIX rather than by an exact list, deliberately. An exact list
// is a second place to remember, and the failure it produces is silent: a new
// `entitlement.update` handler would simply stop nudging, and nobody would
// notice for five minutes at a time. A prefix covers the family.
//
// Artifacts are absent on purpose: they are Channel-1 and Channel-2 content and
// no gateway session reads them, so nudging on an artifact change would drop
// live MCP sessions to no effect.
func nudgesGatewaySessions(action string) bool {
	for _, prefix := range []string{"entitlement.", "role.", "server."} {
		if strings.HasPrefix(action, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) auditedTx(
	ctx context.Context,
	mutate func(tx *store.Store) (store.AuditEvent, error),
) error {
	var emitted store.AuditEvent
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		ev, e := mutate(tx)
		if e != nil {
			return e
		}
		stored, e := tx.AppendAuditEvent(ctx, ev)
		if e != nil {
			return e
		}
		emitted = stored
		// The entitlement-change nudge, emitted INSIDE the transaction so
		// Postgres delivers it on commit and never on a rollback. Driven by the
		// audit action rather than by the call site, so a handler added later
		// cannot forget it: see nudgesGatewaySessions.
		//
		// A failure here is logged and swallowed. Failing an admin's write
		// because a performance hint could not be queued would be the tail
		// wagging the dog, and the gateway's five-minute rebuild is the actual
		// guarantee either way.
		if nudgesGatewaySessions(ev.Action) {
			if e := tx.NotifyEntitlementChange(ctx, ev.TenantID); e != nil {
				slog.WarnContext(ctx, "entitlement nudge not sent", "action", ev.Action, "err", e.Error())
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.emitAuditLog(ctx, emitted)
	return nil
}

// appendDenyAudit records a deny decision that could not go through auditedTx
// because the mutating transaction it belongs to has ALREADY rolled back (a
// scanner block, a separation-of-duties self-approve, …) — so it runs in its
// own implicit transaction instead. ev.Decision is forced to "deny" here so
// that's a guarantee, not a caller convention.
//
// Mirrors the gateway's audit() precedent (internal/gateway/server.go): the
// structured log line is ALWAYS emitted, even when the DB write fails, so a
// dropped write still lands in the log stream/SIEM — flagged distinguishable
// from a durable row via audit_db_write=failed, at Warn instead of Info.
// Unlike the gateway's best-effort per-call audit, callers here are
// fail-closed (see catalog.go's comment on the best-effort catalog.list
// access log): the write error is still returned so the handler 500s instead
// of returning the original deny status code (audit finding B1: deny
// decisions were never audited, let alone durably on a write failure).
//
// The DB write itself runs under context.WithoutCancel(ctx) so a client
// disconnect can't cancel the audit side-effect; context VALUES (e.g.
// request_id, read from the original ctx below) are unaffected by
// cancellation either way, and a genuine DB failure still surfaces as an
// error through the normal fail-closed path.
func (s *Server) appendDenyAudit(ctx context.Context, ev store.AuditEvent) error {
	ev.Decision = "deny"
	stored, err := s.store.AppendAuditEvent(context.WithoutCancel(ctx), ev)
	if err != nil {
		// No row was ever inserted, so there is no generated ID to log — emit
		// from the input ev and omit audit_id, matching how the gateway's
		// best-effort audit() line carries no audit_id for a failed write.
		s.logger.Warn("audit",
			"event", "audit",
			"actor", ev.Actor,
			"action", ev.Action,
			"target", ev.Target,
			"decision", ev.Decision,
			"tenant", ev.TenantID,
			"request_id", logging.RequestID(ctx),
			"metadata", ev.Metadata,
			"audit_db_write", "failed",
			"err", err.Error(),
		)
		return err
	}
	s.emitAuditLog(ctx, stored)
	return nil
}

// logBestEffortAudit records a NON-critical access/action audit event
// (catalog.list, sync.list, marketplace.publish). Availability of the underlying
// read or fire-and-forget action must NOT hinge on the audit write, so — unlike
// appendDenyAudit — a write error does NOT fail the request. But it is no longer
// silent (the old `_, _ =`): a dropped write is logged at Warn and flagged
// audit_db_write=failed for the SIEM stream, mirroring appendDenyAudit's
// failure line (audit B6). The write runs on the request context to preserve the
// original availability semantics (a client disconnect cancels it, which is fine
// for a best-effort access log).
func (s *Server) logBestEffortAudit(ctx context.Context, ev store.AuditEvent) {
	if _, err := s.store.AppendAuditEvent(ctx, ev); err != nil {
		s.logger.Warn("audit",
			"event", "audit",
			"actor", ev.Actor,
			"action", ev.Action,
			"target", ev.Target,
			"decision", ev.Decision,
			"tenant", ev.TenantID,
			"request_id", logging.RequestID(ctx),
			"metadata", ev.Metadata,
			"audit_db_write", "failed",
			"err", err.Error(),
		)
	}
}

// emitAuditLog dual-emits an already-persisted audit row as a structured
// event="audit" log line, for stream/SIEM ingestion (the Postgres audit table
// stays the durable source of truth).
//
// Uses the Server's own configured logger, not logging.LoggerFrom(ctx): that
// helper only reflects s.logger when the request passed through the full
// Requests(s.logger) middleware chain (Handler()); direct handler calls (e.g.
// unit tests) carry no context logger and would silently fall back to the
// process-wide slog.Default(), bypassing whatever s.logger is configured to.
// request_id is still attached explicitly so the audit line correlates with
// the request's http_request line either way.
func (s *Server) emitAuditLog(ctx context.Context, ev store.AuditEvent) {
	s.logger.Info("audit",
		"event", "audit",
		"actor", ev.Actor,
		"action", ev.Action,
		"target", ev.Target,
		"decision", ev.Decision,
		"tenant", ev.TenantID,
		"audit_id", ev.ID,
		"request_id", logging.RequestID(ctx),
		"metadata", ev.Metadata,
	)
}
