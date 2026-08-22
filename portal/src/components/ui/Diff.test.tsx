import { render, screen, within } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { Diff } from "./Diff";

describe("Diff", () => {
  test("classifies unchanged, removed, and added lines independently per pane", () => {
    render(<Diff before={"alpha\nBETA\ngamma"} beforeLabel="Old" after={"alpha\nbeta\ngamma"} afterLabel="New" />);

    // Unchanged lines appear on both sides.
    expect(screen.getAllByText("alpha")).toHaveLength(2);
    expect(screen.getAllByText("gamma")).toHaveLength(2);

    // The changed line: old form only in "Old", new form only in "New".
    const oldPane = screen.getByText("Old").parentElement as HTMLElement;
    const newPane = screen.getByText("New").parentElement as HTMLElement;
    expect(within(oldPane).getByText("BETA")).toBeInTheDocument();
    expect(within(oldPane).queryByText("beta")).not.toBeInTheDocument();
    expect(within(newPane).getByText("beta")).toBeInTheDocument();
    expect(within(newPane).queryByText("BETA")).not.toBeInTheDocument();

    // The removed line reads as removed (red), the added line as added (green).
    expect(screen.getByText("BETA").closest("div")?.className).toMatch(/text-danger/);
    expect(screen.getByText("beta").closest("div")?.className).toMatch(/text-ok/);
  });

  test("an omitted `before` renders exactly one undiffed pane, never a diff against an empty string", () => {
    const { container } = render(<Diff after={"only-ever-version"} afterLabel="Revision #1" />);
    expect(screen.getByText("only-ever-version")).toBeInTheDocument();
    // One DiffPane label only — a diff against "" would still render two
    // panes (one showing nothing but "(empty)"), which is the exact
    // misleading "everything is added" shape this fallback exists to avoid.
    expect(container.querySelectorAll("p").length).toBe(1);
  });

  test("an empty string side renders the (empty) placeholder, not a blank pane", () => {
    render(<Diff before="" beforeLabel="Old" after="new content" afterLabel="New" />);
    expect(screen.getByText("(empty)")).toBeInTheDocument();
    expect(screen.getByText("new content")).toBeInTheDocument();
  });

  test("never renders blank for real multi-line content (the ReviewQueuePage regression class)", () => {
    const before = "line one\nline two\nline three";
    const after = "line one\nline two changed\nline three\nline four";
    render(<Diff before={before} beforeLabel="Old" after={after} afterLabel="New" />);
    expect(screen.getAllByText("line one")).toHaveLength(2);
    expect(screen.getByText("line two")).toBeInTheDocument();
    expect(screen.getByText("line two changed")).toBeInTheDocument();
    expect(screen.getAllByText("line three")).toHaveLength(2);
    expect(screen.getByText("line four")).toBeInTheDocument();
  });
});
