import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import HexLogo from "./HexLogo";

test("renders a hexagon with a purple and a blue vertex point", () => {
  const { container } = render(<HexLogo />);
  const circles = container.querySelectorAll("circle");
  expect(circles.length).toBe(2);
  const fills = Array.from(circles).map((c) => c.getAttribute("fill"));
  expect(fills).toContain("#a24bff");
  expect(fills).toContain("#3d7bff");
  expect(container.querySelector("polygon")).toHaveAttribute("fill", "none");
});
