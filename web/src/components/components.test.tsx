import { render, screen } from "@testing-library/react";
import ErrorBoundary from "./ErrorBoundary";

function Broken(): never {
  throw new Error("boom");
}

describe("ErrorBoundary", () => {
  it("replaces a crashed page with a readable message", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <ErrorBoundary>
        <Broken />
      </ErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByRole("button", { name: "Reload the page" })).toBeInTheDocument();
    spy.mockRestore();
  });
});
