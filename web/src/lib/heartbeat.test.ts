import { describeErrorKind } from "./format";
import {
  alertmanagerSnippet,
  expectationText,
  heartbeatStatusLine,
  repeatInterval,
  secretCommand,
} from "./heartbeat";
import {
  emptyMonitorForm,
  toCreateRequest,
  toMonitorRequest,
  validateMonitorForm,
} from "./monitorForm";

const now = Date.parse("2026-10-21T12:00:00Z");

describe("heartbeat helpers", () => {
  it("describes the expectation", () => {
    expect(expectationText({ expectedIntervalSeconds: 300, graceSeconds: 0 })).toBe(
      "Ping every 5 min, grace 0 s",
    );
  });

  it("states the status", () => {
    const base = { paused: false, statusSince: "2026-10-21T11:00:00Z", lastPingAt: null };
    expect(heartbeatStatusLine({ ...base, status: "unknown" }, now)).toBe(
      "Waiting for the first ping",
    );
    expect(
      heartbeatStatusLine({ ...base, status: "up", lastPingAt: "2026-10-21T11:59:20Z" }, now),
    ).toBe("Up for 1 h · last ping 40 s ago");
    expect(heartbeatStatusLine({ ...base, status: "down" }, now)).toMatch(
      /^Down for 1 h · no ping in time · since /,
    );
    expect(heartbeatStatusLine({ ...base, status: "down", paused: true }, now)).toMatch(/^Paused/);
  });

  it("repeats well inside the interval and keeps the URL out of the config", () => {
    expect(repeatInterval(60)).toBe("30s");
    expect(repeatInterval(300)).toBe("150s");
    expect(repeatInterval(61)).toBe("30s");
    const snippet = alertmanagerSnippet(900);
    expect(snippet).toContain("repeat_interval: 450s");
    expect(snippet).toContain("send_resolved: false");
    expect(snippet).toContain("group_wait: 0s");
    expect(secretCommand("https://x/api/v1/heartbeat/t")).toContain(
      "--from-literal=url='https://x/api/v1/heartbeat/t'",
    );
  });

  it("labels the missed-heartbeat reason", () => {
    expect(describeErrorKind("heartbeat_missed")).toBe("Heartbeat missed");
  });
});

describe("watchdog form", () => {
  const valid = { ...emptyMonitorForm, type: "heartbeat" as const, name: "AM" };

  it.each([
    ["60", "0", true],
    ["86400", "3600", true],
    ["59", "0", false],
    ["86401", "0", false],
    ["300", "-1", false],
    ["300", "3601", false],
    ["300.5", "0", false],
    ["", "0", false],
  ])("interval %s grace %s valid=%s", (interval, grace, ok) => {
    const errors = validateMonitorForm({
      ...valid,
      expectedIntervalSeconds: interval,
      graceSeconds: grace,
    });
    expect(Object.keys(errors).length === 0).toBe(ok);
  });

  it("does not require a URL and builds type-specific bodies", () => {
    expect(validateMonitorForm({ ...valid, url: "" })).toEqual({});
    expect(toCreateRequest(valid)).toEqual({
      type: "heartbeat",
      name: "AM",
      expectedIntervalSeconds: 300,
      graceSeconds: 60,
      notifySlack: true,
    });
    expect(toMonitorRequest(valid)).not.toHaveProperty("type");
    expect(
      toCreateRequest({ ...emptyMonitorForm, name: "S", url: "https://e.com" }),
    ).not.toHaveProperty("type");
  });
});
