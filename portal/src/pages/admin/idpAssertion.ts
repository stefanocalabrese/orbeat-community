import { ApiRequestError } from "../../api/client";

/**
 * The machine-readable body field PUT /v1/admin/roles/{id} returns when no
 * realm-role lookup is configured on this deployment (internal/api/respond.go's
 * idpAssertionRequiredCode). Mirrored here as a plain string constant rather
 * than imported from anywhere, because the API and the portal are separate
 * build artifacts (docs/plans/orbeat-role-rename-2026-08-27.md's decision:
 * "the portal learns the mode from a 400, not a capability endpoint") --
 * pinned identical on both sides by
 * TestUpdateRoleRefusesWithoutAssertionWhenNoLookupConfigured on the Go side
 * and RolesPage.rename.test.tsx on this one.
 */
export const IDP_ASSERTION_REQUIRED_CODE = "idp_rename_assertion_required";

/**
 * True iff `e` is the "confirm you already renamed this in the identity
 * provider" refusal -- the ONLY case the rename form may offer the
 * consequence-bearing checkbox for. Every other 4xx (409 name collision, the
 * plain 400 "no such realm role" a configured lookup returns, a 502 lookup
 * failure) must render inline as an ordinary message: none of those are the
 * operator's to override, only the missing-assertion case is (see
 * admin_roles.go's verifyIdpRename doc comment for why a configured lookup's
 * refusal must never be bypassable by this same flag).
 */
export function isIdpAssertionRequired(e: unknown): e is ApiRequestError {
  return e instanceof ApiRequestError && e.code === IDP_ASSERTION_REQUIRED_CODE;
}
