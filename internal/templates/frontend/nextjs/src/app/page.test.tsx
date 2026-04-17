import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import Page from "./page";

describe("Home", () => {
  it("renders", () => {
    const { container } = render(<Page />);
    expect(container).toBeTruthy();
  });
});
