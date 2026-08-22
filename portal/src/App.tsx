import { BrowserRouter, Navigate, Route, Routes } from "react-router";
import Layout from "./components/Layout";
import AdminLayout from "./components/AdminLayout";
import RouteTitle from "./components/RouteTitle";
import { LimitReachedGate } from "./components/LimitReachedGate";
import AuthCallback from "./auth/AuthCallback";
import { RequireAdmin, RequireAuth } from "./auth/guards";
import LoginPage from "./pages/LoginPage";
import CatalogPage from "./pages/catalog/CatalogPage";
import ConnectPage from "./pages/connect/ConnectPage";
import ServersPage from "./pages/admin/ServersPage";
import ArtifactsPage from "./pages/admin/ArtifactsPage";
import ReviewQueuePage from "./pages/admin/ReviewQueuePage";
import RolesPage from "./pages/admin/RolesPage";
import EntitlementsPage from "./pages/admin/EntitlementsPage";
import ArtifactEntitlementsPage from "./pages/admin/ArtifactEntitlementsPage";
import AuditPage from "./pages/admin/AuditPage";

export default function App() {
  return (
    <BrowserRouter>
      {/* First child on purpose: React commits effects in tree order, so the
          gate has registered its 402 handler before anything under <Routes>
          mounts a query that could raise one. It renders null until a cap is
          reported, and needs no router context: it sits here rather than in
          main.tsx only to keep the app's one mount point in one file. */}
      <LimitReachedGate />
      <RouteTitle />
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        {/* /auth/callback: AuthCallback stays minimal while react-oidc-context
            processes the code+state params in its own useEffect. onSigninCallback
            then calls window.location.replace(target) which triggers a full
            navigation so React Router re-renders at the post-login destination. A
            static <Navigate> would strip the code/state before the OIDC library
            can consume them; history.replaceState alone doesn't fire popstate.
            AuthCallback additionally bounces a params-less (stale/direct) visit
            home and recovers from a failed exchange instead of hanging blank. */}
        <Route path="/auth/callback" element={<AuthCallback />} />
        <Route element={<RequireAuth><Layout /></RequireAuth>}>
          <Route path="/" element={<Navigate to="/catalog" replace />} />
          <Route path="/catalog" element={<CatalogPage />} />
          <Route path="/connect" element={<ConnectPage />} />
          <Route element={<RequireAdmin><AdminLayout /></RequireAdmin>}>
            <Route path="/admin" element={<Navigate to="/admin/servers" replace />} />
            <Route path="/admin/servers" element={<ServersPage />} />
            <Route path="/admin/artifacts" element={<ArtifactsPage />} />
            <Route path="/admin/review" element={<ReviewQueuePage />} />
            <Route path="/admin/artifact-entitlements" element={<ArtifactEntitlementsPage />} />
            <Route path="/admin/roles" element={<RolesPage />} />
            <Route path="/admin/entitlements" element={<EntitlementsPage />} />
            <Route path="/admin/audit" element={<AuditPage />} />
          </Route>
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
