package gateway

import (
	"context"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// UsageCounter is the Community edition's no-op counterpart to the
// Enterprise type of the same name (usage.ee.go): usage metering is
// Enterprise-only (docs/specs/2026-08-25-orbeat-usage-metering-design.md
// section 5 -- "Community already caps seats, servers and roles, which is
// its throttle"), so Count and Flush here do nothing and no store call is
// ever made.
//
// This file exists so a future SHARED caller (once a later task wires a
// call site into rbac_middleware.go, mirroring the resolveVirtualKey/
// keyRevoked pairing in virtualkey.community.go) can reference
// UsageCounter/Count/Flush without an edition-specific branch. It ALSO --
// and today, this is its only observable effect in this repo's own build --
// keeps internal/communitygen's boundary scan
// (TestNoSharedFileReferencesEnterpriseSymbol) from flagging every unrelated
// shared file that merely spells the bare token "Count" (a struct field or
// local variable, not a call to this type) as a leak of the Enterprise-only
// method of the same name: declaring "Count" here too makes it shared
// vocabulary, the same exclusion TestEnterpriseOnlyNamesExcludesSharedVocabulary
// (boundary_test.go) proves for "Scan". Verified: with only usage.ee.go
// declaring Count, the scan flagged 11 unrelated shared files (struct
// fields and local variables coincidentally also named Count) that never
// reference this type at all.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and usage.ee.go, does not fail
// on a duplicate UsageCounter declaration -- the generator renames this file
// to usage.go and drops usage.ee.go, so a generated Community tree compiles
// no reference to store.IncrementUsage/MonthlyCallsForSubject/RoleQuota
// anywhere (those stay Enterprise-only in internal/store/usage.ee.go, also
// dropped).
type UsageCounter struct{}

// NewUsageCounter is the Community stub matching usage.ee.go's constructor
// signature exactly, so a future shared caller needs no edition branch. s
// and tenantID are both ignored: Community never counts or persists usage.
func NewUsageCounter(s *store.Store, tenantID string) *UsageCounter {
	return &UsageCounter{}
}

// Count is a no-op in Community. Signature (including roleID, added by
// Task 1's correction to attribute a call to the entitlement that
// authorized it) matches usage.ee.go's exactly, so a future shared caller
// needs no edition branch.
func (c *UsageCounter) Count(subject, serverID, tool, roleID string) {}

// Flush is a no-op in Community: no accumulation ever happened, so there is
// nothing to write.
func (c *UsageCounter) Flush(ctx context.Context) error { return nil }
