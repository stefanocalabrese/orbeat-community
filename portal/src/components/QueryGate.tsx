import type { ReactNode } from "react";
import type { UseQueryResult } from "@tanstack/react-query";
import { errMsg } from "../api/client";
import { Button } from "./ui/Button";
import { Card } from "./ui/Card";

/**
 * Renders a query's three honest states: loading, error (with a Retry), and
 * success via a data-receiving render prop. Keep any "no rows yet" empty-state
 * INSIDE the success branch — an errored query must never render as an empty
 * list.
 */
export function QueryGate<T>({
  query,
  label,
  children,
}: {
  query: UseQueryResult<T>;
  label: string;
  children: (data: T) => ReactNode;
}) {
  if (query.isPending) return <p className="text-muted">Loading…</p>;
  if (query.isError) {
    return (
      <Card className="mt-4 max-w-xl p-5">
        <p className="text-sm font-medium text-danger">
          Failed to load {label}: {errMsg(query.error)}
        </p>
        <Button
          variant="ghost"
          className="mt-3"
          onClick={() => void query.refetch()}
        >
          Retry
        </Button>
      </Card>
    );
  }
  return <>{children(query.data)}</>;
}
