package gateway

import (
	"context"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// QuotaEnforcer is the Community edition's no-op counterpart to the
// Enterprise type of the same name (quota.ee.go): quota enforcement is
// Enterprise-only (docs/specs/2026-08-25-orbeat-usage-metering-design.md
// section 5 -- "Community already caps seats, servers and roles, which is
// its throttle"), so Refresh and Check here do nothing and no store call is
// ever made; Check always returns nil (never denies).
//
// This file exists so the SHARED caller (rbac_middleware.go, which calls
// s.quota.Check unconditionally whenever s.quota is non-nil) can reference
// QuotaEnforcer/Refresh/Check without an edition-specific branch, mirroring
// usage.community.go's UsageCounter pairing exactly -- including the same
// reason for existing at all: it keeps internal/communitygen's boundary
// scan (TestNoSharedFileReferencesEnterpriseSymbol) from flagging every
// unrelated shared file that merely spells a bare token this type also
// uses ("Check", "Refresh") as a leak of an Enterprise-only symbol.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and quota.ee.go, does not fail
// on a duplicate QuotaEnforcer declaration -- the generator renames this
// file to quota.go and drops quota.ee.go, so a generated Community tree
// compiles no reference to store.RoleQuota / store.GetRoleQuota /
// store.MonthlyCallsForRole anywhere (those stay Enterprise-only in
// internal/store/usage.ee.go, also dropped).
type QuotaEnforcer struct{}

// NewQuotaEnforcer is the Community stub matching quota.ee.go's constructor
// signature exactly, so the shared caller needs no edition branch. s and
// tenantID are both ignored: Community never enforces a quota.
func NewQuotaEnforcer(s *store.Store, tenantID string) *QuotaEnforcer {
	return &QuotaEnforcer{}
}

// Refresh is a no-op in Community: there is no cache to populate.
func (q *QuotaEnforcer) Refresh(ctx context.Context, roleID string) error { return nil }

// Check always allows in Community: quotas do not exist here.
func (q *QuotaEnforcer) Check(roleID string) error { return nil }

// RefreshAll is a no-op in Community: there is no cache to populate and no
// store.ListRoleQuotas to call (that symbol is Enterprise-only, dropped
// entirely from a generated Community tree). Matches quota.ee.go's
// RefreshAll signature exactly, so cmd/gateway/main.go's single call site
// needs no edition branch.
func (q *QuotaEnforcer) RefreshAll(ctx context.Context) error { return nil }
