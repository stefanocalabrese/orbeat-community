import { useEffect, useRef, useState, type ReactNode } from "react";
import { AuthProvider as OidcProvider, useAuth as useOidc } from "react-oidc-context";
import { WebStorageStateStore } from "oidc-client-ts";
import { config } from "../config";
import { setUnauthorizedHandler } from "../api/client";
import { AuthCtx } from "./useAuth";
import { postLoginTarget } from "./postLoginTarget";
import { Button } from "../components/ui/Button";

// Keycloak puts realm roles in the ACCESS token's `realm_access.roles`, not
// the ID-token profile. We first check the profile object (which mirrors the
// ID token, and may carry realm_access if the scope/mapper is configured);
// if that's empty we fall back to decoding the access token directly.
// Unit tests stub `AuthCtx` directly so they're agnostic to this detail.
function decodeJwtPayload(token: string): Record<string, unknown> | null {
  try {
    const part = token.split(".")[1];
    if (!part) return null;
    const json = atob(part.replace(/-/g, "+").replace(/_/g, "/"));
    return JSON.parse(json) as Record<string, unknown>;
  } catch {
    return null;
  }
}

function rolesFrom(accessToken: string, profile: unknown): string[] {
  const fromProfile = (profile as { realm_access?: { roles?: string[] } })?.realm_access?.roles;
  if (fromProfile && fromProfile.length > 0) return fromProfile;
  const payload = accessToken ? decodeJwtPayload(accessToken) : null;
  const ra = (payload as { realm_access?: { roles?: string[] } } | null)?.realm_access;
  return ra?.roles ?? [];
}

function Bridge({ children }: { children: ReactNode }) {
  const oidc = useOidc();

  const login = (target?: string) => {
    // An explicit target (LoginPage forwarding a guard-carried deep link) wins;
    // otherwise save the current path — but never auth-only routes — as the
    // post-login redirect target. The 401 handler calls login() with no
    // argument, so an expired session still restores the page it died on.
    const here = window.location.pathname;
    const fallback = here === "/login" || here === "/auth/callback" ? "/catalog" : here;
    sessionStorage.setItem("orbeat.postLogin", target ?? fallback);
    // Returns the redirect promise (rather than `void`-ing it here) so
    // attemptReauth below can observe a rejection. AuthCtx's own `login` type
    // stays `() => void`: TypeScript allows a void-typed slot to be filled by
    // a function returning something more specific, and every other caller
    // (LoginPage's button, the 401 handler) already ignores the return value.
    return oidc.signinRedirect();
  };

  // Register the 401 → re-login handler once on mount (so the single-fire
  // guard is armed exactly once, not re-armed by every render), routing through
  // a ref so the handler always sees the CURRENT oidc session's login. The
  // dep-less effect keeps the ref fresh on every render.
  const loginRef = useRef(login);
  useEffect(() => {
    loginRef.current = login;
  });
  // Shown while the OIDC round trip is in flight after an expired session.
  // Before this, a 401 navigated the user away with no explanation: the app
  // simply vanished mid-task, which reads as a crash rather than as a session
  // ending. v1.17.0 made the recovery WORK (QueryGate plus a single-fire
  // re-login); this is the part that says so.
  const [reauthenticating, setReauthenticating] = useState(false);
  // B17: set when the MOST RECENT signinRedirect() attempt rejected (network
  // blip, discovery endpoint unreachable, ...) rather than merely being in
  // flight. Before this, the rejection was silently discarded (`void
  // oidc.signinRedirect()`) and the overlay stayed on "Signing you back in"
  // forever with no way out — worse than the crash it replaced, since a crash
  // at least let the user reload. Distinguishing "in flight" from "failed"
  // is what lets the overlay offer Retry/dismiss only once there is actually
  // nothing left to wait for.
  const [reauthFailed, setReauthFailed] = useState(false);
  // Shared by the automatic 401 path and the overlay's own Retry button, so a
  // retry is a genuine second signinRedirect() call, not a dead click. Routed
  // through a ref (same idiom as loginRef above) so the dep-less effect below
  // always calls the CURRENT version rather than closing over the one from
  // the render that registered it.
  const attemptReauth = () => {
    setReauthFailed(false);
    loginRef.current()?.catch(() => setReauthFailed(true));
  };
  const reauthRef = useRef(attemptReauth);
  useEffect(() => {
    reauthRef.current = attemptReauth;
  });
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setReauthenticating(true);
      reauthRef.current();
    });
    return () => setUnauthorizedHandler(null);
  }, []);

  return (
    <AuthCtx.Provider
      value={{
        isLoading: oidc.isLoading,
        authenticated: !!oidc.isAuthenticated,
        token: oidc.user?.access_token ?? "",
        subject: oidc.user?.profile?.sub ?? "",
        email: (oidc.user?.profile?.email as string) ?? "",
        roles: rolesFrom(oidc.user?.access_token ?? "", oidc.user?.profile),
        login,
        logout: () => void oidc.signoutRedirect(),
      }}
    >
      {reauthenticating && <ReauthOverlay failed={reauthFailed} onRetry={attemptReauth} />}
      {children}
    </AuthCtx.Provider>
  );
}

/**
 * The overlay covering the app during a re-login redirect.
 *
 * While the redirect is in flight (`failed` false) it offers nothing to
 * decide: the redirect is already underway, and a button that cancelled it
 * would leave the user on a page whose every request 401s. Its job there is
 * only to answer "why did this stop working".
 *
 * Once the redirect has REJECTED (`failed` true), there is by definition
 * nothing left in flight to wait for, so staying silent would strand the
 * user behind a permanent, actionless overlay (B17) — it offers Retry (a
 * genuine second signinRedirect() attempt, the same one a transient network
 * blip usually just needs) and a dismiss that sends the user to the plain
 * /login page via a full navigation, which also resets api/client.ts's
 * single-fire 401 latch for free (a fresh page load re-initialises the whole
 * JS context) rather than leaving the user stuck on a page where nothing
 * will ever prompt a re-login again.
 */
function ReauthOverlay({ failed, onRetry }: { failed: boolean; onRetry: () => void }) {
  return (
    <div
      role="alert"
      aria-live="assertive"
      className="fixed inset-0 z-50 flex items-center justify-center bg-surface/90 backdrop-blur-sm"
    >
      <div className="max-w-sm rounded-lg border border-border-strong bg-surface p-6 text-center shadow-lg">
        {failed ? (
          <>
            <p className="text-sm font-semibold text-text">Sign-in failed</p>
            <p className="mt-2 text-sm text-muted">
              We could not reach the sign-in server. Check your connection and try again.
            </p>
            <div className="mt-4 flex justify-center gap-3">
              <Button variant="primary" onClick={onRetry}>
                Retry
              </Button>
              <Button variant="ghost" onClick={() => window.location.assign("/login")}>
                Go to sign-in
              </Button>
            </div>
          </>
        ) : (
          <>
            <p className="text-sm font-semibold text-text">Your session expired</p>
            <p className="mt-2 text-sm text-muted">
              Signing you back in. You will land back on the page you were using.
            </p>
          </>
        )}
      </div>
    </div>
  );
}

export function AppAuthProvider({ children }: { children: ReactNode }) {
  // Built at render-time (after main.tsx's loadConfig()) so authority/client_id
  // reflect the runtime /config.json, not the build-time fallback. AppAuthProvider
  // mounts once at the app root.
  const oidcConfig = {
    authority: config.oidcAuthority,
    client_id: config.oidcClientId,
    redirect_uri: `${window.location.origin}/auth/callback`,
    post_logout_redirect_uri: window.location.origin,
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
    onSigninCallback: () => {
      // Validate on read: only a same-origin absolute path is restored, so a
      // hostile stored value can never become an open redirect.
      const target = postLoginTarget(sessionStorage.getItem("orbeat.postLogin"));
      sessionStorage.removeItem("orbeat.postLogin");
      // Use location.replace so that React Router picks up the navigation
      // (history.replaceState alone does not fire popstate and React Router
      // would not re-render to the new route).
      window.location.replace(target);
    },
  };
  return (
    <OidcProvider {...oidcConfig}>
      <Bridge>{children}</Bridge>
    </OidcProvider>
  );
}
