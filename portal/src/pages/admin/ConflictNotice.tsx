import { Button } from "../../components/ui/Button";

/**
 * The unhappy path for a 412 (spec §10): tells the admin their write lost a
 * race against the current row_version and gives them a way out. `onReload`
 * must refetch the underlying query so a subsequent save/approve carries a
 * current precondition — it must NEVER itself re-issue the write. A silent
 * retry would resubmit the same stale body against a fresh version, i.e.
 * last-write-wins wearing a hat, exactly what this slice removes.
 */
export function ConflictNotice({ onReload }: { onReload: () => void }) {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-lg border border-warn bg-warn-weak px-3 py-2 text-sm text-warn">
      <p>This changed since you loaded it — reload to see the current state.</p>
      {/*
       * type="button" is load-bearing, not decoration: ConflictNotice is
       * rendered inside the edit forms' <form> (ServersPage/ArtifactsPage),
       * and a <button> with no explicit type inside a <form> defaults to
       * type="submit". Without this, clicking Reload also fires the form's
       * onSubmit and re-issues the very write that just 412'd — an
       * accidental resubmit of the stale body, caught by
       * ServersPage.conflict.test.tsx's exactly-once PUT assertion.
       */}
      <Button type="button" variant="ghost" onClick={onReload}>
        Reload
      </Button>
    </div>
  );
}
