package api

import (
	"context"
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// syncArtifactDTO is one entitled artifact for the Channel-2 sync client. Content
// is the final, ready-to-write file body (subagent memory frontmatter already
// injected); the client writes it verbatim and derives the path from type+name.
// MemoryScope/MemorySeed are set only for user/project-scope subagents carrying
// a non-empty seed (spec §6) — the sync client uses them to seed the target
// memory file. Seed fields are omitted otherwise — "" and absent are equivalent
// everywhere (empty seed = no seed, spec §4); omitempty just keeps non-seeded
// artifacts' payloads clean.
//
// ID and Revision are UNCONDITIONAL: present in every response, in both
// editions, and independent of whether any feature that consumes them is
// enabled. A client that has to negotiate before it can parse a response is a
// client that breaks differently against each server version. They exist so a
// machine can say which version of which artifact it holds; without them it
// holds bytes and a file path and nothing that names a version.
// The last three fields are the pinning half and are CONDITIONAL, omitted
// wholesale when this build does not support pinning (pinning.community.go).
// That is the opposite treatment from ID and Revision above, and deliberately
// so: those two exist so a machine can name what it holds, which is true of
// every server; these describe a feature a Community server does not perform,
// and advertising a window to a client that cannot pin into it would be a
// promise the handler will not keep.
type syncArtifactDTO struct {
	ID          string `json:"id"`                 // artifact uuid: the stable key a rename cannot break
	Revision    int    `json:"revision,omitempty"` // artifact_revision.revision_num of this snapshot; 0 (omitted) is never a real revision
	Type        string `json:"type"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope,omitempty"` // set only alongside memorySeed (target-path selection)
	MemorySeed  string `json:"memorySeed,omitempty"`  // governed ORBEAT-SEED block body
	// TargetTags is the APPROVED targeting of a rule (migration 00024): the
	// client writes it only into registered projects carrying at least one of
	// these tags. Omitted means every registered project, which is what a rule
	// did before targeting existed, so an old client and a new server agree by
	// the field's absence rather than by a negotiation.
	//
	// It is the approved value and never the live one, for the same reason
	// Type and Name here are: re-targeting is a change to who receives the
	// rule, and this endpoint serves what was approved.
	TargetTags []string `json:"targetTags,omitempty"`
	// RuleScope is "global" on a rule that belongs in the developer's
	// user-level instruction files rather than in each registered project.
	// Omitted means project scope, which is what every rule did before scoping
	// existed, so an older client writing every rule into projects is still
	// correct for every rule an older server can describe.
	RuleScope string `json:"ruleScope,omitempty"`

	// OldestServable is spec §4.2's low = max(floor, MIN(revision_num)): the
	// oldest revision this caller can be served today. With Latest it is the
	// window `orbeat-sync pin --revision N` validates against, so a typo fails
	// at pin time rather than at the next sync.
	OldestServable int `json:"oldestServable,omitempty"`
	// Latest is MAX(revision_num), the ceiling of that window. It is
	// store.Artifact.Revision, the value the existing MAX subquery already
	// produced for the field above, and never a second computation of the
	// same number: when a pin is honoured Revision is BELOW this, which is
	// exactly the difference the client reports.
	Latest int `json:"latest,omitempty"`
	// PinOverride names why a pin was not honoured ("floor", "pruned" or
	// "ahead"), and is absent whenever it was, including when there was no
	// pin. Present-and-empty and absent would mean the same thing, so
	// omitempty costs nothing and keeps an unpinned payload clean.
	PinOverride string `json:"pinOverride,omitempty"`
}

// correctedOverride is resolveSyncPayload's per-artifact verdict AFTER the
// payload-race fallback below has had a chance to override it, i.e. what
// handleSyncArtifacts must count and report -- never pinResolution.Reason,
// the same artifact's verdict BEFORE that fallback, which stays a plain
// string for exactly this reason.
//
// The distinction is load-bearing, not decorative, and it is a review
// finding rather than the original design: a first version of this
// extraction (commit e82fd2a) passed both values into syncListAuditCounts.add
// as plain strings, and a mutant at THAT call site --
// `counts.add(hasPin, res[i].Reason, res[i].Reason, name)`, substituting the
// uncorrected reason for the corrected override -- compiled and passed the
// whole package suite, because two same-typed strings are interchangeable to
// the compiler no matter which one the code means. Giving the corrected
// value its own defined type closes that: a string is not assignable to a
// correctedOverride parameter without an explicit conversion (Go
// assignability, https://go.dev/ref/spec#Assignability -- both are named
// types, so no implicit conversion exists between them), so that exact
// mutant is now a COMPILE ERROR --
// "cannot use res[i].Reason (variable of type string) as correctedOverride
// value in argument to counts.add" -- not a test failure. A determined mutant
// could still wrap the wrong value in an explicit correctedOverride(...)
// conversion, which this type alone cannot stop; that conversion is
// conspicuous in a diff in a way the original one-line swap was not, which is
// the honest limit of what a type distinction buys here.
// TestCorrectedOverrideIsADistinctTypeFromString (sync_audit_count_test.go)
// pins the property this protection depends on: correctedOverride must stay
// a genuinely distinct defined type, never a type ALIAS for string, which
// would silently make the two interchangeable again.
type correctedOverride string

// resolveSyncPayload decides what content, memory seed, revision and
// pin-override reason handleSyncArtifacts serves for ONE entitled artifact,
// given the clamp's resolution (res) and the batched payload read
// (payloads), both already computed by the caller. Extracted to a named,
// directly callable function so the payload-race fallback below (the
// missing-key branch) is testable without reproducing the race itself:
// open-points.md records that no single-threaded test can land a real
// approval between the two queries handleSyncArtifacts runs, but a test can
// build `payloads` by hand with the key missing, which is the exact,
// complete observable symptom of that race on the payload-read side, the
// same values this function would receive in production. Nothing here is
// mocked or injected; arts/res/payloads are the identical concrete values
// the real handler already holds.
//
// tenantID/actor are threaded through only for the fallback's log line
// (recordPinnedPayloadRaceFallback below): this function performs no I/O of
// its own.
func (s *Server) resolveSyncPayload(
	ctx context.Context, tenantID, actor string, a store.Artifact, res pinResolution,
	payloads map[store.ArtifactRevisionKey]store.ArtifactRevisionPayload,
) (src marketplace.Artifact, seed string, revision int, override correctedOverride) {
	// Identity, content and the seed all come from the same place, never
	// half from one and half from the other: serving revision 3's bytes
	// under revision 9's name would put a file on disk at a path that was
	// never approved in that shape, since orbeat-sync derives every local
	// path from type plus name.
	src = marketplace.Artifact{Type: a.Type, Name: a.Name, Content: a.Content, MemoryScope: a.MemoryScope}
	// res.Reason is the ONLY place a plain string legitimately becomes a
	// correctedOverride, and the conversion is explicit and visible right
	// here: everywhere else, override is either left alone (honoured/found)
	// or set from the untyped pinOverridePruned constant (fallback below).
	seed, revision, override = a.MemorySeed, a.Revision, correctedOverride(res.Reason)
	if res.Served < a.Revision {
		if pl, found := payloads[(store.ArtifactRevisionKey{ArtifactID: a.ID, RevisionNum: res.Served})]; found {
			src = marketplace.Artifact{Type: pl.Type, Name: pl.Name, Content: pl.Content, MemoryScope: pl.MemoryScope}
			seed, revision = pl.MemorySeed, res.Served
		} else {
			// The window was read by one query and the payload by another, so
			// an approval landing between them can prune the revision this
			// clamp just resolved. Fall back to the approved snapshot and say
			// pruned, which is what happened. Never leave revision naming
			// bytes it did not come from.
			s.recordPinnedPayloadRaceFallback(ctx, tenantID, actor, a.ID, a.Name, res.Served, a.Revision)
			override = pinOverridePruned
		}
	}
	return src, seed, revision, override
}

// recordPinnedPayloadRaceFallback logs and counts one occurrence of the
// payload-race fallback resolveSyncPayload takes above. Split out from that
// function so a test can drive exactly this side effect (open-points.md's
// gate) and so the log line and the counter can never drift apart into two
// call sites that disagree about when the fallback fired.
//
// resolvedRevision is what the clamp decided to serve (the key the payload
// read did not find); servedRevision is what actually went out instead, the
// artifact's current approved revision (a.Revision), since the fallback
// always degrades to the approved snapshot. Neither the artifact's content
// nor its memory seed is logged: only identifiers and revision numbers,
// nothing that needs redacting from a log stream.
func (s *Server) recordPinnedPayloadRaceFallback(
	ctx context.Context, tenantID, actor, artifactID, artifactName string, resolvedRevision, servedRevision int,
) {
	s.logger.Warn("pinned payload pruned before it could be served",
		"event", "sync.pin_payload_race",
		"tenant", tenantID,
		"actor", actor,
		"artifact_id", artifactID,
		"artifact", artifactName,
		"resolvedRevision", resolvedRevision,
		"servedRevision", servedRevision,
		"reason", pinOverridePruned,
		"request_id", logging.RequestID(ctx),
	)
	s.metrics.PinnedPayloadRaceFallback.Add(ctx, 1)
}

// syncListAuditCounts accumulates the sync.list audit event's
// pinned/overridden/overriddenArtifacts/truncated fields across one request's
// entitled artifacts. add is called once per artifact inside
// handleSyncArtifacts's loop below, the same one-artifact-at-a-time shape
// resolveSyncPayload above already models (commit 31afadd): a plain method
// taking values the caller already holds, no interface seam added.
//
// Extracted for open-points.md's "sync.list audit-count mutant" row.
// handleSyncArtifacts's counting used to be inline in that loop, and a
// mutant that counted from res[i].Reason (an artifact's clamp verdict
// BEFORE the payload-race fallback) instead of override (resolveSyncPayload's
// CORRECTED verdict, that fallback already folded in) survived the whole
// package suite: the two values disagree only when the fallback fires, an
// approval pruning the exact revision the window read resolved between it and
// the batched payload read, and that race has no single-threaded reproduction
// through the live handler.
//
// THE REAL PROTECTION IS override's TYPE, correctedOverride, not this
// method's own logic. A first version of this extraction gave add two plain
// string parameters, reason and override, reasoning that a test could hand
// it a fixture where they disagree BY HAND -- the same trick
// resolveSyncPayload's own tests use for the fallback itself. Review found
// that reasoning incomplete: it gated add's INSIDE (does add read override
// correctly, given override) but not the CALL SITE (does the caller hand add
// the corrected override rather than the raw reason), and a mutant AT the
// call site, `counts.add(hasPin, res[i].Reason, res[i].Reason, name)`,
// compiled and passed the whole suite, since two same-typed strings are
// interchangeable to the compiler no matter which the code means. Giving
// override its own type (see correctedOverride's doc comment above) closes
// that call site: the same mutant is now a compile error, not merely an
// untested one.
type syncListAuditCounts struct {
	pinned              int
	overridden          int
	overriddenArtifacts []string
	truncated           bool
}

// add folds one entitled artifact's contribution into the running counts.
//
// hasPin is pins[a.ID] > 0, the same "<= 0 means absent" sentinel pinResolve
// itself uses: it counts every entitled artifact the caller supplied a ?pin
// for, regardless of whether the clamp honoured it.
//
// reason is that artifact's res[i].Reason, kept on the signature as a plain
// string (deliberately NOT correctedOverride) so a test can still hand add a
// fixture where it disagrees with override, the same shape
// TestSyncListAuditCountUsesTheCorrectedOverride and
// TestSyncListAuditCountAgreeingFixtureWouldBeVacuous already exercise; it is
// not what stops the call-site mutant above, override's type is, and add's
// own body below never reads reason for anything. override is
// resolveSyncPayload's correctedOverride return value for the SAME artifact.
// overridden, overriddenArtifacts and truncated are computed from override
// and MUST NEVER read reason instead.
//
// hasPin and override are independent: an artifact that is both pinned and
// overridden increments pinned (because hasPin) and overridden (because
// override != "") exactly once each, never overridden a second time into
// pinned. Folding the overridden branch into pinned too was a real defect an
// earlier review caught at the handler level (see
// TestSyncListAuditReportsPinnedAndOverriddenExactly's doc comment) and
// TestSyncListAuditCountDoesNotDoubleCountOverriddenIntoPinned red-proves it
// again here.
//
// overriddenArtifacts caps at store.MaxGrantNames names; truncated says
// whether that cap bit. overridden itself is never capped -- the count stays
// exact even once the name list stops growing.
func (c *syncListAuditCounts) add(hasPin bool, reason string, override correctedOverride, name string) {
	// reason is unused by correct code below -- see the doc comment above for
	// why it stays on the signature anyway rather than being dropped.
	if hasPin {
		c.pinned++
	}
	if override != "" {
		c.overridden++
		if len(c.overriddenArtifacts) < store.MaxGrantNames {
			c.overriddenArtifacts = append(c.overriddenArtifacts, name)
		} else {
			c.truncated = true
		}
	}
}

// handleSyncArtifacts returns the caller's entitled role-visibility artifacts
// PLUS every approved org-visibility rule, rendered to final file content.
// RBAC-filtered server-side by the caller's roles.
//
// Org-visibility artifacts of every OTHER type are still never returned here:
// they ship via the Channel-1 plugin, and returning them would install them
// twice. `rule` is the exception because it has no Channel 1 to ship through
// (marketplace.RenderArtifactsPlugin's type switch has no case for it), so an
// org rule used to reach nobody at all, silently, starting from the default
// value of `visibility`.
//
// ?pin=<artifactId>:<revisionNum> is repeatable and serves an OLDER approved
// revision to this caller alone (docs/specs/2026-08-22-orbeat-artifact-version-
// pinning-design.md). It is clamped, never obeyed: pinResolve raises it to the
// artifact's admin floor and to the oldest revision the prune left, and lowers
// it to the newest that exists.
//
// THIS HANDLER IS SHARED ACROSS EDITIONS and pinning is Enterprise-only, so
// the whole pin path is gated on s.pinning AT RUNTIME rather than by a dropped
// file. A Community build compiles every line below and runs none of the
// pinning ones. That is why the parse is inside the if: on a Community server
// a malformed ?pin is not a 400, it is nothing at all.
//
// ENTITLEMENT AND VISIBILITY ARE READ LIVE, and a pin never touches either
// (spec §5.3, a security boundary rather than a preference). The entitled set
// comes from ListEntitledArtifacts before any pin is looked at, so a revoked
// grant or a flip to org visibility ends distribution on the next sync no
// matter what pins the caller holds. Only content, the two memory fields and
// the frozen type/name come from a revision.
//
// artifact_revision DOES carry a visibility column (migration 00016) and two
// Enterprise readers do select it, ListArtifactRevisionsPage and
// RollbackArtifact. What this path must never do is FILTER on it, and the
// guard is structural rather than a rule somebody has to remember:
// store.ArtifactRevisionPayload has no visibility field, so the batched read
// below cannot return one for anybody to reach for.
func (s *Server) handleSyncArtifacts(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var pins map[string]int
	if s.pinning {
		var err error
		if pins, err = parsePins(r.URL.Query()); err != nil {
			fail(w, err)
			return
		}
	}
	arts, err := s.store.ListEntitledArtifacts(r.Context(), rc.TenantID, rc.RoleIDs)
	if err != nil {
		fail(w, err)
		return
	}
	// Org-visibility RULES ride this channel too, and only rules. Every other
	// type reaches an org audience through the Channel-1 marketplace plugin, so
	// returning them here would install them twice; a rule has no Channel 1 at
	// all (marketplace.RenderArtifactsPlugin has no `rule` case), so before this
	// an org rule reached nobody, from the DEFAULT value of `visibility`.
	//
	// Appended AFTER the entitled read, never merged into it: the two sets are
	// disjoint by construction (an artifact has exactly one visibility) and
	// keeping them separate is what stops "org rules are universal" from
	// becoming "role rules are universal" through a shared code path.
	orgRules, err := s.store.ListActiveOrgRules(r.Context(), rc.TenantID)
	if err != nil {
		fail(w, err)
		return
	}
	arts = append(arts, orgRules...)
	// Resolve the clamp for every artifact first, then read every pinned
	// payload in ONE batched query. A pin naming an artifact the caller is not
	// entitled to never appears in arts, so it is dropped by absence and costs
	// nothing here.
	res := make([]pinResolution, len(arts))
	var keys []store.ArtifactRevisionKey
	for i, a := range arts {
		res[i] = pinResolve(pins[a.ID], a.MinRevisionNum, a.OldestRevision, a.Revision)
		if res[i].Served < a.Revision {
			keys = append(keys, store.ArtifactRevisionKey{ArtifactID: a.ID, RevisionNum: res[i].Served})
		}
	}
	// served == maxNum reads approved_content exactly as every release before
	// this one did (spec §5.1). Only served < maxNum reaches the revision
	// chain, which is why the unpinned path costs no extra query at all and
	// why the unenforced "approved_content equals the latest revision"
	// invariant stays load-bearing for a message and never for content.
	var payloads map[store.ArtifactRevisionKey]store.ArtifactRevisionPayload
	if len(keys) > 0 {
		if payloads, err = s.store.ListArtifactRevisionPayloads(r.Context(), rc.TenantID, keys); err != nil {
			fail(w, err)
			return
		}
	}
	out := make([]syncArtifactDTO, 0, len(arts))
	// counts accumulates pinned/overridden/overriddenArtifacts/truncated for
	// the audit event below via syncListAuditCounts.add, one artifact at a
	// time; see that type's doc comment for what each field means and why
	// overridden is computed from override rather than res[i].Reason. A race
	// that flips a served-exactly pin to pruned between the window read and
	// the payload read is reflected in the count too, not just in that one
	// artifact's own pinOverride field.
	counts := syncListAuditCounts{overriddenArtifacts: []string{}}
	for i, a := range arts {
		// Identity, content, the seed and the pin-override reason all come
		// from one decision, resolveSyncPayload, never half computed here
		// and half there.
		src, seed, revision, override := s.resolveSyncPayload(r.Context(), rc.TenantID, p.Subject, a, res[i], payloads)
		counts.add(pins[a.ID] > 0, res[i].Reason, override, a.Name)
		dto := syncArtifactDTO{
			ID: a.ID, Revision: revision, Type: src.Type, Name: src.Name,
			Content: marketplace.RenderArtifactContent(src),
		}
		// Targeting reaches the client for rules only. Reading it off `a` (the
		// live distribution row) rather than `src` (which may be a pinned older
		// revision's payload) is deliberate: a pin selects CONTENT, never
		// audience, exactly as it does for entitlement and visibility.
		if src.Type == "rule" {
			dto.TargetTags = a.ApprovedTargetTags
			// Only "global" is ever sent: "project" is the default and saying
			// so would make an older client, which cannot read the field,
			// differ from a newer one for no reason.
			if a.ApprovedRuleScope == "global" {
				dto.RuleScope = "global"
			}
		}
		// A seed is only deliverable to user/project-scope subagents (spec §6);
		// Go-level gate mirrors the admin validation, fail-closed.
		if src.Type == "subagent" && seed != "" && (src.MemoryScope == "user" || src.MemoryScope == "project") {
			dto.MemoryScope, dto.MemorySeed = src.MemoryScope, seed
		}
		if s.pinning {
			// syncArtifactDTO.PinOverride is a wire-format string (the JSON
			// field a client parses), never a correctedOverride: the explicit
			// conversion here is the one place a correctedOverride legitimately
			// turns back into a plain string, same asymmetry as
			// resolveSyncPayload's own res.Reason -> correctedOverride
			// conversion above, just running the other direction.
			dto.OldestServable, dto.Latest, dto.PinOverride = res[i].Oldest, a.Revision, string(override)
		}
		out = append(out, dto)
	}
	// Best-effort access log (mirrors catalog.list; exposes no secrets). A dropped
	// audit write must not fail a read, but the drop is logged at Warn.
	//
	// pinned/overridden/overriddenArtifacts/truncated are why this event
	// survives having a deployment registry at all, and the reason is not
	// the one the spec's first draft gave. Every artifact_deployment row is
	// self-asserted by a client (spec §4.4 bounds what may be done with one);
	// sync.list is server-observed, since it records what this handler
	// itself decided to serve. A forged deployment report cannot contradict
	// it. For the question an auditor actually asks after raising a floor,
	// "did this server serve anyone a below-floor version, and after which
	// approval", this audit event is the record and the registry is not. The
	// two are complements: the audit says what was served, the registry says
	// what is on disk.
	//
	// The four keys are added only when s.pinning, never as zeros, matching
	// syncArtifactDTO's own OldestServable/Latest/PinOverride fields (the
	// comment at the top of this file states why: these describe a feature a
	// Community server does not perform, and a promise it will not keep).
	// Emitting "pinned":0 unconditionally would be literally true on a
	// Community server, since ?pin is never parsed there at all, but it
	// invites an auditor scanning logs across editions to read the key's
	// ABSENCE elsewhere as missing data rather than as what it is here, a
	// feature this server does not have. Omitting the keys entirely is the
	// one reading that cannot be misread.
	md := map[string]any{"count": len(out)}
	if s.pinning {
		md["pinned"] = counts.pinned
		md["overridden"] = counts.overridden
		md["overriddenArtifacts"] = counts.overriddenArtifacts
		md["truncated"] = counts.truncated
	}
	s.logBestEffortAudit(r.Context(), store.AuditEvent{
		TenantID: rc.TenantID, Actor: p.Subject, Action: "sync.list",
		Decision: "allow", Metadata: md,
	})
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

// handleSyncConfig advertises client bootstrap config: the gateway resource
// URL, so orbeat-sync can write it into each tool's MCP config without a
// second URL knob, whether this server accepts deployment reports, and whether
// it honours ?pin. Authenticated (normal-user); no value here is secret, but
// gating keeps the surface consistent.
//
// deploymentRegistry is what lets a new client face an old server without a
// negotiation step. An old server returns a body with no such key, Go decodes
// the absent bool as false, and the client does not report: correct
// degradation with zero cooperation from the old server, where a POST to a
// pre-registry orbeat-api would return a 404 the client cannot tell from a
// typo'd base URL.
//
// pinning is the SAME mechanism carrying more weight, because the surface it
// negotiates cannot fail loudly. GET /v1/sync/artifacts reads no query
// parameter before this slice and net/http rejects no unknown one, so a new
// client sending ?pin= to any pre-pinning orbeat-api would be silently served
// the LATEST version with no error at all: a developer would believe she was
// held at revision 3 while her machine took revision 9. An old server's body
// has no pinning key, Go decodes the absence as false, and the client warns
// per pin and syncs latest deliberately instead of accidentally.
//
// THIS HANDLER IS SHARED ACROSS EDITIONS, which is why both flags carry the
// edition term rather than a knob alone (see SetDeploymentRegistry, and
// Server.pinning, which has no knob to combine). The halves that decide
// whether the behaviour exists are elsewhere: registerEnterpriseRoutes for the
// report route, s.pinning inside handleSyncArtifacts for the pin path. Reading
// anything else here would promise a Community client a feature its own server
// will not perform.
func (s *Server) handleSyncConfig(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.resolved(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_url":        s.gatewayURL,
		"deploymentRegistry": s.deploymentRegistry,
		"pinning":            s.pinning,
	})
}
