function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Renders arbitrary JSON readably without assuming a schema — used for audit
 * `metadata` (fable-audit §7 #16 item 1), whose shape varies per `action`
 * (e.g. `role.delete`'s `{name, entitlementsRevoked, artifactEntitlementsRevoked,
 * servers[], artifacts[], truncated}` vs. a simple action's flat map).
 * Primitives render as text, arrays as indented lists, objects as indented
 * key/value pairs, recursively — so nested shapes stay legible instead of
 * being hidden or dumped as a raw JSON string.
 */
export function JsonTree({ value }: { value: unknown }) {
  if (value === null || value === undefined) {
    return <span className="text-faint">null</span>;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="text-faint">[]</span>;
    return (
      <ul className="space-y-1">
        {value.map((item, i) => (
          <li key={i} className="border-l border-border pl-2">
            <JsonTree value={item} />
          </li>
        ))}
      </ul>
    );
  }
  if (isPlainObject(value)) {
    const entries = Object.entries(value);
    if (entries.length === 0) return <span className="text-faint">{"{}"}</span>;
    return (
      <dl className="space-y-1">
        {entries.map(([key, v]) => (
          <div key={key} className="flex flex-wrap items-baseline gap-x-2">
            <dt className="font-mono text-xs font-semibold text-muted">{key}</dt>
            <dd className="min-w-0 flex-1 font-mono text-xs text-text">
              <JsonTree value={v} />
            </dd>
          </div>
        ))}
      </dl>
    );
  }
  if (typeof value === "boolean") {
    return <span className="font-mono text-xs text-text">{value ? "true" : "false"}</span>;
  }
  return <span className="font-mono text-xs text-text">{String(value)}</span>;
}
