import type { MonitorList, MonitorListItem } from "../api/endpoints";
import { filterMonitors, statusLine, statusText, summaryLine } from "./monitors";

const now = Date.parse("2026-10-20T14:30:00Z");

function item(over: Partial<MonitorListItem>): MonitorListItem {
  return {
    id: "m",
    name: "Website",
    url: "https://example.com",
    intervalSeconds: 60,
    timeoutSeconds: 10,
    expectedStatus: "2xx",
    failureThreshold: 2,
    notifySlack: true,
    paused: false,
    status: "up",
    statusSince: "2026-10-20T10:00:00Z",
    lastErrorKind: null,
    lastCheckedAt: "2026-10-20T14:29:20Z",
    tlsExpiresAt: null,
    ongoingIncident: null,
    uptime24h: 100,
    lastResponseMs: 120,
    ...over,
  };
}

describe("summaryLine", () => {
  it("counts monitors by state", () => {
    const list: MonitorList = {
      summary: { up: 1, down: 1, paused: 1, unknown: 0 },
      items: [item({ id: "a" }), item({ id: "b" }), item({ id: "c" })],
    };
    expect(summaryLine(list)).toBe("3 monitors · 1 up · 1 down · 1 paused");
  });

  it("uses the singular and adds pending monitors when there are some", () => {
    const list: MonitorList = {
      summary: { up: 0, down: 0, paused: 0, unknown: 1 },
      items: [item({ status: "unknown" })],
    };
    expect(summaryLine(list)).toBe("1 monitor · 0 up · 0 down · 0 paused · 1 pending");
  });
});

describe("filterMonitors", () => {
  const items = [
    item({ id: "1", name: "Website", url: "https://example.com/" }),
    item({ id: "2", name: "API", url: "https://api.example.org/health" }),
    item({ id: "3", name: "Billing", url: "https://pay.example.net/" }),
  ];

  it.each([
    ["", ["1", "2", "3"]],
    ["   ", ["1", "2", "3"]],
    ["web", ["1"]],
    ["HEALTH", ["2"]],
    [".example.", ["2", "3"]],
    ["nothing", []],
  ])("filter %j keeps %j", (query, ids) => {
    expect(filterMonitors(items, query).map((m) => m.id)).toEqual(ids);
  });
});

describe("statusText", () => {
  it("shows the down duration and plain labels otherwise", () => {
    expect(statusText(item({ status: "down", statusSince: "2026-10-20T14:18:00Z" }), now)).toBe(
      "Down 12 min",
    );
    expect(statusText(item({ status: "up" }), now)).toBe("Up");
    expect(statusText(item({ status: "unknown" }), now)).toBe("Pending");
    expect(statusText(item({ status: "down", paused: true }), now)).toBe("Paused");
  });
});

describe("statusLine", () => {
  const down = {
    paused: false,
    status: "down" as const,
    statusSince: "2026-10-20T14:18:00Z",
    lastErrorKind: "unexpected_status",
  };

  it("uses the HTTP code of the latest failed check", () => {
    const check = {
      checkedAt: "2026-10-20T14:29:00Z",
      location: "home",
      ok: false,
      httpStatus: 503,
      responseMs: 80,
      errorKind: "unexpected_status",
    };
    expect(statusLine(down, check, now)).toBe("Down for 12 min · HTTP 503 · since 14:18");
  });

  it("falls back to the error kind", () => {
    expect(statusLine({ ...down, lastErrorKind: "timeout" }, undefined, now)).toBe(
      "Down for 12 min · Timeout · since 14:18",
    );
  });

  it("describes up, unknown and paused monitors", () => {
    expect(statusLine({ ...down, status: "up" }, undefined, now)).toBe(
      "Up for 12 min · since 14:18",
    );
    expect(statusLine({ ...down, status: "unknown" }, undefined, now)).toBe(
      "Waiting for the first check",
    );
    expect(statusLine({ ...down, paused: true }, undefined, now)).toMatch(/^Paused/);
  });
});
