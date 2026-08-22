import { Link, NavLink, Outlet } from "react-router";
import { useAuth, useIsAdmin } from "../auth/useAuth";
import HexLogo from "./HexLogo";
import ThemeToggle from "../theme/ThemeToggle";

const navCls = ({ isActive }: { isActive: boolean }) =>
  `rounded-md px-3 py-1.5 text-sm font-medium transition-colors ${
    isActive ? "bg-accent-weak text-text" : "text-muted hover:bg-surface-2 hover:text-text"
  }`;

export default function Layout() {
  const { email, logout } = useAuth();
  const isAdmin = useIsAdmin();
  return (
    <div className="min-h-screen bg-bg">
      <header className="border-b border-border bg-surface">
        <div className="mx-auto flex max-w-6xl items-center gap-4 px-6 py-3">
          <Link to="/catalog" className="flex items-center gap-2 text-text">
            <HexLogo />
            <span className="text-base font-semibold tracking-tight">orbeat</span>
          </Link>
          <nav className="flex flex-1 gap-1">
            <NavLink to="/catalog" className={navCls}>Catalog</NavLink>
            <NavLink to="/connect" className={navCls}>Connect</NavLink>
            {isAdmin && <NavLink to="/admin/servers" className={navCls}>Admin</NavLink>}
          </nav>
          <ThemeToggle />
          <span className="font-mono text-sm text-muted">{email}</span>
          <button onClick={logout} className="text-sm text-muted hover:text-text">Sign out</button>
        </div>
      </header>
      <main className="mx-auto max-w-6xl"><Outlet /></main>
    </div>
  );
}
