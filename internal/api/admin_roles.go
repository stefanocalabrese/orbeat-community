package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

type roleInput struct {
	Name string `json:"name"`
}

// keycloakBuiltinRealmRoleNames are the realm-level role names Keycloak
// creates automatically for every realm and reassigns to every new user
// (audit B8, "Creating a role named offline_access grants its entitlement to
// the whole realm"). Sourced two ways, both required by the finding:
//
//   - LOCAL evidence, from the two committed realm files under
//     deploy/keycloak/ (orbeat-realm.json, orbeat-realm.prod.json, read but
//     not edited for this fix): "offline_access" is NOT declared under
//     roles.realm in either file (Keycloak provisions it itself; the import
//     only ASSIGNS a role Keycloak already has), yet 8 of the 9 user rows in
//     orbeat-realm.json — alice, boss, boss2, and all five seeded e2e-*
//     accounts — explicitly carry "offline_access" in realmRoles. Counted,
//     not recalled. The prod realm's single user carries none, which is not
//     a counter-example: Keycloak assigns the role via default-roles-<realm>
//     at first login whatever the import says, so the dev file merely makes
//     visible what the prod one leaves implicit. And
//     docs/runbooks/phase1-acceptance.md step 5 records granting it as a
//     deliberate step, not an accident. internal/auth's
//     principalFromClaims reads realm_access.roles verbatim into
//     auth.Principal.Roles, so this is the literal string every such user's
//     token carries.
//   - EXTERNAL, Keycloak's own documented default: Keycloak provisions
//     exactly TWO built-in realm roles for every realm it creates,
//     unconditionally, whether or not an import file mentions them —
//     "offline_access" (gates the OAuth offline_access scope) and
//     "uma_authorization" (gates User-Managed Access). Cross-checked
//     2026-09-01 against Keycloak's own community forum and Stack Overflow
//     ("Keycloak Creates new realm with default roles uma_authorization and
//     offline_access"; "in the console, in the realm roles we can see the 3
//     predefined roles default-roles-<realm>, offline_access and
//     uma_authorization"). "uma_authorization" appears in neither committed
//     realm file (this deployment never enables UMA), which is exactly what
//     "Keycloak provisions it regardless of the import file" predicts, and
//     is why it cannot be evidenced locally the way offline_access can —
//     it carries the identical exact-name-collision risk the moment any
//     deployment's realm has it assigned to a user, which is outside this
//     repo's control.
//
// store.GetRolesByNames (rbac.go) matches an orbeat role against
// auth.Principal.Roles by EXACT string equality, so a role created here
// under either name is granted to the same population as the identically
// named Keycloak built-in — B8's escalation.
//
// NOW included, by PREFIX rather than exact match: "default-roles-<realm>",
// Keycloak's third auto-assigned role (2026-09-01, audit B37/defect 2). It
// carries the identical exact-name-collision risk the two names above do —
// store.GetRolesByNames (rbac.go) still matches by EXACT string equality
// against auth.Principal.Roles, so a role created here under the realm's own
// default-roles name would be granted to every user Keycloak already grants
// it to, i.e. essentially the whole realm.
//
// WHY PREFIX AND NOT THE EXACT REALM NAME, argued rather than defaulted to:
// this deployment's realm name genuinely cannot be read from inside
// internal/api today, and adding a way to would need to cross a package
// boundary this fix is not scoped to touch (verified, not assumed, before
// concluding this):
//   - auth.Principal never carries the token's iss claim — principalFromClaims
//     (internal/auth/principal.go) copies exactly sub/email/azp/
//     preferred_username/realm_access.roles off a validated token and reads
//     no others, so nothing observed per-request names the realm.
//   - auth.Validator DOES hold the configured issuer (its unexported
//     cfg.Issuer, internal/auth/validator.go), and Server.validator already
//     holds a *Validator reference — wired unconditionally by every caller of
//     api.New, not behind an optional Set* installer — but Validator exports
//     no accessor for it. Adding one is a one-line internal/auth change this
//     fix does not touch.
//   - internal/config's OIDCIssuer (e.g. "http://kc/realms/orbeat") is where
//     the realm name actually lives on disk (the last path segment after
//     "/realms/"), but internal/api imports neither internal/config nor
//     cmd/api, and plumbing it in would need either of those.
//   - The wiring shape this codebase uses for "a value Server needs but New's
//     signature doesn't carry" is a SetXxx installer (SetContactEmail,
//     SetGatewayURL, ...), each one required to be called from cmd/api's
//     run() by cmd/api/installer_wiring_test.go's
//     TestAllServerInstallersAreWiredOrExempt — a go/ast gate that derives
//     every declared Set* method on Server and fails the build if cmd/api's
//     run() does not call it (or it is not named, with a reason, in that
//     test's own exemption list). A new SetRealmName would need cmd/api
//     wired to call it or a reasoned exemption added to that gate — this
//     fix's scope is internal/api, internal/store, internal/migrate,
//     internal/keycloak and portal/src only, and cmd/api is not in it. That
//     gate exists BECAUSE of exactly this failure mode (its own doc comment
//     names virtual keys shipping "wired to nothing" for nine tasks before
//     anyone noticed) — declaring a Set* method here without wiring it would
//     reproduce the class it was built to catch, not fix B37.
//
// GIVEN THAT, a prefix refusal is not a fallback taken for lack of the exact
// name — it is the MORE ROBUST rule even where the exact name were available:
//   - Keycloak always mints this exact literal prefix ("default-roles-",
//     confirmed by the LOCAL evidence this file's other comment already
//     cites and by Keycloak's own documented convention); no legitimate
//     business role has a reason to be named starting with it.
//   - It is realm-name-agnostic: it protects this deployment correctly
//     whatever its realm is called, and it protects dev/staging/prod
//     identically without a config value that could drift between them or
//     be set wrong in exactly one environment.
//   - It cannot become an INERT feature the way a config-value-fed exact
//     match could: there is no wiring step between "this code runs" and "the
//     protection is active", which is the precise shape of failure the
//     wiring gate above exists to catch one layer up.
//
// Case-sensitive, deliberately: Keycloak's own literal is always this exact
// lowercase prefix (it is Keycloak's OWN generated identifier, never
// operator input the way the realm-name suffix would be), so a differently
// cased string is not what Keycloak assigns and is a distinct, legitimate
// name — refusing it would cost real names for no security benefit, mirroring
// refuseKeycloakBuiltinRoleName's own "exact match only, near-matches are not
// refused" reasoning below.
const keycloakDefaultRolesPrefix = "default-roles-"

var keycloakBuiltinRealmRoleNames = map[string]bool{
	"offline_access":    true,
	"uma_authorization": true,
}

// refuseKeycloakBuiltinRoleName reports whether name is one of Keycloak's
// built-in realm roles: either an exact match against
// keycloakBuiltinRealmRoleNames, or a name beginning with
// keycloakDefaultRolesPrefix (that constant's own doc comment argues why a
// prefix is the right rule for the third built-in specifically). An orbeat
// role created or renamed to such a name would be granted to the same
// universal population the Keycloak built-in already carries (B8/B37). The
// exact-match half is intentionally still exact, not prefix, for
// offline_access/uma_authorization: the vulnerability is
// store.GetRolesByNames' exact-string comparison against the token's role
// list, so only a byte-identical name actually collides for THOSE two, and
// refusing a near-match would reject legitimate role names for no security
// benefit — default-roles-<realm> is the one case where the very thing being
// matched is a variable suffix orbeat cannot enumerate, which is what makes
// prefix matching necessary there and nowhere else in this function.
func refuseKeycloakBuiltinRoleName(name string) bool {
	return keycloakBuiltinRealmRoleNames[name] || strings.HasPrefix(name, keycloakDefaultRolesPrefix)
}

// roleDTO is the admin read/write projection of a role.
type roleDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// RowVersion is the optimistic-concurrency token a PUT must echo in
	// If-Match. Carried on the LIST rows and the CREATE response too, not
	// only a by-id read (there is no GET /v1/admin/roles/{id}), because
	// that is the only way the console ever obtains it for a rename —
	// mirrors entitlementDTO.RowVersion's own doc comment.
	RowVersion int64 `json:"rowVersion"`
}

func toRoleDTO(r store.Role) roleDTO {
	return roleDTO{ID: r.ID, Name: r.Name, RowVersion: r.RowVersion}
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in roleInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if refuseKeycloakBuiltinRoleName(in.Name) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%q is a Keycloak built-in realm role; creating an orbeat role with this exact name "+
				"would grant its entitlements to every user carrying that built-in (essentially the whole realm)", in.Name))
		return
	}
	if err := s.checkRoleCap(r.Context(), rc.TenantID); err != nil {
		fail(w, err)
		return
	}
	var created store.Role
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		created, e = tx.CreateRole(r.Context(), rc.TenantID, in.Name)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.create",
			Target: created.ID, Decision: "allow", Metadata: map[string]any{"name": created.Name},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRoleDTO(created))
}

// handleListRoles is keyset-paginated (?limit, ?cursor; see paging.go). The
// returned nextCursor uses the standard "possibly more" heuristic:
// len(rows)==limit means maybe-more, never definitely-more (there is no
// LIMIT+1 lookahead), so a tenant whose true role count is an exact multiple
// of limit gets one extra page back empty with nextCursor=="". That is
// expected keyset-pagination behavior, not a bug — pinned by
// TestListRolesPaginationExactMultiplePage in paging_test.go.
//
// ?q= searches name (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task
// 4), the same column this list sorts on, IN SQL via
// store.ListRolesPage's search argument, never a Go filter applied to the
// returned page, which pagination would silently turn into missing rows
// (v1.22.0's ?state defect, reproduced for search by
// TestListRolesSearchComposesWithPaging).
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText})
	if err != nil {
		fail(w, err)
		return
	}
	desc, err := sortOrderParams(r, roleSortName)
	if err != nil {
		fail(w, err)
		return
	}
	roles, err := s.store.ListRolesPage(r.Context(), rc.TenantID, cursor, limit, desc, searchParam(r))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]roleDTO, 0, len(roles))
	for _, role := range roles {
		out = append(out, toRoleDTO(role))
	}
	next := ""
	if len(roles) == limit && limit > 0 {
		next = encodeListCursor(store.RoleCursor(roles[len(roles)-1], desc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out, "limit": limit, "nextCursor": next})
}

// roleDeleteResponse is handleDeleteRole's 200 body: the counts of what its
// cascade revoked. Both fields are always present (never omitted), even when
// zero — a role deleted with no grants still returns {0, 0}, not an absent
// key, so a client never has to special-case "field missing" vs "zero".
type roleDeleteResponse struct {
	EntitlementsRevoked         int `json:"entitlementsRevoked"`
	ArtifactEntitlementsRevoked int `json:"artifactEntitlementsRevoked"`
}

// handleDeleteRole removes a role and everything granted to it.
//
// Deleting a role cascades to FIVE child tables over seven FK paths (ON
// DELETE CASCADE throughout, see store.DeleteRole's doc comment), so this
// one call revokes every server and artifact grant hung off the role, kills
// every virtual key capped by it, drops its monthly quota and erases its
// metering history. That is intended
// (docs/specs/2026-08-11-orbeat-role-deletion-design.md §3.1): the protection
// here is legibility, not prevention. The audit metadata below names exactly
// what was revoked so an operator can later answer "why did alice lose
// access?" and re-grant it if the deletion was a mistake: recoverable by
// inspection, NOT reversible. There is no undo.
//
// That legibility claim was FALSE for the last three children until A10
// (2026-08-30): the metadata was built from a struct that reported two of
// them, so a role deletion that broke every CI job authenticating with one of
// its virtual keys left a record saying only how many entitlements went.
//
// Returns 200 with the revoked counts rather than 204 like its sibling
// deletes (handleDeleteServer, handleDeleteEntitlement — spec §6.1): the
// portal cannot compute them client-side, because useEntitlements and
// useArtifactEntitlements are capped at 100 rows by default, and a
// client-side count would silently understate the blast radius on exactly
// the roles that have the most grants — the ones where getting it wrong
// matters most.
func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var revoked store.RevokedGrants
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		g, e := tx.DeleteRole(r.Context(), rc.TenantID, id)
		if e != nil {
			return store.AuditEvent{}, e
		}
		revoked = g
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.delete",
			Target: id, Decision: "allow",
			Metadata: map[string]any{
				"name":                        g.RoleName,
				"entitlementsRevoked":         g.Entitlements,
				"artifactEntitlementsRevoked": g.ArtifactEntitlements,
				"servers":                     g.ServerNames,
				"artifacts":                   g.ArtifactNames,
				// The last three children of role, silent in this record
				// until A10 (2026-08-30). virtualKeyClientIds is the one an
				// operator cannot reconstruct from anywhere else: each of
				// those client_ids is a Keycloak client this DELETE just
				// orphaned, and the query virtual_key.ee.go points operators
				// at for orphans (?revoked=true) cannot return a row that no
				// longer exists. quotaMonthlyCalls is JSON null, never
				// absent, when the role had no quota, because "no quota" and
				// "a quota of zero" are different facts to whoever re-creates
				// the role.
				"virtualKeysRevoked":  g.VirtualKeys,
				"virtualKeyClientIds": g.VirtualKeyClientIDs,
				"usageRowsDeleted":    g.UsageRows,
				"usageCallsDeleted":   g.UsageCalls,
				"quotaMonthlyCalls":   g.QuotaMonthlyCalls,
				"truncated":           g.Truncated,
			},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	// AFTER the commit, never inside it, and best effort by construction.
	// Same placement and same reasoning as handleRevokeVirtualKey's cleanup:
	// the Keycloak call is a network round trip, and holding the audited
	// transaction open across it is audit finding B7, which was just fixed on
	// the create path. A failure here must not roll back a role deletion that
	// has already happened and been audited.
	//
	// Until 2026-09-01 this did not exist, so deleting a role destroyed its
	// virtual_key rows and left every one of their Keycloak clients behind,
	// while the revoke path (migration 00030) cleaned up correctly. The
	// client_ids were written to the audit event so an operator could delete
	// them by hand, and that record stays: it is what makes a failure below
	// recoverable rather than merely logged. capo's decision, 2026-09-01.
	s.deleteRoleKeycloakClients(r.Context(), revoked.VirtualKeysForCleanup)

	writeJSON(w, http.StatusOK, roleDeleteResponse{
		EntitlementsRevoked:         revoked.Entitlements,
		ArtifactEntitlementsRevoked: revoked.ArtifactEntitlements,
	})
}

// deleteRoleKeycloakClients deletes the Keycloak clients of the virtual keys a
// role deletion destroyed.
//
// Every arm logs, and none returns an error: the orbeat rows are already gone
// and committed, so there is nothing left to fail. What an operator gets from
// a failure is the WARN plus the client_ids already in the role.delete audit
// event, which together are the manual recovery path this replaces for the
// common case rather than removes.
//
// A key with no sealed token is the pre-migration-00030 population, and it is
// logged distinctly rather than lumped in with a failed delete: one is "orbeat
// never had what it needed", the other is "orbeat had it and Keycloak refused",
// and an operator chasing orphans needs to tell those apart.
func (s *Server) deleteRoleKeycloakClients(ctx context.Context, keys []store.RevokedVirtualKey) {
	if len(keys) == 0 {
		return
	}
	if s.dcrDelete == nil {
		s.logger.WarnContext(ctx, "role deleted with virtual keys but no Keycloak registrar is configured; "+
			"their Keycloak clients were not deleted and are now orphaned",
			"count", len(keys))
		return
	}
	for _, k := range keys {
		if k.RegistrationAccessTokenSealed == "" {
			s.logger.WarnContext(ctx, "role deleted a virtual key with no registration access token on file; "+
				"its Keycloak client cannot be cleaned up automatically and is now orphaned "+
				"(keys created before migration 00030 have none)",
				"clientId", k.ClientID)
			continue
		}
		if err := s.dcrDelete(ctx, k.ClientID, k.RegistrationAccessTokenSealed); err != nil {
			s.logger.WarnContext(ctx, "role deleted a virtual key but its Keycloak client could not be deleted; "+
				"it is now orphaned and is named in the role.delete audit event",
				"clientId", k.ClientID, "error", err)
			continue
		}
		s.logger.InfoContext(ctx, "deleted the Keycloak client of a virtual key destroyed by a role deletion",
			"clientId", k.ClientID)
	}
}

// roleRenameInput is the body of PUT /v1/admin/roles/{id}
// (docs/plans/orbeat-role-rename-2026-08-27.md). IdpRenamed is the
// operator's assertion that the realm role was already renamed to Name in
// the identity provider — consulted ONLY when no realm-role lookup is
// configured (s.roleExists == nil). When a lookup IS configured it is
// authoritative and this flag is IGNORED: see verifyIdpRename's doc
// comment for why an assertion must never be allowed to bypass a check
// that is actually running.
type roleRenameInput struct {
	Name       string `json:"name"`
	IdpRenamed bool   `json:"idpRenamed"`
}

// idpAssertionRequiredError maps to HTTP 400 with a machine-readable body
// (idpAssertionRequiredCode, respond.go): this deployment has no realm-role
// lookup configured, so a rename can only proceed on the operator's
// explicit assertion that the IdP side was already renamed, and this
// request did not supply one. Distinct from a plain validationError so
// fail() can dispatch it to writeIdpAssertionRequired instead of a bare
// message, letting the portal recognise it programmatically and offer the
// confirmation checkbox (the design's decision: "the portal learns the mode
// from a 400, not a capability endpoint").
type idpAssertionRequiredError struct{ msg string }

func (e idpAssertionRequiredError) Error() string { return e.msg }

// idpUnavailableError maps to HTTP 502: an operator-configured identity
// provider check could not be evaluated (403/401/500/timeout from
// Keycloak). This must NEVER be treated the same as "the check is
// unavailable, fall back to the assertion" — a 403 means the service
// account is configured but lacks view-realm, so the operator believes a
// check is running that is not (docs/plans/orbeat-role-rename-2026-08-27.md,
// "Decisions taken inside the approved design").
type idpUnavailableError struct{ msg string }

func (e idpUnavailableError) Error() string { return e.msg }

// verifyIdpRename decides whether renaming a role to in.Name may proceed.
// idpCheck names the mechanism that decided it — "verified"/"asserted" on
// success, or one of "verified_absent"/"lookup_error"/"assertion_missing"
// alongside a non-nil err — and is always recorded in the role.rename audit
// metadata (handleUpdateRole) regardless of outcome: it is the whole
// governance value of this feature, since authz.Resolver.Resolve binds a
// role to the IdP BY NAME (internal/store/rbac.go's GetRolesByNames), so a
// rename that detaches from the real realm role silently revokes every user
// holding it while their entitlements survive.
//
// THE SINGLE MOST IMPORTANT BEHAVIOUR HERE: when s.roleExists is configured,
// it is authoritative and in.IdpRenamed is IGNORED from the moment the
// lookup runs — an operator ticking the assertion checkbox must never be
// able to talk this function past a lookup that says the target name does
// not exist, and a lookup error must never be read as "the check is
// unavailable, fall back to the assertion" (that is precisely how a 403 from
// a service account missing view-realm would silently disable the guard the
// operator believes is protecting them).
func (s *Server) verifyIdpRename(ctx context.Context, in roleRenameInput) (idpCheck string, err error) {
	if s.roleExists == nil {
		// No lookup configured (Community, or an Enterprise deployment with
		// no ORBEAT_DCR_CLIENT_ID): the operator's explicit assertion is the
		// only signal available, and it is consulted ONLY in this branch.
		if !in.IdpRenamed {
			return "assertion_missing", idpAssertionRequiredError{
				msg: "no realm-role lookup is configured on this deployment; confirm the role was " +
					"already renamed in the identity provider by resubmitting with idpRenamed=true",
			}
		}
		return "asserted", nil
	}

	exists, lookupErr := s.roleExists(ctx, in.Name)
	if lookupErr != nil {
		return "lookup_error", idpUnavailableError{
			msg: fmt.Sprintf("could not verify %q against the identity provider: %s", in.Name, lookupErr.Error()),
		}
	}
	if !exists {
		return "verified_absent", validationError{
			msg: fmt.Sprintf("no realm role named %q exists in the identity provider; rename it there first", in.Name),
		}
	}
	return "verified", nil
}

// handleUpdateRole renames a role.
//
// If-Match is REQUIRED (parsed before any read, matching every other guarded
// route in this package): a missing, malformed or refused precondition
// rejects the request before it touches the store or calls out to the
// identity provider. The role's current name is read BEFORE
// verifyIdpRename runs, both to answer 404 without spending a network round
// trip on a nonexistent role and to capture the audit trail's "from" value —
// safe to read outside the transaction that performs the actual rename
// because UpdateRoleName's own row_version guard is what decides whether the
// write proceeds; a name observed here can only be stale in a way that
// guard already rejects.
//
// Every outcome is audited, allow and deny alike (v1.17.0: "deny decisions
// were never audited" must not regress here of all places), carrying
// verifyIdpRename's idpCheck value so the trail can answer "who said this
// role existed?" — verified against the identity provider, or asserted by
// the operator.
func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	expected, err := ifMatch(r)
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	var in roleRenameInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if refuseKeycloakBuiltinRoleName(in.Name) {
		// Refused before GetRole and before verifyIdpRename, deliberately:
		// verifyIdpRename's own doc comment names exactly this asymmetry as
		// the sharp part of B8 — a configured realm-role lookup answers
		// "verified" for "offline_access" (Keycloak really does have that
		// role), and the lookup has no way to know the caller is asking about
		// its OWN role table's collision with the IdP's. This check must run
		// first so that lookup is never even consulted for a name it cannot
		// safely arbitrate. No audit trail (matching the in.Name == "" arm
		// immediately above): this is input validation, not a governance
		// decision the way a stale If-Match or a failed idpCheck is.
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%q is a Keycloak built-in realm role; renaming an orbeat role to this exact name "+
				"would grant its entitlements to every user carrying that built-in (essentially the whole realm)", in.Name))
		return
	}

	current, err := s.store.GetRole(r.Context(), rc.TenantID, id)
	if err != nil {
		// A malformed/unknown/cross-tenant id is a plain 404, not a security
		// event on this surface — deliberately NOT audited, matching every
		// sibling If-Match handler's ErrNotFound arm.
		fail(w, err)
		return
	}

	idpCheck, verr := s.verifyIdpRename(r.Context(), in)
	if verr != nil {
		if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.rename",
			Target: id, Decision: "deny",
			Metadata: map[string]any{"from": current.Name, "to": in.Name, "idpCheck": idpCheck, "reason": verr.Error()},
		}); aerr != nil {
			fail(w, aerr)
			return
		}
		fail(w, verr)
		return
	}

	var updated store.Role
	err = s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		updated, e = tx.UpdateRoleName(r.Context(), rc.TenantID, id, in.Name, expected)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.rename",
			Target: updated.ID, Decision: "allow",
			Metadata: map[string]any{"from": current.Name, "to": updated.Name, "idpCheck": idpCheck},
		}, nil
	})
	if err != nil {
		// A stale If-Match is a rejected mutation on an authorization
		// surface, so it leaves a durable trace before the client sees the
		// 412 — mirroring handleUpdateEntitlement/handleUpdateServer exactly.
		if errors.Is(err, store.ErrVersionMismatch) {
			if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
				TenantID: rc.TenantID, Actor: p.Subject, Action: "role.rename",
				Target: id, Decision: "deny",
				Metadata: map[string]any{"from": current.Name, "to": in.Name, "idpCheck": idpCheck, "reason": "version_mismatch"},
			}); aerr != nil {
				fail(w, aerr)
				return
			}
		}
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(updated.RowVersion))
	writeJSON(w, http.StatusOK, toRoleDTO(updated))
}
