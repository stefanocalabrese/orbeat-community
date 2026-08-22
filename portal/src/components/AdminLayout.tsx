import { NavLink, Outlet } from "react-router";

const itemCls = ({ isActive }: { isActive: boolean }) =>
  `block rounded-md px-3 py-2 text-sm font-medium transition-colors ${
    isActive ? "bg-accent-weak text-text" : "text-muted hover:bg-surface-2 hover:text-text"
  }`;

export default function AdminLayout() {
  return (
    <div className="flex gap-6 p-8">
      <aside className="w-44 shrink-0 space-y-1">
        <p className="px-3 pb-1 text-[10.5px] font-bold uppercase tracking-widest text-faint">Console</p>
        <NavLink to="/admin/servers" className={itemCls}>Servers</NavLink>
        <NavLink to="/admin/artifacts" className={itemCls}>Artifacts</NavLink>
        <NavLink to="/admin/review" className={itemCls}>Review queue</NavLink>
        <NavLink to="/admin/roles" className={itemCls}>Roles</NavLink>
        <NavLink to="/admin/entitlements" className={itemCls}>Entitlements</NavLink>
        <NavLink to="/admin/artifact-entitlements" className={itemCls}>Artifact entitlements</NavLink>
        <NavLink to="/admin/audit" className={itemCls}>Audit</NavLink>
      </aside>
      <section className="min-w-0 flex-1"><Outlet /></section>
    </div>
  );
}
