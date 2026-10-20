import { ApiError } from "../api/client";
import {
  emptyMonitorForm,
  mapServerErrors,
  toMonitorRequest,
  validateMonitorForm,
  validateMonitorUrl,
} from "./monitorForm";
import { validateSlackUrl } from "./slack";

describe("validateMonitorUrl", () => {
  it.each([
    ["https://example.com/health", undefined],
    ["https://example.com:443/health", undefined],
    ["  https://example.com  ", undefined],
    ["", "Enter a URL."],
    ["example.com", "Enter a full URL, such as https://example.com/health."],
    ["http://example.com", "Use an https:// URL."],
    ["ftp://example.com", "Use an https:// URL."],
    ["https://user:pass@example.com", "Remove the user name and password from the URL."],
    ["https://user@example.com", "Remove the user name and password from the URL."],
    ["https://example.com:8443/", "Only port 443 is allowed."],
    [`https://example.com/${"a".repeat(2048)}`, "Use 2048 characters or fewer."],
  ])("%s -> %s", (url, expected) => {
    expect(validateMonitorUrl(url)).toBe(expected);
  });
});

describe("validateMonitorForm", () => {
  const valid = { ...emptyMonitorForm, name: "Website", url: "https://example.com" };

  it("accepts a valid form", () => {
    expect(validateMonitorForm(valid)).toEqual({});
  });

  it("checks the name length after trimming", () => {
    expect(validateMonitorForm({ ...valid, name: "   " }).name).toBe("Enter a name.");
    expect(validateMonitorForm({ ...valid, name: "a".repeat(80) }).name).toBeUndefined();
    expect(validateMonitorForm({ ...valid, name: "a".repeat(81) }).name).toBe(
      "Use 80 characters or fewer.",
    );
  });

  it("checks an exact expected status code", () => {
    const exact = { ...valid, expectedMode: "exact" as const };
    expect(validateMonitorForm({ ...exact, expectedCode: "204" })).toEqual({});
    expect(validateMonitorForm({ ...exact, expectedCode: "99" }).expectedStatus).toBeDefined();
    expect(validateMonitorForm({ ...exact, expectedCode: "301" }).expectedStatus).toBeDefined();
    expect(validateMonitorForm({ ...exact, expectedCode: "600" }).expectedStatus).toBeDefined();
    expect(validateMonitorForm({ ...valid, expectedCode: "x" })).toEqual({});
  });

  it.each(["2xx", "200", "204", "299", "400", "404", "599"])(
    "accepts expected status %s",
    (code) => {
      const exact = { ...valid, expectedMode: "exact" as const, expectedCode: code };
      expect(validateMonitorForm(exact)).toEqual({});
    },
  );

  it.each(["100", "101", "199", "300", "301", "399", "600", "20", "2000", "abc", ""])(
    "rejects expected status %j",
    (code) => {
      const exact = { ...valid, expectedMode: "exact" as const, expectedCode: code };
      expect(validateMonitorForm(exact).expectedStatus).toBe(
        "Use 2xx, a code from 200 to 299, or a code from 400 to 599",
      );
    },
  );

  it("counts the name in code points", () => {
    expect(validateMonitorForm({ ...valid, name: "😀".repeat(80) }).name).toBeUndefined();
    expect(validateMonitorForm({ ...valid, name: "😀".repeat(81) }).name).toBe(
      "Use 80 characters or fewer.",
    );
  });

  it("builds the request body", () => {
    expect(
      toMonitorRequest({ ...valid, name: " Site ", expectedMode: "exact", expectedCode: "204" }),
    ).toEqual({
      name: "Site",
      url: "https://example.com",
      expectedStatus: "204",
      failureThreshold: 2,
      notifySlack: true,
    });
  });
});

describe("mapServerErrors", () => {
  const apiError = (status: number, fieldErrors: { field: string; message: string }[]) =>
    new ApiError({ type: "x", title: "Validation failed", status, fieldErrors });

  it("puts known field errors next to fields", () => {
    expect(
      mapServerErrors(
        apiError(422, [
          { field: "url", message: "host resolves to a private address" },
          { field: "name", message: "is required" },
        ]),
      ),
    ).toEqual({
      fields: { url: "Host resolves to a private address", name: "Is required" },
    });
  });

  it("reports unknown fields in the general message", () => {
    const mapped = mapServerErrors(apiError(422, [{ field: "intervalSeconds", message: "60" }]));
    expect(mapped.general).toBe("intervalSeconds: 60");
  });

  it("uses the problem for errors without fields", () => {
    const limit = new ApiError({
      type: "x",
      title: "Conflict",
      status: 409,
      detail: "The monitor limit (50) is reached.",
    });
    expect(mapServerErrors(limit)).toEqual({
      fields: {},
      general: "The monitor limit (50) is reached.",
    });
  });
});

describe("validateSlackUrl", () => {
  it.each([
    ["https://hooks.slack.com/services/T0/B0/x", undefined],
    ["", "Enter the webhook URL."],
    ["https://hooks.slack.com/services/", expect.stringContaining("Incoming Webhook")],
    ["https://example.com/services/x", expect.stringContaining("Incoming Webhook")],
  ])("%s", (url, expected) => {
    expect(validateSlackUrl(url)).toEqual(expected);
  });
});
