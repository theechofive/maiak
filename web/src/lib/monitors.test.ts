import type { MonitorList, MonitorListItem } from "../api/endpoints";
import {
  checkSettings,
  downReason,
  failureStreak,
  filterMonitors,
  monitorState,
  quietState,
  sortMonitors,
  statusLine,
  statusText,
  summaryLine,
  summaryParts,
} from "./monitors";

const now = Date.parse("2026-10-20T14:30:00Z");

function item(over: Partial<MonitorListItem>): MonitorListItem {
  return {
    id: "m",
    projectId: "p-default",
    name: "Website",
    type: "http",
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
    lastPingAt: null,
    expectedIntervalSeconds: null,
    graceSeconds: null,
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

describe("monitorState", () => {
  it("prefers paused, maps unknown to pending", () => {
    expect(monitorState(item({ paused: true, status: "down" }))).toBe("paused");
    expect(monitorState(item({ status: "unknown" }))).toBe("pending");
    expect(monitorState(item({ status: "down" }))).toBe("down");
  });
});

describe("summaryParts", () => {
  it("marks the down part only when something is down", () => {
    const list = (down: number): MonitorList => ({
      summary: { up: 1, down, paused: 0, unknown: 0 },
      items: [item({})],
    });
    expect(summaryParts(list(0)).some((p) => p.down)).toBe(false);
    expect(summaryParts(list(2)).filter((p) => p.down)).toEqual([{ text: "2 down", down: true }]);
  });
});

describe("sortMonitors", () => {
  it("puts the newest failure first, paused last and the rest by name", () => {
    const sorted = sortMonitors([
      item({ name: "Zeta" }),
      item({ name: "Old outage", status: "down", statusSince: "2026-10-20T10:00:00Z" }),
      item({ name: "Alpha", paused: true }),
      item({ name: "Beta", status: "unknown" }),
      item({ name: "New outage", status: "down", statusSince: "2026-10-20T14:00:00Z" }),
      item({ name: "Paused while down", paused: true, status: "down" }),
    ]);
    expect(sorted.map((m) => m.name)).toEqual([
      "New outage",
      "Old outage",
      "Beta",
      "Zeta",
      "Alpha",
      "Paused while down",
    ]);
  });
});

describe("quietState", () => {
  const list = (items: MonitorListItem[], down = 0, unknown = 0): MonitorList => ({
    summary: { up: items.length, down, paused: 0, unknown },
    items,
  });

  it("is quiet only when nothing is down", () => {
    expect(quietState(list([item({ status: "down" })], 1))).toBeNull();
    expect(quietState(list([item({})]))?.title).toBe("Everything is up.");
  });

  it("does not claim everything is up while some are pending, or when all are paused", () => {
    expect(quietState(list([item({ status: "unknown" })], 0, 1))?.title).toBe("Nothing is down.");
    expect(quietState(list([item({ paused: true })]))).toBeNull();
  });

  it("names the lowest uptime when some checks failed", () => {
    expect(quietState(list([item({ uptime24h: 100 }), item({ uptime24h: null })]))?.detail).toBe(
      "No failed checks in the last 24 hours.",
    );
    expect(
      quietState(list([item({ name: "API", uptime24h: 99.2 }), item({ uptime24h: 99.9 })]))?.detail,
    ).toBe("Lowest uptime in the last 24 hours: 99.2%, API.");
  });
});

describe("downReason", () => {
  it("states the timeout and falls back to the error kind", () => {
    expect(downReason(item({ lastErrorKind: "timeout", timeoutSeconds: 10 }))).toBe(
      "Timed out after 10 s",
    );
    expect(downReason(item({ lastErrorKind: "dns" }))).toBe("DNS lookup failed");
  });
});

describe("failureStreak", () => {
  const check = (ok: boolean, at: string) => ({
    checkedAt: at,
    location: "home",
    ok,
    httpStatus: ok ? 200 : 503,
    responseMs: 100,
    errorKind: ok ? null : "unexpected_status",
  });

  it("counts failures before the newest success", () => {
    const streak = failureStreak([check(false, "3"), check(false, "2"), check(true, "1")]);
    expect(streak.count).toBe(2);
    expect(streak.all).toBe(false);
    expect(streak.lastSuccess?.checkedAt).toBe("1");
  });

  it("says when every loaded check failed", () => {
    expect(failureStreak([check(false, "2"), check(false, "1")])).toEqual({ count: 2, all: true });
  });
});

describe("checkSettings", () => {
  it("describes the HTTP check", () => {
    expect(checkSettings(item({}))).toBe(
      "every 60 s · 10 s timeout · expects 2xx · alerts after 2 failed checks",
    );
    expect(checkSettings(item({ expectedStatus: "204", failureThreshold: 1 }))).toBe(
      "every 60 s · 10 s timeout · expects HTTP 204 · alerts on the first failure",
    );
  });
});
