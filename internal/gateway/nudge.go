package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// Entitlement-change nudge.
//
// A gateway session snapshots entitlements, roles and servers at build time, so
// a revocation takes effect when the session is rebuilt: within sessionMaxAge,
// five minutes. That ceiling is the CORRECTNESS guarantee and it is unchanged
// by anything in this file. What this adds is a hint that arrives sooner.
//
// The distinction is not pedantic, it is the design constraint. Every failure
// mode here (the listener cannot connect, the database restarts, a notification
// is dropped, the payload is nonsense) degrades to exactly the behaviour the
// gateway had before the nudge existed. Nothing waits for a notification and no
// decision depends on one arriving, so there is no state in which a missing
// nudge is a security problem rather than a slower revocation.
const (
	// nudgeRetryMin and nudgeRetryMax bound the reconnect backoff. The listener
	// holds one connection; a tight retry against an unreachable database would
	// be a busy loop against something already in trouble.
	nudgeRetryMin = time.Second
	nudgeRetryMax = 30 * time.Second
)

// StartEntitlementNudge listens for entitlement-change notifications until ctx
// ends, invalidating the affected tenant's cached sessions. It blocks, so
// callers run it in a goroutine; it returns only when ctx is done.
//
// Reconnects with backoff rather than giving up: a listener that quit on the
// first database blip would leave the gateway permanently back on the
// five-minute ceiling with nothing saying so, which is the quiet-degradation
// shape this repo keeps finding in its own postmortems.
func (s *Server) StartEntitlementNudge(ctx context.Context) {
	backoff := nudgeRetryMin
	for {
		err := s.store.Listen(ctx, store.EntitlementChannel, func(tenantID string) {
			if tenantID == "" {
				return
			}
			if n := s.sessions.invalidateTenant(tenantID); n > 0 {
				s.logger.Info("entitlement change: sessions invalidated",
					"tenant", tenantID, "sessions", n)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, store.ErrNoPool) {
			// A tx-bound store can never listen. Retrying would spin forever
			// on a condition no amount of waiting fixes.
			s.logger.Warn("entitlement nudge disabled", "err", err.Error())
			return
		}
		s.logger.Warn("entitlement nudge listener dropped, reconnecting",
			"err", err.Error(), "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > nudgeRetryMax {
			backoff = nudgeRetryMax
		}
	}
}
