// Pure route→title mapping, kept out of the component file so fast-refresh
// (react-refresh/only-export-components) stays happy and the map is unit-testable.
const LABELS: Record<string, string> = {
  "/login": "Sign in",
  "/catalog": "Catalog",
  "/connect": "Connect",
  "/admin/servers": "Servers",
  "/admin/artifacts": "Artifacts",
  "/admin/review": "Review queue",
  "/admin/artifact-entitlements": "Artifact entitlements",
  "/admin/roles": "Roles",
  "/admin/entitlements": "Entitlements",
  "/admin/audit": "Audit log",
};

export function titleFor(pathname: string): string {
  const label = LABELS[pathname];
  return label ? `orbeat · ${label}` : "orbeat";
}
