import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import Page from "./page";

describe("DashboardPage", () => {
  it("renders the dashboard heading", () => {
    render(<Page />);
    expect(screen.getByRole("heading", { name: "Dashboard" })).toBeTruthy();
  });
});
