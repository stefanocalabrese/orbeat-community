package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// approvedIdentityTaken is fail()'s floor sentence for a collision against
// store.ApprovedIdentityUniqueIndex when no handler supplied a specific one.
// It says less than denyApprovedIdentityConflict's message but it says nothing
// false, which "already exists" does: the name is genuinely free in the live
// namespace the admin is looking at.
const approvedIdentityTaken = "another artifact already distributes under that approved type and name"

// isApprovedIdentityConflict reports whether err is a refused write against
// migration 00016's approved-identity unique index, through any amount of
// wrapping (store.transition wraps with %w, store.ApprovedIdentityConflict
// unwraps to the same *pgconn.PgError, and auditedTx returns the inner error).
func isApprovedIdentityConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == store.ApprovedIdentityUniqueIndex
}

// approvedIdentityPair returns the (type, name) the refused write was trying
// to distribute under, when the STORE attached it.
//
// Only RollbackArtifact does, and only it has to: it picks the identity out of
// a target revision or a fallback, so its caller cannot recompute what it
// chose without keeping a second copy of that choice. SetArtifactApproved
// copies the live columns the handler just read under FOR UPDATE, so there the
// handler already holds the pair and making the store re-read it before every
// UPDATE would buy one round trip per approval and nothing else.
func approvedIdentityPair(err error) (artType, name string, ok bool) {
	var ic *store.ApprovedIdentityConflict
	if errors.As(err, &ic) && ic.Type != "" && ic.Name != "" {
		return ic.Type, ic.Name, true
	}
	return "", "", false
}

// denyApprovedIdentityConflict audits a refused approve or rollback and
// returns the error the caller hands to fail(): a conflictError carrying a
// sentence naming what collided, or the audit write's own error when THAT
// failed.
//
// The audit is the fail-closed half (audit finding B1, v1.17.0). A 23505
// aborts the whole auditedTx, the audit row it was going to write included, so
// a governance decision refusing to publish something would otherwise leave no
// trace at all. appendDenyAudit runs in its own implicit transaction, which is
// why this row survives the rollback of the transaction that raised the
// conflict, and if that write fails the caller gets a 500 rather than a silent
// 409.
//
// artType/name may be empty when the writer did not attach them; the message
// and the metadata then omit the pair rather than assert an empty one. The
// holder lookup is best effort for the same reason: it runs after the failed
// transaction is gone, on the pool, so it can find nothing (the holder was
// withdrawn or deleted in between) without that changing the outcome. The pair
// is in the audit metadata either way.
func (s *Server) denyApprovedIdentityConflict(
	ctx context.Context, tenantID, actor, action, target, artifactName, artType, name string,
) error {
	msg := approvedIdentityTaken
	meta := map[string]any{"name": artifactName, "reason": "identity_conflict"}
	if artType != "" && name != "" {
		meta["conflictType"], meta["conflictName"] = artType, name
		msg = fmt.Sprintf("another artifact already distributes as %s/%s; approve its pending change, "+
			"withdraw it or delete it to free that identity", artType, name)
		if holderID, holderName, herr := s.store.ArtifactDistributingAs(ctx, tenantID, artType, name); herr == nil {
			meta["conflictsWith"], meta["conflictsWithName"] = holderID, holderName
			msg = fmt.Sprintf("artifact %q already distributes as %s/%s; approve its pending change, "+
				"withdraw it or delete it to free that identity", holderName, artType, name)
		}
	}
	if aerr := s.appendDenyAudit(ctx, store.AuditEvent{
		TenantID: tenantID, Actor: actor, Action: action, Target: target,
		Decision: "deny", Metadata: meta,
	}); aerr != nil {
		return aerr
	}
	return conflictError{msg}
}
