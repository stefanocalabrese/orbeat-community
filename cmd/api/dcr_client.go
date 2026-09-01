package main

import (
	"context"

	"github.com/lestrrat-go/jwx/v3/jwk"

	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
)

// buildDCRClient is the Community no-op counterpart to dcr_client.ee.go's
// real implementation (docs/specs/2026-08-19-orbeat-community-repo-
// generation-design.md sec 4). Virtual keys are Enterprise only (docs/specs/
// 2026-08-25-orbeat-virtual-keys-design.md sec 12: Community's single role
// gives a key no shape distinct from every human's own access), and the
// identity-provider role-rename check (docs/plans/orbeat-role-rename-
// 2026-08-27.md) is Enterprise-only for the identical reason its "one
// credential, not two" decision only matters where more than one role
// exists to rename between -- so a Community build never imports
// internal/keycloak at all and this always returns (nil, nil, nil, nil)
// regardless of ORBEAT_DCR_CLIENT_ID -- that variable is therefore inert
// here rather than wrong, the same relationship startDeploymentRetention's
// Community counterpart documents for ORBEAT_DEPLOYMENT_RETENTION_DAYS.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and dcr_client.ee.go, does not
// fail on a duplicate declaration; see internal/api/routes_enterprise.
// community.go for the toolchain-level proof this file plays no part in a
// normal `go build`.
func buildDCRClient(ctx context.Context, cfg config.Config, secretsResolver *secrets.Resolver) (
	register func(ctx context.Context, jwks jwk.Set, name string) (clientID, registrationAccessToken string, err error),
	del func(ctx context.Context, clientID, registrationAccessToken string) error,
	checkRoleExists func(ctx context.Context, roleName string) (bool, error),
	err error,
) {
	return nil, nil, nil, nil
}
