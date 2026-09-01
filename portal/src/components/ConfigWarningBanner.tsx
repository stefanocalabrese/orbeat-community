import { config, configLoadFailed } from "../config";

interface Props {
  /**
   * Defaults to the build's PROD flag (`import.meta.env.PROD`, Vite's
   * standard `vite build` vs `vite dev` signal). `npm run dev`'s dev server
   * never serves /config.json at all, so a failed load there is EXPECTED
   * and silent by design (loadConfig's own doc comment) — this banner exists
   * for the shipped image (deploy/Dockerfile.portal, `cmd/portal`), where a
   * failed load means an operator forgot ORBEAT_PORTAL_* or a proxy is
   * returning something other than /config.json.
   *
   * Overridable (rather than reading import.meta.env.PROD unconditionally
   * inside the component body) purely for testability: Vitest always runs
   * with PROD=false, so a hardcoded read could never be exercised true in a
   * unit test. Vite inlines `import.meta.env.PROD` as a build-time literal
   * wherever it textually appears, including here, so the real app still
   * gets the correct default with zero runtime cost.
   */
  enabled?: boolean;
}

/**
 * B16: a persistent, non-dismissible, operator-facing signal that the portal
 * is running on its BUILT-IN fallback config rather than the runtime
 * /config.json (config.ts's own comment) — never a hard app-boot block.
 *
 * Blocking was considered and rejected: the dominant real failure (a missing
 * ORBEAT_PORTAL_API_BASE, or a proxy serving HTML for /config.json) already
 * leaves the app pointed at a fallback that the portal's own CSP blocks
 * outright, so the console is ALREADY non-functional the moment this fires —
 * a full-screen block would remove the one debugging affordance (the URL bar,
 * devtools) an operator has to diagnose it from the very page reporting the
 * problem, and would turn a transient blip (a slow proxy on first paint) into
 * a dead reload loop. A banner turns silent, mysterious breakage into
 * diagnosable breakage without making a recoverable condition unrecoverable.
 *
 * Non-dismissible on purpose: this is an infra signal, not a per-user
 * notice, and it should still be on screen if someone opens the console
 * later in the same session having missed it the first time.
 */
export default function ConfigWarningBanner({ enabled = import.meta.env.PROD }: Props = {}) {
  if (!enabled || !configLoadFailed) return null;
  return (
    <div
      role="alert"
      className="border-b border-danger bg-danger-weak px-4 py-2 text-center text-sm font-medium text-danger"
    >
      Runtime configuration failed to load. Running on the built-in fallback ({config.apiBase}),
      which is almost certainly wrong for this deployment. Check ORBEAT_PORTAL_* and the portal
      server logs.
    </div>
  );
}
