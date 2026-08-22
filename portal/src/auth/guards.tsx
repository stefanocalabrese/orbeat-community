import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router";
import { useAuth, useIsAdmin } from "./useAuth";

export function RequireAuth({ children }: { children: ReactNode }) {
  const { isLoading, authenticated } = useAuth();
  const location = useLocation();
  // Hold while the OIDC provider initialises (e.g. loading stored user from
  // sessionStorage after a page reload). Redirecting during loading would
  // bounce the user to /login on every page reload even when authenticated.
  if (isLoading) return null;
  // Carry the intended URL so LoginPage can restore the deep link after SSO.
  if (!authenticated) return <Navigate to="/login" replace state={{ from: location }} />;
  return <>{children}</>;
}

export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isLoading, authenticated } = useAuth();
  const isAdmin = useIsAdmin();
  const location = useLocation();
  if (isLoading) return null;
  if (!authenticated) return <Navigate to="/login" replace state={{ from: location }} />;
  if (!isAdmin) return <Navigate to="/catalog" replace />;
  return <>{children}</>;
}
