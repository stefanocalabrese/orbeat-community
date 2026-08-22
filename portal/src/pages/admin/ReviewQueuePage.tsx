import { useState } from "react";
import {
  useReviewQueue,
  useApproveArtifact,
  useRejectArtifact,
} from "../../api/queries";
import type { AdminArtifact, ScanFinding } from "../../api/types";
import { errMsg } from "../../api/client";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { StateBadge, Chip } from "../../components/ui/Badge";
import { Card } from "../../components/ui/Card";
import { Diff } from "../../components/ui/Diff";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";

const sevCls: Record<ScanFinding["severity"], string> = {
  info: "text-muted bg-surface-2",
  warn: "text-warn bg-warn-weak",
  block: "text-danger bg-danger-weak",
};

function withSeed(content: string, memorySeed: string | null | undefined) {
  return memorySeed ? `${content}\n\n--- seed ---\n${memorySeed}` : content;
}

// fable-audit §7 #16 item 2: reuses the shared line-level Diff (extracted
// from this file's former plain-`<pre>` two-pane render) rather than a
// second bespoke diff view — see portal/src/components/ui/Diff.tsx. `before`
// (currently live) is omitted entirely when there is no approved snapshot
// yet, rendering a single undiffed "Proposed" pane instead of a diff against
// an empty string, which would falsely mark the whole proposal as "added".
function ArtifactDiff({ a }: { a: AdminArtifact }) {
  return (
    <div className="mt-4">
      <Diff
        before={a.approvedContent !== undefined ? withSeed(a.approvedContent, a.approvedMemorySeed) : undefined}
        beforeLabel="Currently live (approved)"
        after={withSeed(a.content, a.memorySeed)}
        afterLabel="Proposed (working)"
      />
    </div>
  );
}

// `onReload` refetches the review queue — the parent's list refetch feeds
// this card fresh props (including a fresh `rowVersion`) without needing a
// remount: unlike the two edit forms, approve carries no admin-typed body,
// so there is nothing here to preserve or discard (Task 11 judgment call —
// see the Task 11 report).
function ReviewCard({ a, onReload }: { a: AdminArtifact; onReload: () => void }) {
  const approve = useApproveArtifact();
  const reject = useRejectArtifact();
  const [reason, setReason] = useState("");
  const conflict = isConflict(approve.error);

  return (
    <Card className="max-w-3xl p-0">
      <div className="border-b border-border px-5 py-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-semibold text-text">{a.name}</span>
          <Chip variant={a.type}>{a.type}</Chip>
          <Chip>{a.visibility}</Chip>
          <StateBadge state="pending" />
        </div>
        <p className="mt-1 text-xs text-muted">submitted by {a.submittedBy}</p>
      </div>

      <div className="px-5 py-4">
        {conflict ? (
          <ConflictNotice
            onReload={() => {
              approve.reset();
              onReload();
            }}
          />
        ) : (
          (approve.error || reject.error) && (
            <p className="text-sm text-danger">{errMsg(approve.error ?? reject.error)}</p>
          )
        )}

        {a.scanFindings && a.scanFindings.length > 0 && (
          <ul className="space-y-1.5">
            {a.scanFindings.map((f, i) => (
              <li
                key={i}
                className={`flex flex-wrap items-center gap-2 rounded-lg px-3 py-2 text-sm ${sevCls[f.severity]}`}
              >
                <span className="text-xs font-semibold uppercase tracking-wide">{f.severity}</span>
                <span className="font-mono text-xs">{f.rule}</span>
                <span>{f.message}</span>
              </li>
            ))}
          </ul>
        )}

        <ArtifactDiff a={a} />
      </div>

      <div className="flex items-center gap-2 border-t border-border bg-inset px-5 py-3">
        <input
          aria-label="Reject reason"
          className="flex-1 rounded-md border border-border-strong bg-surface px-2.5 py-1.5 text-sm text-text placeholder:text-faint focus:border-accent focus:outline-none"
          placeholder="reason (for reject)"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
        <Button
          variant="danger"
          onClick={() => reject.mutate({ id: a.id, reason })}
          disabled={reject.isPending}
        >
          Reject
        </Button>
        <Button
          variant="approve"
          onClick={() => approve.mutate({ id: a.id, rowVersion: a.rowVersion })}
          disabled={approve.isPending}
        >
          Approve
        </Button>
      </div>
    </Card>
  );
}

export default function ReviewQueuePage() {
  const queue = useReviewQueue();
  const { rows: artifacts } = queue;

  const cards = (
    <div className="mt-4 space-y-4">
      {artifacts.map((a) => (
        <ReviewCard key={a.id} a={a} onReload={() => void queue.refetch()} />
      ))}
    </div>
  );

  return (
    <div>
      <p className="text-xs font-semibold uppercase tracking-wide text-faint">Governance</p>
      <h1 className="mt-1 text-2xl font-semibold text-text">Review queue</h1>
      <p className="mt-2 text-sm text-muted">
        Artifacts awaiting approval. Submitters cannot approve their own submission.
      </p>

      {artifacts.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // queue never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={queue} label="the review queue">
          {() => <p className="mt-6 text-faint">Nothing pending review.</p>}
        </QueryGate>
      ) : (
        <>
          {cards}
          {queue.isError && (
            // No separate Retry action: refetch() on an infinite query
            // re-fetches the last SUCCESSFUL page, not the failed next one —
            // "Load more" below (fetchNextPage) is the control that
            // correctly retries the page that just failed.
            <p className="mt-2 text-sm text-danger">
              Failed to load more of the review queue: {errMsg(queue.error)}
            </p>
          )}
          {queue.isFetchingNextPage ? (
            <p className="mt-2 text-sm text-muted">Loading more…</p>
          ) : (
            queue.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void queue.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}
    </div>
  );
}
