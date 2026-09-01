package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
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
	// TargetTags narrows a RULE to projects carrying at least one of these
	// tags. Absent or empty means every registered project, which is what a
	// rule did before migration 00024, so omitting the field is the old
	// behaviour rather than a new empty-set meaning.
	TargetTags []string `json:"targetTags"`
	// RuleScope is "project" (the default when absent) or "global". A global
	// rule lands in the developer's user-level instruction files rather than in
	// each registered project, so it is for instructions about the person
	// rather than about a repository.
	RuleScope string `json:"ruleScope"`
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
	// TargetTags is the LIVE targeting an admin is editing; ApprovedTargetTags
	// below is the one being distributed. They differ exactly while a
	// re-targeting waits for approval, which is the same pending-identity gap
	// migration 00016 created for name/type/visibility.
	TargetTags []string `json:"targetTags,omitempty"`
	RuleScope  string   `json:"ruleScope,omitempty"`

	ApprovalState       string `json:"approvalState"`
	Approved            bool   `json:"approved"` // a live approved snapshot exists
	ApprovedContent     string `json:"approvedContent,omitempty"`
	ApprovedMemoryScope string `json:"approvedMemoryScope,omitempty"`
	ApprovedMemorySeed  string `json:"approvedMemorySeed,omitempty"`

	// ApprovedType/Name/Visibility are the identity that is actually being
	// DISTRIBUTED (migration 00016): the file path on every developer machine
	// and the channel it arrives on. They differ from the live Type/Name/
	// Visibility above exactly while an identity edit is waiting for a second
	// admin to approve it, and that gap is the whole answer to "why is the
	// file on my machine still called foo".
	//
	// Present on the LIST route too, unlike ApprovedContent, which the slim
	// projection blanks: these are slug-sized (see artifactSlimCols' comment
	// for the asymmetry and why it is deliberate), so the row-level pending
	// marker works without ?include=content.
	//
	// omitempty, like ApprovedContent: all four are absent together when no
	// approved snapshot exists, which the artifact_approved_identity_complete
	// CHECK makes a real invariant rather than a convention. Absent therefore
	// means "nothing is distributed", never "distributed under an empty name".
	ApprovedType       string   `json:"approvedType,omitempty"`
	ApprovedName       string   `json:"approvedName,omitempty"`
	ApprovedVisibility string   `json:"approvedVisibility,omitempty"`
	ApprovedTargetTags []string `json:"approvedTargetTags,omitempty"`
	ApprovedRuleScope  string   `json:"approvedRuleScope,omitempty"`

	SubmittedBy  string          `json:"submittedBy,omitempty"`
	ApprovedBy   string          `json:"approvedBy,omitempty"`
	RejectReason string          `json:"rejectReason,omitempty"`
	ScanFindings json.RawMessage `json:"scanFindings,omitempty"`

	// ScanFindingsDigest, FindingsAcknowledged, FindingsAckBy and FindingsAckAt
	// expose the mandatory-acknowledgment state (docs/plans/orbeat-scan-
	// acknowledgment-2026-08-27.md) that POST .../acknowledge-findings and
	// POST .../approve both gate on. Before this, neither endpoint had any
	// wire-level way for a real client to learn the artifact's current
	// digest: both were reachable only from inside the test binary, which
	// reads the digest through direct store access. This closes that gap.
	//
	// DECISION 1, on which route carries these: every route, list included,
	// NOT gated behind ?include=content. That mechanism exists to drop the
	// four ~64KiB payload columns (content/approvedContent/memorySeed/
	// approvedMemorySeed) that made a 100-row page ~160KiB per row before
	// v1.22.0; a digest is a fixed 64 lowercase hex characters
	// (govern.Digest) and an actor id plus a timestamp are the same
	// slug/timestamp sizes as submittedBy/approvedBy right next to them,
	// never that payload class. The store layer already reached this same
	// conclusion for the same reason (artifactSlimCols' own comment,
	// internal/store/artifact.go): the review queue IS a list of pending
	// artifacts, and "does this row still need its author's acknowledgment"
	// is precisely the question a reviewer asks before opening any one of
	// them, so gating the answer behind a second ?include=content round trip
	// per row would defeat the point of a list. This is the same reasoning
	// ApprovedType/Name/Visibility above already establish for the
	// pending-identity badge, applied to the pending-acknowledgment one.
	//
	// DECISION 2, on FindingsAcknowledged's shape: a SERVER-COMPUTED boolean
	// (a.FindingsAckDigest == a.ScanFindingsDigest, computed in toArtifactDTO
	// below), not a raw echo of the stored acknowledgment digest. The store
	// keeps FindingsAckDigest as its own column deliberately, because a
	// stale value left behind by a re-scan is real historical data
	// (store.Artifact's own doc comment: "FindingsAckDigest !=
	// ScanFindingsDigest is therefore a valid, ordinary state... not a
	// violation; comparing the two is a caller's job"). But sending that raw
	// digest to a CLIENT and leaving the comparison to it would reintroduce,
	// one layer up, the exact bug this whole feature exists to prevent:
	// handleApproveArtifact already had to write this comparison once
	// (cur.FindingsAckDigest != findingsDigest) to refuse a stale approval,
	// and a client-side reimplementation of that same comparison on the read
	// path is a second place to get it wrong, with no server-side backstop,
	// on the exact surface a console renders its "waiting on the author"
	// badge from. Computing it here means the one comparison that matters is
	// made exactly once, by the code that already has to get it right.
	// FindingsAckBy/FindingsAckAt follow the same rule: populated ONLY when
	// FindingsAcknowledged is true, so a stale by/at pair left behind by a
	// re-scan can never be misread as "someone acknowledged this" the way a
	// raw non-empty digest could be.
	ScanFindingsDigest   string     `json:"scanFindingsDigest,omitempty"`
	FindingsAcknowledged bool       `json:"findingsAcknowledged"`
	FindingsAckBy        string     `json:"findingsAckBy,omitempty"`
	FindingsAckAt        *time.Time `json:"findingsAckAt,omitempty"`

	// RowVersion is the optimistic-concurrency token (spec §4): the value a
	// client must echo back in If-Match to update this row. artifactDTO is
	// shared by every artifact endpoint (get, create, update, submit,
	// approve, reject, withdraw, rollback), so adding it here covers all of
	// them via toArtifactDTO.
	RowVersion int64 `json:"rowVersion"`

	// MinRevision is the admin's minimum-revision floor: the oldest approved
	// revision any developer machine may keep being served for this artifact,
	// overriding whatever it has pinned locally. 0 means NO FLOOR. Written by
	// PUT /v1/admin/artifacts/{id}/min-revision, Enterprise-only, but READ in
	// both editions, because the column is projected by both artifactCols and
	// artifactSlimCols and a Community console showing nothing where a number
	// used to be would look like a bug rather than an edition.
	//
	// NO omitempty, and that is the opposite call from every other numeric
	// field near it. Here 0 is the meaningful value "no floor", so hiding it
	// would leave a client unable to tell "this artifact has no floor" from
	// "this server does not send floors". Contrast syncArtifactDTO.Revision,
	// where 0 is never a real revision and omitting it is correct.
	MinRevision int `json:"minRevision"`

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
// store.MaxGrantNames, with truncated saying whether the cap bit).
//
// These grants are LIVE while the artifact's APPROVED visibility is "role" and
// DORMANT while it is "org": an org artifact ships to everyone through the
// Channel-1 plugin and its artifact_entitlement rows are simply not consulted.
// Flipping back to "role" revives all of them at once, when that flip reaches
// distribution: ListEntitledArtifacts filters on approved_visibility (migration
// 00016), so the live column an admin just edited is a proposal until it is
// approved. The field carries the same numbers either way, and it is
// ApprovedVisibility alongside it that says which of the two an admin is
// looking at.
type artifactRoleGrantsDTO struct {
	Count     int      `json:"count"`
	Roles     []string `json:"roles"`
	Truncated bool     `json:"truncated"`
}

func toArtifactRoleGrantsDTO(g store.ArtifactRoleGrants) *artifactRoleGrantsDTO {
	return &artifactRoleGrantsDTO{Count: g.Count, Roles: g.RoleNames, Truncated: g.Truncated}
}

func toArtifactDTO(a store.Artifact) artifactDTO {
	dto := artifactDTO{
		ID: a.ID, Type: a.Type, Name: a.Name, Description: a.Description,
		Content: a.Content, MemoryScope: a.MemoryScope, MemorySeed: a.MemorySeed,
		Version: a.Version, Visibility: a.Visibility, TargetTags: a.TargetTags,
		RuleScope:     a.RuleScope,
		ApprovalState: a.ApprovalState,
		RowVersion:    a.RowVersion,
		// Filled on every route, list included: min_revision_num is an int
		// that BOTH artifactCols and artifactSlimCols project (see
		// artifactSlimCols' own comment for that asymmetry with
		// approved_content), so this is a real value here and never a zero
		// standing in for an unread column.
		MinRevision: a.MinRevisionNum,
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
		ApprovedMemorySeed:  a.ApprovedMemorySeed,
		ApprovedType:        a.ApprovedType,
		ApprovedName:        a.ApprovedName,
		ApprovedVisibility:  a.ApprovedVisibility,
		ApprovedTargetTags:  a.ApprovedTargetTags,
		ApprovedRuleScope:   a.ApprovedRuleScope,
		SubmittedBy:         a.SubmittedBy,
		ApprovedBy:          a.ApprovedBy, RejectReason: a.RejectReason, ScanFindings: a.ScanFindings,
		ScanFindingsDigest: a.ScanFindingsDigest,
	}
	// FindingsAcknowledged/FindingsAckBy/FindingsAckAt: see the struct's own
	// doc comment (decision 2) for why this is a server-computed comparison
	// against the CURRENT digest rather than a raw echo of the stored
	// acknowledgment. A digest of "" (no findings at all) never matches a
	// stored FindingsAckDigest, since a real digest is always 64 hex
	// characters (govern.Digest's own doc comment), so a clean artifact
	// falls through to the zero value here without a separate check.
	if a.ScanFindingsDigest != "" && a.FindingsAckDigest == a.ScanFindingsDigest {
		dto.FindingsAcknowledged = true
		dto.FindingsAckBy = a.FindingsAckBy
		dto.FindingsAckAt = a.FindingsAckAt
	}
	return dto
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
// create/update time, audit B3) are govern.MaxContentBytes and
// govern.MaxSeedBytes, referenced directly below so this reject and govern's
// declared ceiling cannot drift apart.
//
// They are NOT the sizes govern.Scanner warns at, although this comment said
// so until 2026-08-28. The scanner warns at WarnContentBytes and
// WarnSeedBytes, three quarters of these caps (48KiB of 64KiB, 12KiB of
// 16KiB), and the gap is deliberate: scanner.go's comment on those constants
// records that a warning fired at the rejection size leaves no room to act on
// it, and that on the submit path it was unreachable anyway, because
// validateArtifact runs first and 400s before the scanner is ever handed the
// content.
//
// The reject is what stops an oversized payload. The scanner only WARNS
// (informing the human approver), so before this check one was accepted,
// stored verbatim, and copied into every revision, audit export and both
// distribution channels.

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
	if err := validateTargetTags(in); err != nil {
		return err
	}
	if err := validateRuleScope(in); err != nil {
		return err
	}
	if len(in.Content) > govern.MaxContentBytes {
		return validationError{"content exceeds the 64KiB limit"}
	}
	// CONTENT carries no sentinel either, and until audit finding A15 nothing
	// checked it. The seed check below has existed since Slice B and covers
	// only ORBEAT-SEED, only in the seed; artifact CONTENT was checked for
	// neither sentinel, in either edition.
	//
	// The reproduced failure needs no malice. A rule artifact documenting
	// orbeat's own managed-block format quotes "<!-- ORBEAT-RULES:END -->".
	// renderRulesBlock (internal/syncclient/rules.go) wraps that content in a
	// block of its own, so the developer's AGENTS.md ends up with one BEGIN
	// and two ENDs; on the next run rulesMarkersHealthy sees begins=1 ends=2,
	// refuses to splice the file, and skips that project for good, with the
	// injected tail sitting OUTSIDE the block where stripRules can never
	// reach it. A rule is the artifact type most likely to be written about
	// orbeat itself, which is why this is a plausible accident rather than a
	// theoretical one.
	//
	// govern.HasReservedMarker is the scanner's own reserved-marker rule
	// (scanner.go), called rather than reimplemented: the Community edition
	// reaches the scanner only through scanArtifactWrite below, and a
	// hand-written second list here is exactly how the ORBEAT-RULES half went
	// missing in the first place.
	//
	// It matches the HTML-comment form, "<!--" then optional space then
	// ORBEAT-SEED/ORBEAT-RULES then :BEGIN or :END, which is a strict
	// superset of every literal the sync client actually looks for
	// (rulesBeginRe, rulesEndRe, seedBeginRe, seedEndRe; gated by
	// internal/syncclient.TestGovernReservedMarkerCoversEverySyncSentinel).
	// Prose that merely names the feature stays publishable, which the
	// scanner's own comment calls out as deliberate and which this reject
	// inherits.
	if govern.HasReservedMarker(in.Content) {
		return validationError{"content must not contain an orbeat managed-block sentinel (ORBEAT-SEED or ORBEAT-RULES)"}
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
		//
		// Kept as a bare substring, strictly BROADER than
		// govern.HasReservedMarker below, which needs the "<!--" wrapper. It
		// is not the drifting second copy A15 is about: narrowing it to the
		// shared regex would newly ACCEPT seeds this repo has rejected since
		// Slice B, which is a relaxation of a shipped control and nothing
		// about A15 asks for one.
		if strings.Contains(in.MemorySeed, "ORBEAT-SEED") {
			return validationError{"memorySeed must not contain the ORBEAT-SEED sentinel"}
		}
		// The other sentinel, for symmetry with the content check above: the
		// line above knows only about ORBEAT-SEED, so an ORBEAT-RULES block
		// forged inside a seed reached MEMORY.md unchallenged. It corrupts
		// nothing on the rules path (a seed is never written into an
		// AGENTS.md), but it is a governed managed block appearing on a
		// developer's disk that no orbeat writer put there, and
		// orbeat-sync doctor's orphan scan will report it forever.
		if govern.HasReservedMarker(in.MemorySeed) {
			return validationError{"memorySeed must not contain an orbeat managed-block sentinel (ORBEAT-SEED or ORBEAT-RULES)"}
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

// maxTargetTags mirrors migration 00024's arity CHECK. Both exist: the CHECK is
// the backstop for anything reaching the table another way, this is what turns
// the seventeenth tag into a 400 with a sentence instead of a 500 with a
// constraint name.
const maxTargetTags = 16

// validateTargetTags enforces the shape the database cannot. A CHECK constraint
// may not contain a subquery, so `unnest`-ing the array to regex each element is
// not expressible in SQL; arity and the rule-only restriction are, and 00024
// carries those. Element shape is therefore only ever checked here, which is why
// it is checked on the write path rather than trusted from the client.
//
// Duplicates are rejected rather than silently deduped: a repeated tag means the
// caller believes something about the set that is not true, and matching is an
// intersection test where a duplicate changes nothing, so accepting it would
// store a value whose extra element can never do anything.
func validateTargetTags(in artifactInput) error {
	if len(in.TargetTags) == 0 {
		return nil
	}
	if in.Type != "rule" {
		return validationError{"targetTags is only valid for rules"}
	}
	if len(in.TargetTags) > maxTargetTags {
		return validationError{fmt.Sprintf("targetTags accepts at most %d tags", maxTargetTags)}
	}
	seen := make(map[string]bool, len(in.TargetTags))
	for _, t := range in.TargetTags {
		if !slugRe.MatchString(t) {
			return validationError{"each targetTag must be a slug (lowercase, digits, dashes)"}
		}
		if seen[t] {
			return validationError{fmt.Sprintf("targetTags contains %q twice", t)}
		}
		seen[t] = true
	}
	return nil
}

// validateRuleScope keeps `ruleScope` to the two values migration 00025's CHECK
// allows, and refuses the one combination that is meaningless rather than
// merely unusual: a GLOBAL rule with target tags.
//
// Tags describe PROJECTS. A global rule is written to the user-level files that
// every project inherits, so there is no project for a tag to be compared
// against, and accepting the pair would store a targeting that can never be
// consulted. That is the same failure the empty-versus-null distinction avoids
// on target_tags itself: a value whose extra information can never do anything
// is worse than a rejection, because it reads as a constraint that is being
// applied.
func validateRuleScope(in artifactInput) error {
	if in.RuleScope == "" {
		return nil
	}
	if in.Type != "rule" {
		return validationError{"ruleScope is only valid for rules"}
	}
	if in.RuleScope != "project" && in.RuleScope != "global" {
		return validationError{"ruleScope must be project or global"}
	}
	if in.RuleScope == "global" && len(in.TargetTags) > 0 {
		return validationError{"a global rule cannot carry targetTags: tags select projects, and a global rule is not written into any"}
	}
	return nil
}

// artifactScanPayload is the exact surface an artifact is submitted to the
// scanner on. It exists so the payload that gets SCANNED and the payload that
// gets COMPARED under the row lock (handleSubmitArtifact,
// admin_artifact_review.ee.go) are produced by one function and can never drift
// into describing different sets of columns.
//
// That matters more than it looks. The comparison there is a struct comparison
// on govern.ArtifactPayload, not a hand-written list of columns, so adding a
// field to ArtifactPayload extends the stability guard automatically instead of
// silently leaving the new field unguarded.
//
// It lives in this shared file rather than beside its first caller because the
// generator drops every *.ee.go, so a Community tree would have had no way to
// build a scanner payload at all (audit finding A15). scanArtifactWrite below
// is its second caller, and it is the same function for both: the Enterprise
// submit scan and the Community write scan see identical fields, so a finding
// means the same thing in either edition.
func artifactScanPayload(a store.Artifact) govern.ArtifactPayload {
	return govern.ArtifactPayload{
		Type: a.Type, Name: a.Name, Content: a.Content,
		MemoryScope: a.MemoryScope, MemorySeed: a.MemorySeed,
		// Description is intentionally not scanned: it is never distributed.
	}
}

// scanArtifactWrite runs the governance scanner on an artifact that a
// create/update is about to write, and it is the answer to audit finding A15:
// the Community edition never scanned artifact content at all. Its only
// production call site was handleSubmitArtifact in admin_artifact_review.ee.go,
// a file the generator drops, so `grep 'scanner.Scan(' ` over a generated
// Community tree returned zero hits while internal/govern shipped intact and
// cmd/api still called SetScanner. The installer-wiring gate could not see it:
// SetScanner IS called, so the value is installed and then never read.
//
// IT RUNS ONLY WHERE THE WRITE PUBLISHES, which is what keeps Enterprise from
// scanning the same bytes twice. s.autoApprove is the same field
// maybeAutoApprove reads, and the condition is not "is this Community" but
// "does this write reach developers by itself": with auto-approve on, the
// create/update IS the publication, so it is the last moment any gate can act.
// With it off there is a submit step, that step scans, and its findings are
// persisted, digested and acknowledged by a human, which is a strictly richer
// treatment than this one. Enterprise therefore reaches the scanner exactly
// once, at submit, unchanged by this function.
//
// IT SCANS THE INCOMING PAYLOAD, NEVER THE STORED ROW, and that is the whole
// reason a blocking finding can safely refuse the write. The refusal is a
// statement about the bytes in this request; the row on disk is not re-judged.
// So an artifact that somehow already holds blocking content (written before
// this check existed, or restored from a backup taken then) is still editable:
// a PUT carrying clean content is accepted on its own merits, and DELETE is
// untouched. There is no input an admin can be stuck with and no artifact this
// makes unfixable. The inverse design, scanning the row after the write,
// would have made exactly those artifacts permanently unwritable.
//
// IT RUNS OUTSIDE THE TRANSACTION, before auditedTx opens one, for the reason
// handleSubmitArtifact's own comment gives at length: govern.Scanner is an
// interface, SetScanner accepts any implementation, and the shipped LLM one is
// a network call with a 120s ceiling. Holding SELECT ... FOR UPDATE on the
// artifact row across an arbitrary scanner is a defect that repo already
// removed once.
//
// Non-blocking findings are returned rather than dropped: the callers put them
// in the create/update audit row. That is the only place they can go here,
// because scan_findings and the whole acknowledgment surface are Enterprise
// (admin_artifact_review.ee.go, artifact_findings columns), and a warn nobody
// can ever read is the same as no rule.
func (s *Server) scanArtifactWrite(ctx context.Context, a store.Artifact) ([]govern.Finding, error) {
	if !s.autoApprove {
		return nil, nil
	}
	return s.scanner.Scan(ctx, artifactScanPayload(a))
}

// scanWriteOrReject wraps scanArtifactWrite with the two things both write
// handlers have to do identically with a blocking finding: audit the deny and
// answer 422. It reports ok=false when it has already written the response.
//
// The deny is fail-closed, matching handleSubmitArtifact's block path exactly:
// if the audit write itself fails the caller gets 500, never a silent 422,
// because a refused write is a governance decision and this repo's invariant
// (audit finding B1) is that a deny decision never goes unrecorded.
//
// target is the artifact id on update and empty on create, where no row exists
// yet and none ever will; audit_event.target is `text NOT NULL DEFAULT ”`
// (migration 00001), so an empty target is the schema's own way of saying "no
// row", and metadata carries the name either way.
func (s *Server) scanWriteOrReject(
	w http.ResponseWriter, r *http.Request,
	rc authz.ResolvedContext, p auth.Principal,
	action, target string, a store.Artifact,
) ([]govern.Finding, bool) {
	findings, err := s.scanArtifactWrite(r.Context(), a)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if !govern.HasBlocking(findings) {
		return findings, true
	}
	if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
		TenantID: rc.TenantID, Actor: p.Subject, Action: action,
		Target: target, Decision: "deny",
		Metadata: map[string]any{"name": a.Name, "findings": findings, "reason": "scanner_blocked"},
	}); aerr != nil {
		fail(w, aerr)
		return nil, false
	}
	writeBlocked(w, "artifact blocked by scanner", findings)
	return nil, false
}

// withFindings adds a non-empty findings slice to an audit row's metadata. The
// key is absent when the scanner had nothing to say, so an ordinary write's
// audit shape does not change, and present findings are legible in the one
// surface a Community operator has for them (there is no review queue to read
// them in).
func withFindings(md map[string]any, findings []govern.Finding) map[string]any {
	if len(findings) > 0 {
		md["findings"] = findings
	}
	return md
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
		Version: in.Version, Visibility: in.Visibility, TargetTags: in.TargetTags,
		RuleScope: in.RuleScope,
	}
	// Target is empty: under auto-approve this create is also the publication,
	// so the scan has to happen before the row exists and there is no id to
	// name in a deny.
	findings, ok := s.scanWriteOrReject(w, r, rc, p, "artifact.create", "", a)
	if !ok {
		return
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
			Metadata: withFindings(map[string]any{"name": created.Name}, findings),
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
//
// ?q= searches name, NOT type (docs/plans/orbeat-admin-search-sort-2026-08-27.md
// Task 4, see ArtifactPageOpts.Search's own comment for why the search
// column deliberately differs from artifactSortName here, unlike every other
// searchable list). Applied IN SQL via ArtifactPageOpts.Search, composing
// with ?state and the keyset cursor rather than filtering the returned page:
// the same v1.22.0-class correctness reason ?state itself is applied in SQL
// (this handler's own comment above), pinned by
// TestListArtifactsSearchComposesWithStateFilter.
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
	desc, err := sortOrderParams(r, artifactSortName)
	if err != nil {
		fail(w, err)
		return
	}
	artifacts, err := s.store.ListArtifactsPage(r.Context(), rc.TenantID, store.ArtifactPageOpts{
		State:          r.URL.Query().Get("state"),
		Cursor:         cursor,
		Limit:          limit,
		IncludeContent: include == "content",
		Desc:           desc,
		Search:         searchParam(r),
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
		next = encodeListCursor(store.ArtifactCursor(artifacts[len(artifacts)-1], desc))
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
	// full-replace that omits visibility is not recorded below as a visibility
	// flip it never made. The audit metadata block is its only reader.
	effVis := in.Visibility
	if effVis == "" {
		effVis = "org"
	}
	a := store.Artifact{
		ID: id, TenantID: rc.TenantID, Type: in.Type, Name: in.Name, Description: in.Description,
		Content: in.Content, MemoryScope: in.MemoryScope, MemorySeed: in.MemorySeed,
		Version: in.Version, Visibility: in.Visibility, TargetTags: in.TargetTags,
		RuleScope: in.RuleScope,
	}
	// Before the transaction, so no scanner implementation can be run while
	// this row is locked. It also runs before the If-Match version is compared
	// against the stored row, which costs a stale client one regex pass and no
	// database work: the alternative, reading the row first, is the shape whose
	// removal handleSubmitArtifact documents.
	findings, ok := s.scanWriteOrReject(w, r, rc, p, "artifact.update", id, a)
	if !ok {
		return
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
		// the UPDATE's WHERE clause. It is the first thing checked after the
		// read, so a stale client is rejected on the precondition it actually
		// violated rather than on anything derived from data it never saw.
		if current.RowVersion != expected {
			return store.AuditEvent{}, store.ErrVersionMismatch
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
		md := withFindings(map[string]any{"name": updated.Name}, findings)
		// A visibility flip is the one edit that silently changes who receives
		// this artifact WITHOUT touching a single entitlement row. role -> org
		// leaves every per-role grant in place but stops consulting it
		// (store.ListEntitledArtifacts filters on approved_visibility='role'),
		// and org -> role brings all of them back at once, with nobody having
		// re-granted anything.
		//
		// SINCE MIGRATION 00016 THAT FLIP IS NOT IMMEDIATE. Distribution reads
		// approved_visibility, so where auto-approve is off the grants change
		// state when the flip is APPROVED, not when it is saved. That inverted
		// the MEANING of roleGrantsEffect while leaving its name and its two
		// old values ("revived"/"dormant") reading as accomplished facts, and
		// a key whose meaning quietly inverted is worse than a missing one: an
		// operator reading "revived" would conclude alice already has the
		// artifact back.
		//
		// So the VALUE now carries the timing, which is what makes the
		// correction detectable: an assertion pinning the old value fails,
		// where a reworded doc comment would have left every test green. The
		// four values are total over (new visibility) x (has the flip reached
		// distribution), and the second term is read off the row this
		// transaction just wrote rather than off the edition, so one code path
		// stays truthful in both: under auto-approve tx.UpdateArtifact and
		// s.maybeAutoApprove share this one auditedTx, approved_visibility
		// moves with the live column, and the value is the immediate one.
		//
		// visibilityFrom/visibilityTo are untouched: they report the LIVE
		// column, which did move on this request, and they stay literally
		// true. roleGrantsAffected is the count as of THIS update; where the
		// effect is deferred a grant added before the approval lands is not in
		// it, which is the same before-picture limit any audit row carries.
		//
		// The grants are deliberately NOT deleted, since
		// that retention is what makes a mistaken flip recoverable, so the
		// protection here is the same one v1.24.0 chose for role deletion:
		// legibility, not prevention. These keys are what lets an operator
		// answer "why did alice get this again?" months later.
		if effVis != current.Visibility {
			md["visibilityFrom"] = current.Visibility
			md["visibilityTo"] = effVis
			md["roleGrantsAffected"] = grants.Count
			md["roleGrantsEffect"] = grantsEffect(effVis, updated.ApprovedVisibility)
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
// artifact's existing per-role grants, AND whether it has happened yet.
//
// newVisibility is total over the two values validateArtifact admits (org,
// role), and "role" is the only one that makes grants live, so the default arm
// is org and any third value would have been rejected before reaching here.
//
// distributedVisibility is approved_visibility as it stands AFTER this
// transaction's writes. Distribution reads that column, not the live one
// (migration 00016), so the flip has taken effect exactly when the two agree:
//
//	"revived" / "dormant"                          already in effect
//	"revives_on_approval" / "goes_dormant_on_approval"   waiting for an approval
//
// Deriving it from the row rather than from s.autoApprove is what keeps one
// code path truthful in both editions, and it stays correct in three cases an
// edition check would get wrong: an artifact with NO approved snapshot at all
// (nothing is distributed, so nothing changes until it is first approved,
// distributedVisibility ""), a flip BACK onto the visibility that is already
// being distributed (nothing pending, the immediate value is the true one),
// and any future path that promotes a snapshot outside maybeAutoApprove.
func grantsEffect(newVisibility, distributedVisibility string) string {
	inEffect := distributedVisibility == newVisibility
	if newVisibility == "role" {
		if inEffect {
			return "revived"
		}
		return "revives_on_approval"
	}
	if inEffect {
		return "dormant"
	}
	return "goes_dormant_on_approval"
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
