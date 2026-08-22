import { Link } from "react-router";
import { useCatalog } from "../../api/queries";
import { Card } from "../../components/ui/Card";
import { Chip, Pill } from "../../components/ui/Badge";

function ServerGlyph() {
  return (
    <div className="grid h-8 w-8 shrink-0 place-items-center rounded-lg bg-accent-weak text-accent-ink">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <rect x="3" y="4" width="18" height="6" rx="1.5" />
        <rect x="3" y="14" width="18" height="6" rx="1.5" />
        <circle cx="7" cy="7" r="0.75" fill="currentColor" stroke="none" />
        <circle cx="7" cy="17" r="0.75" fill="currentColor" stroke="none" />
      </svg>
    </div>
  );
}

export default function CatalogPage() {
  const { data, isLoading, error } = useCatalog();
  if (isLoading) return <p className="p-8 text-muted">Loading catalog…</p>;
  if (error) return <p className="p-8 text-danger">Failed to load catalog: {(error as Error).message}</p>;
  const servers = data?.servers ?? [];
  return (
    <div className="p-8">
      <p className="text-xs font-semibold uppercase tracking-wide text-faint">Self-service</p>
      <h1 className="mt-1 text-2xl font-semibold text-text">Catalog</h1>
      <p className="mt-2 text-sm text-muted">Entitled MCP servers, ready to connect.</p>

      {servers.length === 0 && (
        <p className="mt-6 text-faint">No entitled MCP servers yet. Ask an admin for access.</p>
      )}

      <div className="mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {servers.map((s) => (
          <Card key={s.id} className="p-4 transition hover:border-border-strong hover:shadow-sm">
            <div className="flex items-center gap-3">
              <ServerGlyph />
              <div className="min-w-0">
                <h2 className="truncate text-sm font-semibold text-text">{s.name}</h2>
                <p className="font-mono text-xs text-faint">{s.transport}</p>
              </div>
            </div>
            {s.description && <p className="mt-3 text-sm text-muted">{s.description}</p>}
            <div className="mt-4 flex items-center justify-between gap-2">
              <Chip variant="subagent">MCP Tool</Chip>
              <Pill variant={s.status === "active" ? "ok" : "neutral"} dot>
                {s.status}
              </Pill>
            </div>
            <Link
              to="/connect"
              className="mt-4 inline-flex w-full items-center justify-center rounded-md bg-accent px-3.5 py-2 text-sm font-medium text-white shadow-sm transition hover:brightness-110"
            >
              Connect
            </Link>
          </Card>
        ))}
      </div>
    </div>
  );
}
