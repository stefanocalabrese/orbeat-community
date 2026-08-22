import { useEffect, useState } from "react";
import { Navigate, useNavigate } from "react-router";
import { useAuth as useOidc } from "react-oidc-context";

// The IdP returns to /auth/callback with `code`+`state` (or an `error`); react-
// oidc-context consumes those in its own effect and onSigninCallback then does a
// full-page location.replace to the post-login target. A visit WITHOUT those
// params — a direct link, a stale bookmark, a back-button — has nothing to
// process, so the old blank <div/> stayed blank forever. Bounce those home, and
// surface (then recover from) a failed exchange instead of hanging blank.
function hasAuthParams(): boolean {
  const p = new URLSearchParams(window.location.search);
  return p.has("code") || p.has("state") || p.has("error");
}

const GRACE_MS = 8000; // library neither finished nor errored → treat as failed
const RECOVER_MS = 4000; // let the user read the error, then fall back home

export default function AuthCallback() {
  const oidc = useOidc();
  const navigate = useNavigate();
  const [timedOut, setTimedOut] = useState(false);
  const processing = hasAuthParams();
  const failed = processing && (!!oidc.error || timedOut);

  useEffect(() => {
    if (!processing) return;
    const t = setTimeout(() => setTimedOut(true), GRACE_MS);
    return () => clearTimeout(t);
  }, [processing]);

  useEffect(() => {
    if (!failed) return;
    const t = setTimeout(() => navigate("/catalog", { replace: true }), RECOVER_MS);
    return () => clearTimeout(t);
  }, [failed, navigate]);

  // Stale/direct/back-button visit: nothing to consume — go home immediately.
  if (!processing) return <Navigate to="/catalog" replace />;

  if (failed) {
    return (
      <div role="alert" className="mx-auto mt-24 max-w-md px-6 text-center">
        <p className="text-sm font-medium text-danger">Sign-in could not be completed.</p>
        {oidc.error?.message && <p className="mt-1 text-xs text-muted">{oidc.error.message}</p>}
        <p className="mt-3 text-xs text-faint">Returning to orbeat…</p>
      </div>
    );
  }

  // Happy path: keep this render minimal while the library processes the
  // exchange; a successful exchange navigates away via location.replace.
  return <div />;
}
