import { render, screen } from "@testing-library/react";
import { UNLIMITED } from "../../lib/limits";
import { Meter } from "./Data";

describe("Meter", () => {
  it("shows a limit without a quota as Unlimited, without a bar or a warning", () => {
    render(<Meter label="Monitors" used={1200} limit={UNLIMITED} />);
    expect(screen.getByText("/ Unlimited")).toBeInTheDocument();
    expect(screen.queryByText(String(UNLIMITED), { exact: false })).toBeNull();
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.queryByText("Limit reached")).toBeNull();
  });

  it("keeps the bar and the warning for a real limit", () => {
    render(<Meter label="Monitors" used={3} limit={3} />);
    expect(screen.getByRole("meter", { name: "Monitors" })).toBeInTheDocument();
    expect(screen.getByText("Limit reached")).toBeInTheDocument();
  });
});
