import { useState } from "react";
import { useCatalog } from "../../api/queries";
import { config } from "../../config";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";

function CopyBlock({ label, text }: { label: string; text: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");

  async function copy() {
    try {
      // navigator.clipboard is undefined on non-secure origins and writeText
      // rejects when the page lacks focus/permission — either way, tell the
      // user instead of silently swallowing the failure.
      await navigator.clipboard.writeText(text);
      setState("copied");
    } catch {
      setState("failed");
    }
    setTimeout(() => setState("idle"), 2000);
  }

  return (
    <div className="mt-3">
      <p className="text-xs font-semibold uppercase tracking-wide text-faint">{label}</p>
      <div className="mt-1 flex items-start gap-2">
        <pre className="flex-1 overflow-x-auto rounded-lg border border-border bg-inset p-3 font-mono text-sm text-text">{text}</pre>
        <Button
          variant="ghost"
          aria-label={`Copy ${label}`}
          onClick={() => void copy()}
          className={state === "failed" ? "text-danger" : state === "copied" ? "text-ok" : ""}
        >
          {state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : "Copy"}
        </Button>
      </div>
    </div>
  );
}

export default function ConnectPage() {
  const catalog = useCatalog();
  const mcpUrl = `${config.gatewayUrl}/mcp`;
  const marketplaceAddCmd = `claude plugin marketplace add ${config.marketplaceSource}`;
  const pluginInstallCmd = `claude plugin install orbeat-gateway@orbeat`;
  const teamSettings = JSON.stringify(
    {
      extraKnownMarketplaces: { orbeat: { source: config.marketplaceSource } },
      enabledPlugins: { "orbeat-gateway@orbeat": true },
    },
    null,
    2,
  );
  const claudeCmd = `claude mcp add --transport http orbeat ${mcpUrl}`;
  const mcpJson = JSON.stringify({ mcpServers: { orbeat: { type: "http", url: mcpUrl } } }, null, 2);
  return (
    <div className="max-w-3xl p-8">
      <h1 className="text-2xl font-semibold text-text">Connect to the orbeat gateway</h1>
      <p className="mt-2 text-sm text-muted">
        One governed endpoint. The client authenticates via SSO (OAuth) and sees only entitled tools.
      </p>

      <Card className="mt-6 p-6">
        <h2 className="text-lg font-medium text-text">Native (Claude Code plugin)</h2>
        <p className="mt-1 text-sm text-muted">
          Recommended. Run these in a terminal. Sign-in is requested once, on first use.
        </p>
        <CopyBlock label="1. Add the marketplace" text={marketplaceAddCmd} />
        <CopyBlock label="2. Install the plugin" text={pluginInstallCmd} />
        <CopyBlock label="Team setup (.claude/settings.json)" text={teamSettings} />
      </Card>

      <Card className="mt-6 p-6">
        <h2 className="text-lg font-medium text-text">Native (artifacts)</h2>
        <p className="mt-1 text-sm text-muted">
          Org-wide skills and subagents authored by administrators. Install once to get the full governed set.
        </p>
        <CopyBlock label="Install the artifacts plugin" text={`claude plugin install orbeat-artifacts@orbeat`} />
      </Card>

      <Card className="mt-6 p-6">
        <h2 className="text-lg font-medium text-text">Manual setup</h2>
        <CopyBlock label="Gateway endpoint" text={mcpUrl} />
        <CopyBlock label="Claude Code" text={claudeCmd} />
        <CopyBlock label="Generic MCP client (JSON)" text={mcpJson} />
      </Card>

      <h2 className="mt-8 text-lg font-medium text-text">Entitled servers &amp; tools</h2>
      <QueryGate query={catalog} label="entitled servers">
        {({ servers }) => (
          <>
            {servers.length === 0 && (
              <p className="mt-2 text-sm text-muted">Nothing yet — ask an admin for an entitlement.</p>
            )}
            <ul className="mt-2 space-y-2">
              {servers.map((s) => (
                <li key={s.id} className="rounded-xl border border-border bg-surface p-4 shadow-sm">
                  <span className="font-medium text-text">{s.name}</span>
                  <span className="ml-2 text-sm text-muted">
                    {s.allowedTools === null ? "all tools" : s.allowedTools.join(", ")}
                  </span>
                </li>
              ))}
            </ul>
          </>
        )}
      </QueryGate>
    </div>
  );
}
