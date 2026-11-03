import { applyTheme, readTheme } from "./theme";

afterEach(() => {
  applyTheme("system");
  localStorage.clear();
});

describe("theme", () => {
  it("follows the system until a choice is saved", () => {
    expect(readTheme()).toBe("system");
    applyTheme("dark");
    expect(readTheme()).toBe("dark");
    expect(document.documentElement).toHaveClass("theme-dark");
  });

  it("switches classes and forgets the choice for system", () => {
    applyTheme("dark");
    applyTheme("light");
    expect(document.documentElement).toHaveClass("theme-light");
    expect(document.documentElement).not.toHaveClass("theme-dark");
    applyTheme("system");
    expect(document.documentElement.className).toBe("");
    expect(localStorage.getItem("maiak-theme")).toBeNull();
  });

  it("ignores an unknown saved value", () => {
    localStorage.setItem("maiak-theme", "sepia");
    expect(readTheme()).toBe("system");
  });
});
