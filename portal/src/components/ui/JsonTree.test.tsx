import { render, screen } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { JsonTree } from "./JsonTree";

describe("JsonTree", () => {
  test("renders a nested object with arrays, numbers, and booleans (the role.delete metadata shape)", () => {
    render(
      <JsonTree
        value={{
          name: "eng",
          entitlementsRevoked: 3,
          artifactEntitlementsRevoked: 1,
          servers: ["orbeat-gateway", "upstream-x"],
          artifacts: ["release-notes-skill"],
          truncated: false,
        }}
      />,
    );
    expect(screen.getByText("name")).toBeInTheDocument();
    expect(screen.getByText("eng")).toBeInTheDocument();
    expect(screen.getByText("servers")).toBeInTheDocument();
    expect(screen.getByText("orbeat-gateway")).toBeInTheDocument();
    expect(screen.getByText("upstream-x")).toBeInTheDocument();
    expect(screen.getByText("artifacts")).toBeInTheDocument();
    expect(screen.getByText("release-notes-skill")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("1")).toBeInTheDocument();
    // booleans render as the literal words, not "0"/"1" or empty
    expect(screen.getByText("false")).toBeInTheDocument();
  });

  test("renders a doubly-nested object (object containing an object)", () => {
    render(<JsonTree value={{ outer: { inner: "leaf-value" } }} />);
    expect(screen.getByText("outer")).toBeInTheDocument();
    expect(screen.getByText("inner")).toBeInTheDocument();
    expect(screen.getByText("leaf-value")).toBeInTheDocument();
  });

  test("an empty object renders {} rather than nothing", () => {
    render(<JsonTree value={{}} />);
    expect(screen.getByText("{}")).toBeInTheDocument();
  });

  test("an empty array renders [] rather than nothing", () => {
    render(<JsonTree value={[]} />);
    expect(screen.getByText("[]")).toBeInTheDocument();
  });

  test("null and undefined render the word null, not a blank", () => {
    const { unmount } = render(<JsonTree value={null} />);
    expect(screen.getByText("null")).toBeInTheDocument();
    unmount();
    render(<JsonTree value={undefined} />);
    expect(screen.getByText("null")).toBeInTheDocument();
  });
});
