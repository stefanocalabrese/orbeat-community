import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, test } from "vitest";
import RouteTitle from "./RouteTitle";
import { titleFor } from "./routeTitleMap";

test("titleFor maps known routes to 'orbeat · <label>' and falls back to 'orbeat'", () => {
  expect(titleFor("/catalog")).toBe("orbeat · Catalog");
  expect(titleFor("/admin/servers")).toBe("orbeat · Servers");
  expect(titleFor("/admin/audit")).toBe("orbeat · Audit log");
  expect(titleFor("/unknown/route")).toBe("orbeat");
});

test("RouteTitle sets document.title from the current pathname", () => {
  render(
    <MemoryRouter initialEntries={["/connect"]}>
      <RouteTitle />
    </MemoryRouter>,
  );
  expect(document.title).toBe("orbeat · Connect");
});
