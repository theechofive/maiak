import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import App from "./App";

describe("App routes", () => {
  it.each([
    ["/login", "Login"],
    ["/", "Monitors"],
    ["/monitors/new", "Add monitor"],
    ["/monitors/42", "Monitor details"],
    ["/monitors/42/edit", "Edit monitor"],
    ["/settings", "Settings"],
  ])("renders %s", (path, heading) => {
    render(
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>,
    );
    expect(screen.getByRole("heading", { name: heading })).toBeInTheDocument();
  });
});
