import { useEffect, useRef, type ReactNode } from "react";
import { AuthProvider as OidcProvider, useAuth as useOidc } from "react-oidc-context";
import { WebStorageStateStore } from "oidc-client-ts";
import { config } from "../config";
import { setUnauthorizedHandler } from "../api/client";
import { AuthCtx } from "./useAuth";
import { postLoginTarget } from "./postLoginTarget";

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
    void oidc.signinRedirect();
  };

  // Register the 401 → re-login handler once on mount (so the single-fire
  // guard is armed exactly once, not re-armed by every render), routing through
  // a ref so the handler always sees the CURRENT oidc session's login. The
  // dep-less effect keeps the ref fresh on every render.
  const loginRef = useRef(login);
  useEffect(() => {
    loginRef.current = login;
  });
  useEffect(() => {
    setUnauthorizedHandler(() => loginRef.current());
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
      {children}
    </AuthCtx.Provider>
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
