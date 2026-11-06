import {
  accentError,
  embedSnippets,
  formatUptime,
  groupComponents,
  hasInvisible,
  logoError,
  move,
  passwordError,
  slugError,
  slugFrom,
} from "./statusPage";

describe("status page rules", () => {
  it("checks slugs like the server, reserved words included", () => {
    for (const ok of ["acme", "acme-status", "a1b"]) expect(slugError(ok)).toBeUndefined();
    for (const bad of ["", "ab", "-acme", "Acme", "a_b", "acme.json", "status", "api", "admin"])
      expect(slugError(bad)).toBeDefined();
    expect(slugFrom("Acme Status!")).toBe("acme-status");
    expect(slugFrom("ä")).toBe("");
  });

  it("accepts only #rrggbb accents and https logos", () => {
    expect(accentError("")).toBeUndefined();
    expect(accentError("#1F6FEB")).toBeUndefined();
    for (const bad of ["red", "#fff", "#1234567", "#12345g;}"])
      expect(accentError(bad)).toBeDefined();
    expect(logoError("")).toBeUndefined();
    expect(logoError("https://cdn.example.com/logo.svg")).toBeUndefined();
    for (const bad of [
      "http://cdn.example.com/a.png",
      "javascript:alert(1)",
      "https://u:p@cdn.example.com/a.png",
      'https://cdn.example.com/a.png" onerror="x',
      "not a url",
    ])
      expect(logoError(bad)).toBeDefined();
  });

  it("refuses invisible formatting characters like the server", () => {
    expect(hasInvisible("API\u202egnp.exe")).toBe(true);
    expect(hasInvisible("Pay\u200bments")).toBe(true);
    expect(hasInvisible("Zürich – 東京 🚀")).toBe(false);
  });

  it("requires 12 characters for a password", () => {
    expect(passwordError("short")).toBeDefined();
    expect(passwordError("twelve chars")).toBeUndefined();
  });

  it("groups consecutive components and moves rows", () => {
    const g = groupComponents([
      { group: "Core", n: 1 },
      { group: "Core", n: 2 },
      { group: null, n: 3 },
      { group: "Core", n: 4 },
    ]);
    expect(g.map((x) => [x.name, x.items.length])).toEqual([
      ["Core", 2],
      [null, 1],
      ["Core", 1],
    ]);
    expect(move([1, 2, 3], 0, 1)).toEqual([2, 1, 3]);
    expect(move([1, 2, 3], 0, -1)).toEqual([1, 2, 3]);
    expect(move([1, 2, 3], 2, 1)).toEqual([1, 2, 3]);
  });

  it("formats uptime and embed snippets", () => {
    expect(formatUptime(null)).toBe("No data");
    expect(formatUptime(99.996)).toBe("99.99 % uptime");
    expect(formatUptime(100)).toBe("100.00 % uptime");
    const s = embedSnippets("https://maiak.example.com/status/acme");
    expect(s.script).toBe(
      '<script src="https://maiak.example.com/status/acme/embed.js" async></script>',
    );
    expect(s.iframe).toContain('src="https://maiak.example.com/status/acme/badge"');
  });
});
