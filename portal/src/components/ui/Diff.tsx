import { diffLines } from "diff";

interface DiffLine {
  text: string;
  kind: "same" | "added" | "removed";
}

const lineCls: Record<DiffLine["kind"], string> = {
  added: "bg-ok-weak text-ok",
  removed: "bg-danger-weak text-danger",
  same: "text-text",
};

const marker: Record<DiffLine["kind"], string> = {
  added: "+",
  removed: "-",
  same: " ",
};

// jsdiff keeps each line's trailing "\n" inside `value` (the last chunk of
// the whole diff omits it iff the source string itself didn't end in one) —
// strip exactly one trailing newline before splitting, so a mid-content
// blank line isn't lost and no phantom trailing empty line is added.
function splitLine(value: string): string[] {
  const trimmed = value.endsWith("\n") ? value.slice(0, -1) : value;
  return trimmed.split("\n");
}

function paneLines(parts: ReturnType<typeof diffLines>, side: "before" | "after"): DiffLine[] {
  const lines: DiffLine[] = [];
  for (const part of parts) {
    if (side === "before" && part.added) continue;
    if (side === "after" && part.removed) continue;
    const kind: DiffLine["kind"] = part.added ? "added" : part.removed ? "removed" : "same";
    for (const text of splitLine(part.value)) lines.push({ text, kind });
  }
  return lines;
}

function rawLines(text: string): DiffLine[] {
  return text.split("\n").map((line) => ({ text: line, kind: "same" }));
}

function DiffPane({ label, lines }: { label: string; lines: DiffLine[] }) {
  return (
    <div className="min-w-0">
      <p className="mb-1 text-xs font-medium text-muted">{label}</p>
      <div className="overflow-x-auto rounded-lg border border-border bg-inset p-3 font-mono text-xs">
        {lines.length === 0 ? (
          <span className="text-faint">(empty)</span>
        ) : (
          lines.map((l, i) => (
            <div key={i} className={`whitespace-pre px-1 ${lineCls[l.kind]}`}>
              <span aria-hidden="true" className="select-none pr-2 text-faint">
                {marker[l.kind]}
              </span>
              {l.text === "" ? " " : l.text}
            </div>
          ))
        )}
      </div>
    </div>
  );
}

/**
 * Line-level diff of two text blobs (fable-audit §7 #16 item 2 — revision
 * `content` was already in the payload and rendered as opaque, undiffed
 * blocks). Built on jsdiff's `diffLines`: every line of both `before` and
 * `after` is individually classified as added/removed/unchanged, then split
 * into a "before" pane (unchanged + removed) and an "after" pane (unchanged
 * + added) — each pane independently reconstructs its own full source text,
 * so neither can render blank or lossy (see
 * ReviewQueuePage.content.test.tsx for the prior defect class this must not
 * reintroduce: an approver approving without ever seeing full content).
 *
 * `before` is optional: omit it (and `beforeLabel`) when there is nothing to
 * compare against yet (e.g. an artifact's first-ever revision, or a review
 * queue item with no prior approved snapshot) — a single undiffed pane of
 * `after` renders instead of the misleading "everything is added" a diff
 * against an empty string would imply.
 */
export function Diff({
  before,
  beforeLabel,
  after,
  afterLabel,
}: {
  before?: string;
  beforeLabel?: string;
  after: string;
  afterLabel: string;
}) {
  if (before === undefined) {
    return (
      <div className="grid gap-3">
        <DiffPane label={afterLabel} lines={rawLines(after)} />
      </div>
    );
  }
  const parts = diffLines(before, after);
  return (
    <div className="grid gap-3 md:grid-cols-2">
      <DiffPane label={beforeLabel ?? ""} lines={paneLines(parts, "before")} />
      <DiffPane label={afterLabel} lines={paneLines(parts, "after")} />
    </div>
  );
}
