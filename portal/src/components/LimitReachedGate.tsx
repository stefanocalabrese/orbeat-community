import { useCallback, useEffect, useRef, useState } from "react";
import { setLimitReachedHandler } from "../api/client";
import type { LimitInfo } from "../api/types";
import { LimitReachedModal } from "./LimitReachedModal";

/**
 * Owns the 402 burst policy and renders LimitReachedModal. Renders nothing
 * until a cap is reported, so it is free to mount at the app root.
 *
 * THE BURST PROBLEM, and it is worse here than for the 401 it sits beside.
 * The seat cap (spec §3.2) is enforced in authz.Resolver.Middleware, which
 * runs on EVERY authenticated request, so a capped user 402s on ordinary
 * catalog and admin GETs: every query on the page fails at once, and the
 * admin pages' marketplace-status poll (useMarketplaceStatus,
 * refetchInterval 2000) keeps failing every two seconds for as long as the
 * tab is open. A handler that opened the modal on each 402 would re-open it
 * two seconds after every dismissal: a dialog the user cannot close, with a
 * Close button that visibly does nothing.
 *
 * THE POLICY, in one sentence: a 402 opens the dialog only when nothing is
 * already open AND the user has not already dismissed a dialog for that same
 * `resource`.
 *
 *  - `setOpen(prev => prev ?? info)` makes a burst idempotent: the first 402
 *    wins and the rest return the same reference, which React treats as a
 *    bail-out rather than a re-render. The first is kept rather than the
 *    latest because a burst is all one cap, and swapping the payload under a
 *    dialog someone is reading would make the numbers flicker.
 *  - `dismissed` is a ref, not state: it must never cause a render, and it is
 *    only ever read and written from event callbacks, never during render.
 *  - Dismissal is per-resource, so hitting the server cap, closing the
 *    dialog, then hitting the ROLE cap still tells the user about the role
 *    cap. It is deliberately NOT re-armed for the same resource afterwards:
 *    the condition is not transient (a seat frees only when the 7-day active
 *    window rolls, or an admin deletes a user), so re-arming would rebuild
 *    the unclosable dialog the policy exists to prevent. A page reload
 *    re-arms everything, which is the right granularity for a limit that
 *    moves only when someone buys or an admin deletes.
 *
 * THE TRADE THIS MAKES, recorded rather than fixed. The latch covers 402s of
 * mutation origin too, not only the query burst that motivates it. So an admin
 * who dismisses the servers dialog and then tries to create another server
 * gets no dialog the second time: they fall back to the inline error their
 * page already renders, which shows limitError.Error()'s prose ("community
 * edition limit reached: servers (10 of 10 used)") and therefore NOT the
 * contact address, since only the structured `limit` object carries it. Making
 * mutation-origin 402s always re-open would fix that, at the cost of a
 * behaviour beyond spec §6 and a second policy to reason about. Reviewed and
 * left as is; revisit if an operator actually reports losing the address.
 */
export function LimitReachedGate() {
  const [open, setOpen] = useState<LimitInfo | null>(null);
  const dismissed = useRef<Set<string>>(new Set());

  useEffect(() => {
    setLimitReachedHandler((info) => {
      if (dismissed.current.has(info.resource)) return;
      setOpen((prev) => prev ?? info);
    });
    return () => setLimitReachedHandler(null);
  }, []);

  const dismiss = useCallback(() => {
    if (open) dismissed.current.add(open.resource);
    setOpen(null);
  }, [open]);

  if (!open) return null;
  return <LimitReachedModal info={open} onDismiss={dismiss} />;
}
