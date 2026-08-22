package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// artifactInput is the admin write DTO for artifact create/update.
type artifactInput struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope"`
	MemorySeed  string `json:"memorySeed"`
	Version     string `json:"version"`
	Visibility  string `json:"visibility"` // "" defaults to org
}

// artifactDTO is the admin read projection for an artifact. Content is included
// (no secrets exist on artifacts, and admins need to edit the content).
type artifactDTO struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope"`
	MemorySeed  string `json:"memorySeed"`
	Version     string `json:"version"`
	Visibility  string `json:"visibility"`

	ApprovalState       string          `json:"approvalState"`
	Approved            bool            `json:"approved"` // a live approved snapshot exists
	ApprovedContent     string          `json:"approvedContent,omitempty"`
	ApprovedMemoryScope string          `json:"approvedMemoryScope,omitempty"`
	ApprovedMemorySeed  string          `json:"approvedMemorySeed,omitempty"`
	SubmittedBy         string          `json:"submittedBy,omitempty"`
	ApprovedBy          string          `json:"approvedBy,omitempty"`
	RejectReason        string          `json:"rejectReason,omitempty"`
	ScanFindings        json.RawMessage `json:"scanFindings,omitempty"`
	// RowVersion is the optimistic-concurrency token (spec §4): the value a
	// client must echo back in If-Match to update this row. artifactDTO is
	// shared by every artifact endpoint (get, create, update, submit,
	// approve, reject, withdraw, rollback), so adding it here covers all of
	// them via toArtifactDTO.
	RowVersion int64 `json:"rowVersion"`

	// RoleGrants is the per-role grant list attached to this artifact
	// (store.ArtifactRoleGrants). Present on the two single-artifact routes
	// that can afford to derive it, GET /v1/admin/artifacts/{id} and PUT of
	// the same, and absent everywhere else, which is why it is a pointer.
	//
	// It is deliberately NOT on the list route. A per-row grant count needs a
	// query per artifact, so a 100-row page would pay 100 of them, and the
	// list projection is slim BY DESIGN (see handleListArtifacts). The list
	// row is not where this number is needed either: it is needed on the edit
	// form, which already fetches its artifact by id.
	//
	// Absent therefore means "not derived on this route", not "zero grants".
	// On the routes that do derive it, it is always emitted, count 0 and an
	// empty roles array included, so a client reading it there never has to
	// tell missing from zero (the same rule roleDeleteResponse states for its
	// counts).
	RoleGrants *artifactRoleGrantsDTO `json:"roleGrants,omitempty"`
}

// artifactRoleGrantsDTO is the read projection of store.ArtifactRoleGrants:
// how many roles hold a grant on this artifact, and which (capped at
// store's maxGrantNames, with truncated saying whether the cap bit).
//
// These grants are LIVE while the artifact's visibility is "role" and DORMANT
// while it is "org": an org artifact ships to everyone through the Channel-1
// plugin and its artifact_entitlement rows are simply not consulted. Flipping
// back to "role" revives all of them at once. The field carries the same
// numbers either way, and it is the visibility alongside it that says which of
// the two an admin is looking at.
type artifactRoleGrantsDTO struct {
	Count     int      `json:"count"`
	Roles     []string `json:"roles"`
	Truncated bool     `json:"truncated"`
}

func toArtifactRoleGrantsDTO(g store.ArtifactRoleGrants) *artifactRoleGrantsDTO {
	return &artifactRoleGrantsDTO{Count: g.Count, Roles: g.RoleNames, Truncated: g.Truncated}
}

func toArtifactDTO(a store.Artifact) artifactDTO {
	return artifactDTO{
		ID: a.ID, Type: a.Type, Name: a.Name, Description: a.Description,
		Content: a.Content, MemoryScope: a.MemoryScope, MemorySeed: a.MemorySeed,
		Version: a.Version, Visibility: a.Visibility,
		ApprovalState: a.ApprovalState,
		RowVersion:    a.RowVersion,
		// Approved reports the real has_approved column (approved_content IS
		// NOT NULL), NOT a.ApprovedContent != "" (spec correction C2). The
		// list's slim projection (handleListArtifacts, below) omits
		// approved_content entirely — deriving Approved from it would report
		// every listed artifact as unapproved, silently vanishing the
		// portal's Live badge. has_approved is selected in BOTH the slim and
		// full projections for exactly this reason.
		Approved:            a.HasApproved,
		ApprovedContent:     a.ApprovedContent,
		ApprovedMemoryScope: a.ApprovedMemoryScope,
		ApprovedMemorySeed:  a.ApprovedMemorySeed, SubmittedBy: a.SubmittedBy,
		ApprovedBy: a.ApprovedBy, RejectReason: a.RejectReason, ScanFindings: a.ScanFindings,
	}
}

// artifactRevisionDTO and toArtifactRevisionDTO moved to
// admin_artifact_review.ee.go: revision history is Enterprise-only, and its
// only caller (handleListArtifactRevisions) already lives there
// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).

// publishStatusDTO is the read projection of store.PublishState.
type publishStatusDTO struct {
	LastAttemptAt *time.Time `json:"lastAttemptAt"`
	LastSuccessAt *time.Time `json:"lastSuccessAt"`
	LastCommit    string     `json:"lastCommit"`
	LastError     string     `json:"lastError"`
}

func toPublishStatusDTO(ps store.PublishState) publishStatusDTO {
	return publishStatusDTO{
		LastAttemptAt: ps.LastAttemptAt,
		LastSuccessAt: ps.LastSuccessAt,
		LastCommit:    ps.LastCommit,
		LastError:     ps.LastError,
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// The hard caps on artifact content/memorySeed (rejected with 400 at
// create/update time, audit B3) are the SAME ceilings govern.Scanner warns at —
// the scanner only WARNS (informing the human approver), so without this reject
// an oversized payload was silently accepted, stored verbatim, and copied into
// every revision, audit export, and both distribution channels. One source of
// truth: reference govern's exported constants directly so the two can never
// drift.

func validateArtifact(in artifactInput) error {
	if in.Type != "skill" && in.Type != "subagent" && in.Type != "rule" {
		return validationError{"type must be skill, subagent, or rule"}
	}
	if !slugRe.MatchString(in.Name) {
		return validationError{"name must be a slug (lowercase, digits, dashes)"}
	}
	if in.Visibility != "" && in.Visibility != "org" && in.Visibility != "role" {
		return validationError{"visibility must be org or role"}
	}
	if len(in.Content) > govern.MaxContentBytes {
		return validationError{"content exceeds the 64KiB limit"}
	}
	if in.MemoryScope != "" {
		if in.Type != "subagent" {
			return validationError{"memoryScope is only valid for subagents"}
		}
		if in.MemoryScope != "user" && in.MemoryScope != "project" && in.MemoryScope != "local" {
			return validationError{"memoryScope must be user, project, or local"}
		}
	}
	if in.MemorySeed != "" {
		if in.Type != "subagent" {
			return validationError{"memorySeed is only valid for subagents"}
		}
		if in.MemoryScope != "user" && in.MemoryScope != "project" {
			return validationError{"memorySeed requires memoryScope user or project"}
		}
		if len(in.MemorySeed) > govern.MaxSeedBytes {
			return validationError{"memorySeed exceeds the 16KiB limit"}
		}
		// The sync client's managed block is delimited by ORBEAT-SEED sentinels;
		// a seed containing that text would corrupt the merge on the client.
		if strings.Contains(in.MemorySeed, "ORBEAT-SEED") {
			return validationError{"memorySeed must not contain the ORBEAT-SEED sentinel"}
		}
	}
	// A rule's content is plain markdown instruction text (name/description are
	// separate artifact fields), so it must NOT carry — nor be validated for —
	// YAML frontmatter. Skills/subagents are rendered into files that require it.
	if in.Type == "rule" {
		if strings.TrimSpace(in.Content) == "" {
			return validationError{"rule content must not be empty"}
		}
	} else if err := marketplace.ValidateArtifactContent(in.Content); err != nil {
		return validationError{err.Error()}
	}
	return nil
}

// communityAutoApproveActor is the approved_by value maybeAutoApprove records
// (spec §2): a SYSTEM actor, deliberately not the creating/editing admin's
// subject. Recording a human as the approver of content nobody actually
// reviewed would put a false name in an append-only governance record.
// "system:" mirrors the prefix migration 00006 already uses for its own
// backfill actor (system:migration).
const communityAutoApproveActor = "system:auto-approve"

// maybeAutoApprove runs after handleCreateArtifact/handleUpdateArtifact write
// an artifact's working copy, inside the SAME auditedTx as that write (tx is
// the open transaction handle, not a fresh one), so the extra approval write
// is atomic with the create/update it follows. It is a no-op unless
// s.autoApprove is set (Community only, autoapprove.ee.go/autoapprove.community.go).
//
// current.ID is used rather than a separate id parameter: both call sites
// already have the just-written row in hand (CreateArtifact's return value,
// UpdateArtifact's return value), so there is nothing to look up.
//
// Reuses SetArtifactApproved unchanged rather than inventing a second
// approval path (spec §2: "one state machine, no upgrade migration"). keep
// is s.revisionKeep, the SAME configured prune cap handleApproveArtifact
// passes: cmd/api wires it unconditionally in both editions from
// cfg.ArtifactRevisionKeepN(), so there is no separate Community knob to
// invent, and none is needed: a fresh create has exactly one revision
// regardless of keep, and a re-auto-approved edit grows the history the same
// way a real approval would. The revision this appends to artifact_revision
// is unreachable by any Community reader (the read side,
// artifact_revision.ee.go, is Enterprise-only) but is not wasted:
// artifact_revision.go's own doc comment already anticipates this exact
// call, and the row is what makes an artifact's history coherent if the
// tenant ever moves to Enterprise, with no upgrade migration required.
func (s *Server) maybeAutoApprove(ctx context.Context, tx *store.Store, tenantID string, current store.Artifact) (store.Artifact, error) {
	if !s.autoApprove {
		return current, nil
	}
	approved, _, err := tx.SetArtifactApproved(ctx, tenantID, current.ID, communityAutoApproveActor, s.revisionKeep)
	if err != nil {
		return store.Artifact{}, err
	}
	return approved, nil
}

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in artifactInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if err := validateArtifact(in); err != nil {
		fail(w, err)
		return
	}
	a := store.Artifact{
		TenantID: rc.TenantID, Type: in.Type, Name: in.Name, Description: in.Description,
		Content: in.Content, MemoryScope: in.MemoryScope, MemorySeed: in.MemorySeed,
		Version: in.Version, Visibility: in.Visibility,
	}
	var created store.Artifact
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		created, e = tx.CreateArtifact(r.Context(), a)
		if e != nil {
			return store.AuditEvent{}, e
		}
		created, e = s.maybeAutoApprove(r.Context(), tx, rc.TenantID, created)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact.create",
			Target: created.ID, Decision: "allow",
			Metadata: map[string]any{"name": created.Name},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.publisher.Enqueue()
	writeJSON(w, http.StatusCreated, toArtifactDTO(created))
}

// handleListArtifacts is keyset-paginated (?limit, ?cursor; see paging.go)
// and slim by default: the four heavy payload columns
// (content/memorySeed/approvedContent/approvedMemorySeed) are omitted unless
// ?include=content is set (BREAKING — a client that needs them, e.g. the
// portal's Review queue and edit form, must now request ?include=content;
// see paging_test.go's TestArtifactListSlimByDefault). Cursor shape is
// {cursorText, cursorText}: artifacts sort on (type, name), two keys, unlike
// the single-key lists in Task 7.
//
// ?state filters IN SQL via ArtifactPageOpts.State, not a Go loop applied
// after the page query — with the filter applied after LIMIT, a page could
// come back short (or empty) while more matches existed (spec §4.1;
// TestArtifactListStateFilterFullPage). An unknown ?state (e.g. "bogus")
// matches nothing in SQL, exactly as the prior Go-loop version matched
// nothing in memory — an empty 200 either way. Left lenient deliberately:
// ?state is pre-existing (clients may already send values this handler used
// to silently no-op on), so rejecting an unrecognized one now would be a
// SECOND breaking change on top of this task's slimming — deferred, not
// forgotten.
//
// ?include is the opposite case and IS validated: it is introduced by this
// very commit, so no client depends on any particular handling of a bad
// value yet, and rejecting one is not a breaking change. This also matches
// the actual house convention (verified against every other admin-list-style
// query param in this package, not assumed): default the absent value,
// reject an unrecognized one — see ?format on GET /v1/admin/audit/export
// (admin_audit.go: absent → "json", "json"/"csv" accepted, anything else →
// 400) and parseAuditBound's ?from/?to. Left lenient, "?include=Content"
// (or any other typo) would silently return blank content with a 200 — the
// same silent-blank-content failure shape as the C8 portal regressions this
// task's commit message documents, just triggered by the API caller instead
// of by an as-yet-unmigrated portal fetch.
//
// The nextCursor heuristic (len(rows)==limit means "possibly more"; an exact
// multiple of limit costs one extra empty page) is documented once, on
// handleListRoles.
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText, cursorText})
	if err != nil {
		fail(w, err)
		return
	}
	include := r.URL.Query().Get("include")
	if include != "" && include != "content" {
		fail(w, validationError{`include must be "content"`})
		return
	}
	artifacts, err := s.store.ListArtifactsPage(r.Context(), rc.TenantID, store.ArtifactPageOpts{
		State:          r.URL.Query().Get("state"),
		Cursor:         cursor,
		Limit:          limit,
		IncludeContent: include == "content",
	})
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]artifactDTO, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, toArtifactDTO(a))
	}
	next := ""
	if len(artifacts) == limit && limit > 0 {
		next = encodeListCursor(store.ArtifactCursor(artifacts[len(artifacts)-1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out, "limit": limit, "nextCursor": next})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	// A cross-tenant id is invisible: store.GetArtifact filters by tenant in
	// SQL, so ErrNotFound covers both "doesn't exist" and "belongs to another
	// tenant".
	a, err := s.store.GetArtifact(r.Context(), rc.TenantID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	// The per-role grants ride along on this route because this is the read
	// the portal's edit form already makes (useAdminArtifact), and the form is
	// where the number has to be on screen: an admin switching visibility from
	// org back to role revives every dormant grant at once, and that is the
	// last moment before it happens. Counting client-side from
	// GET /v1/admin/artifact-entitlements instead would be wrong on exactly
	// the artifacts that matter most, because that list is capped (100 rows by
	// default) and an undercount there understates the blast radius.
	//
	// Unlocked (ArtifactRoleGrants, not ...ForUpdate): nothing is being
	// mutated, and a read that took row locks would make opening an edit form
	// block behind unrelated grant writes.
	grants, err := s.store.ArtifactRoleGrants(r.Context(), rc.TenantID, a.ID)
	if err != nil {
		fail(w, err)
		return
	}
	dto := toArtifactDTO(a)
	dto.RoleGrants = toArtifactRoleGrantsDTO(grants)
	// The by-id read is where a client obtains the ETag it will later echo
	// back as If-Match (spec §4: this route reads no query params, so it has
	// exactly one representation and a strong ETag is safe here). Mirrors
	// handleGetServer.
	w.Header().Set("ETag", etag(a.RowVersion))
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleUpdateArtifact(w http.ResponseWriter, r *http.Request) {
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
	var in artifactInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if err := validateArtifact(in); err != nil {
		fail(w, err)
		return
	}
	// Effective visibility mirrors the store default (empty → "org") so a
	// full-replace that omits visibility is not mistaken for an identity change.
	effVis := in.Visibility
	if effVis == "" {
		effVis = "org"
	}
	a := store.Artifact{
		ID: id, TenantID: rc.TenantID, Type: in.Type, Name: in.Name, Description: in.Description,
		Content: in.Content, MemoryScope: in.MemoryScope, MemorySeed: in.MemorySeed,
		Version: in.Version, Visibility: in.Visibility,
	}
	var updated store.Artifact
	var grants store.ArtifactRoleGrants
	err = s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		current, e := tx.GetArtifactForUpdate(r.Context(), rc.TenantID, id)
		if e != nil {
			return store.AuditEvent{}, e
		}
		// The decisive precondition check (spec §6.1): expected is the
		// CLIENT's If-Match, current.RowVersion is the just-locked read.
		// GetArtifactForUpdate's FOR UPDATE lock means current cannot go
		// stale before UpdateArtifact runs below, so there is no TOCTOU
		// window despite the comparison happening here in Go rather than in
		// the UPDATE's WHERE clause. Checked before the identity lock so a
		// stale client is rejected on the precondition it actually violated,
		// not on a business rule evaluated against data it never saw.
		if current.RowVersion != expected {
			return store.AuditEvent{}, store.ErrVersionMismatch
		}
		// Identity lock: while a live approved snapshot exists, name/type/visibility
		// are frozen — changing them would desync the distributed row from its
		// approved snapshot. Content/seed edits are always allowed (they re-dirty
		// to draft without touching the snapshot). current.HasApproved, not
		// current.ApprovedContent != "" — the same C2 derivation toArtifactDTO
		// stopped using; GetArtifactForUpdate always selects the full artifactCols
		// projection, so both forms agree today, but has_approved is the honest
		// signal (a defect fixed in one place must be grepped for in its siblings).
		//
		// Gated on !s.autoApprove, deliberately NOT on the edition or a build
		// tag. The desync the lock prevents needs identity and snapshot to be
		// writable at different TIMES, and s.autoApprove is exactly the
		// condition under which they are not: tx.UpdateArtifact and
		// s.maybeAutoApprove both run below inside this one auditedTx closure,
		// so the new name/type/visibility and the snapshot of the new content
		// commit together or not at all, and no reader can observe one without
		// the other. An edition check would lift the lock on a fact that merely
		// coincides with that invariant today; this predicate re-arms the lock
		// by itself the moment auto-approve is off, and stays lifted if
		// auto-approve is ever enabled outside Community.
		//
		// It has to lift somewhere, because the documented unlock is withdraw
		// (handleWithdrawArtifact, admin_artifact_review.ee.go), which is
		// Enterprise-only and so reaches no generated Community tree. Under
		// auto-approve HasApproved is true from creation and stays true through
		// every edit, so without this term a Community admin could never rename
		// an artifact, change its type or change its visibility at all: delete
		// and recreate was the only way out, and it loses the id, the
		// entitlements and the revision history.
		if !s.autoApprove && current.HasApproved &&
			(in.Name != current.Name || in.Type != current.Type || effVis != current.Visibility) {
			return store.AuditEvent{}, validationError{"name, type, and visibility are locked while an approved version is live; withdraw it first"}
		}
		// current.RowVersion is a read-then-compare backstop passed straight
		// through: GetArtifactForUpdate's FOR UPDATE lock means current cannot
		// go stale before this call runs, so this always matches (it was just
		// verified above). The predicate on UpdateArtifact's own UPDATE
		// statement exists for any OTHER caller that does not hold the lock.
		// Read INSIDE this transaction and AFTER GetArtifactForUpdate's lock,
		// never before it. That ordering is the whole difference between a
		// record of what this update did to those grants and a racy
		// before-picture, and it is v1.24.0's DeleteRole fix applied to the
		// artifact row instead of the role row (design doc
		// docs/specs/2026-08-11-orbeat-role-deletion-design.md §5, which this
		// repo falsified with a reproduced race rather than reasoning about):
		// handleCreateArtifactEntitlement holds a FOR SHARE lock on this very
		// artifact row across its INSERT, so a read taken before the FOR
		// UPDATE lock could complete, the update could then block behind that
		// inserter, and the inserter could commit one more grant while it
		// waited. Position relative to tx.UpdateArtifact below is immaterial
		// (nothing can change these rows while the lock is held); position
		// relative to the LOCK is everything. ...ForUpdate closes the
		// symmetric revoke race; see its doc comment for what it does and
		// does not cover.
		grants, e = tx.ArtifactRoleGrantsForUpdate(r.Context(), rc.TenantID, id)
		if e != nil {
			return store.AuditEvent{}, e
		}
		updated, e = tx.UpdateArtifact(r.Context(), a, current.RowVersion)
		if e != nil {
			return store.AuditEvent{}, e
		}
		// A Community edit must re-approve, or the edit would never take
		// effect: UpdateArtifact always resets approval_state to 'draft'
		// (comment above), and the generated Community tree has no submit/
		// approve workflow to move it back; the approved snapshot would
		// stay frozen at whatever it was before this edit, forever, since
		// there is no route to change it. maybeAutoApprove is a no-op in
		// Enterprise, where the real workflow owns that transition.
		updated, e = s.maybeAutoApprove(r.Context(), tx, rc.TenantID, updated)
		if e != nil {
			return store.AuditEvent{}, e
		}
		md := map[string]any{"name": updated.Name}
		// A visibility flip is the one edit that silently changes who receives
		// this artifact WITHOUT touching a single entitlement row. role -> org
		// leaves every per-role grant in place but stops consulting it
		// (store.ListEntitledArtifacts filters on visibility='role'), and
		// org -> role brings all of them back at once, with nobody having
		// re-granted anything. The grants are deliberately NOT deleted, since
		// that retention is what makes a mistaken flip recoverable, so the
		// protection here is the same one v1.24.0 chose for role deletion:
		// legibility, not prevention. These keys are what lets an operator
		// answer "why did alice get this again?" months later.
		if effVis != current.Visibility {
			md["visibilityFrom"] = current.Visibility
			md["visibilityTo"] = effVis
			md["roleGrantsAffected"] = grants.Count
			md["roleGrantsEffect"] = grantsEffect(effVis)
			md["roles"] = grants.RoleNames
			md["truncated"] = grants.Truncated
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact.update",
			Target: updated.ID, Decision: "allow",
			Metadata: md,
		}, nil
	})
	if err != nil {
		// A stale If-Match is a rejected mutation and, under the fail-closed
		// audit invariant (v1.17.0 finding B1: "deny decisions were never
		// audited"), must leave a durable trace before the client sees the
		// 412 — mirrors admin_servers.go's handleUpdateServer exactly,
		// including its precedent: if the audit write itself fails, the
		// caller gets 500, not a silent 412. A 428 (missing/refused
		// If-Match) is a client bug, not a security event, and is
		// deliberately NOT audited (spec §9).
		if errors.Is(err, store.ErrVersionMismatch) {
			if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
				TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact.update",
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
	s.publisher.Enqueue()
	// Same field as the by-id read, same meaning, and still accurate: the
	// update statement cannot add or remove a grant, and the transaction held
	// its locks from the read to the commit.
	dto := toArtifactDTO(updated)
	dto.RoleGrants = toArtifactRoleGrantsDTO(grants)
	writeJSON(w, http.StatusOK, dto)
}

// grantsEffect names what a visibility flip TO newVisibility does to the
// artifact's existing per-role grants. Total over the two values validateArtifact
// admits (org, role), and "role" is the only one that makes grants live, so the
// default arm is org and any third value would have been rejected before
// reaching here.
func grantsEffect(newVisibility string) string {
	if newVisibility == "role" {
		return "revived"
	}
	return "dormant"
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		if e := tx.DeleteArtifact(r.Context(), rc.TenantID, id); e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact.delete",
			Target: id, Decision: "allow",
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.publisher.Enqueue()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMarketplacePublish(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	s.publisher.Enqueue()
	// Best-effort audit of the admin-triggered publish (the only admin
	// mutation-adjacent action that was previously unaudited). Availability of
	// the enqueue must not hinge on the audit write, but a drop is never silent.
	s.logBestEffortAudit(r.Context(), store.AuditEvent{
		TenantID: rc.TenantID, Actor: p.Subject, Action: "marketplace.publish",
		Decision: "allow",
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) handleMarketplaceStatus(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	ps, err := s.store.GetPublishState(r.Context(), rc.TenantID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPublishStatusDTO(ps))
}
