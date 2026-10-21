import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MonitorDetail, MonitorListItem } from "../api/endpoints";
import { json, mockApi, problem, renderApp, signedIn } from "../test/utils";

const listItem = (over: Partial<MonitorListItem>): MonitorListItem => ({
  id: "m1",
  name: "Website",
  url: "https://example.com/",
  intervalSeconds: 60,
  timeoutSeconds: 10,
  expectedStatus: "2xx",
  failureThreshold: 2,
  notifySlack: true,
  paused: false,
  status: "up",
  statusSince: new Date(Date.now() - 3_600_000).toISOString(),
  lastErrorKind: null,
  lastCheckedAt: new Date(Date.now() - 40_000).toISOString(),
  tlsExpiresAt: null,
  ongoingIncident: null,
  uptime24h: 99.5,
  lastResponseMs: 230,
  ...over,
});

const detail = (over: Partial<MonitorDetail> = {}): MonitorDetail => {
  const base: Partial<MonitorListItem> = listItem({});
  delete base.uptime24h;
  delete base.lastResponseMs;
  return {
    ...(base as Omit<MonitorListItem, "uptime24h" | "lastResponseMs">),
    uptime: { h24: 99.5, d7: 99.9, d30: null },
    tlsExpiresAt: new Date(Date.now() + 20.5 * 86_400_000).toISOString(),
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-01T00:00:00Z",
    ...over,
  };
};

const usageOf = (monitors: number, maxMonitors: number, plan = { key: "free", name: "Free" }) => ({
  plan,
  limits: { monitors: maxMonitors, heartbeats: 1, channels: 1 },
  usage: { monitors, heartbeats: 0, channels: 1 },
});

describe("monitors list", () => {
  const list = {
    summary: { up: 1, down: 1, paused: 1, unknown: 0 },
    items: [
      listItem({ id: "a", name: "Website", url: "https://example.com/" }),
      listItem({
        id: "b",
        name: "API",
        url: "https://api.example.org/health",
        status: "down",
        statusSince: new Date(Date.now() - 12 * 60_000 - 5_000).toISOString(),
      }),
      listItem({ id: "c", name: "Billing", url: "https://pay.example.net/", paused: true }),
    ],
  };

  it("shows the summary, statuses and filters by name or URL", async () => {
    mockApi({ ...signedIn, "GET /api/v1/monitors": () => json(list) });
    renderApp("/");
    expect(await screen.findByText("3 monitors · 1 up · 1 down · 1 paused")).toBeInTheDocument();
    expect(screen.getByText("Down 12 min")).toBeInTheDocument();
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.getAllByText("40 s ago")).toHaveLength(3);
    expect(screen.getByText(/Last updated/)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("Filter by name or URL"), "health");
    const rows = within(screen.getByRole("table")).getAllByRole("row");
    expect(rows).toHaveLength(2); // header + API
    expect(within(rows[1]!).getByText("API")).toBeInTheDocument();

    await userEvent.clear(screen.getByLabelText("Filter by name or URL"));
    await userEvent.type(screen.getByLabelText("Filter by name or URL"), "zzz");
    expect(screen.getByText("No monitors match “zzz”.")).toBeInTheDocument();
  });

  it("opens the details when a row is clicked", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () => json(list),
      "GET /api/v1/monitors/a": () => json(detail({ id: "a", name: "Website" })),
    });
    renderApp("/");
    await userEvent.click(await screen.findByText("https://example.com/"));
    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
  });

  it("shows the empty state", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Start your first monitor" }));
    expect(screen.getByRole("button", { name: "Add monitor" })).toHaveAttribute(
      "href",
      "/monitors/new",
    );
  });

  it("replaces the add button at the plan's monitor limit", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () => json(list),
      "GET /api/v1/usage": () => json(usageOf(3, 3)),
    });
    renderApp("/");
    const note = await screen.findByText(
      "The Free plan allows 3 monitors. Delete a monitor to add a new one.",
    );
    const button = screen.getByRole("button", { name: "Add monitor" });
    expect(button).toHaveAttribute("aria-disabled", "true");
    expect(button).not.toHaveAttribute("href");
    expect(button).toHaveAccessibleDescription(note.textContent!);
    expect(screen.queryByRole("link", { name: "Add monitor" })).not.toBeInTheDocument();
  });

  it("keeps the add link below the limit and when usage cannot be loaded", async () => {
    let usage: Response | undefined = json(usageOf(2, 3));
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () => json(list),
      "GET /api/v1/usage": () => usage ?? problem(500, "Internal server error"),
    });
    const { unmount } = renderApp("/");
    expect(await screen.findByText("3 monitors · 1 up · 1 down · 1 paused")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Add monitor" })).toHaveAttribute(
        "href",
        "/monitors/new",
      ),
    );
    unmount();

    usage = undefined;
    renderApp("/");
    expect(await screen.findByText("3 monitors · 1 up · 1 down · 1 paused")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add monitor" })).toHaveAttribute(
      "href",
      "/monitors/new",
    );
  });

  it("explains the limit in the empty state of a plan without monitors", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
      "GET /api/v1/usage": () => json(usageOf(0, 0, { key: "none", name: "Suspended" })),
    });
    renderApp("/");
    expect(
      await screen.findByText(
        "The Suspended plan allows 0 monitors. Delete a monitor to add a new one.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add monitor" })).toHaveAttribute(
      "aria-disabled",
      "true",
    );
  });

  it("shows an error state instead of crashing when the API answers 501", async () => {
    mockApi({ ...signedIn }); // every other route answers 501
    renderApp("/");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Cannot load monitors");
    expect(alert).toHaveTextContent("This feature is not available yet.");
  });
});

describe("monitor form", () => {
  it("validates on the client without calling the API", async () => {
    const fetchMock = mockApi({ ...signedIn });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("URL"), "http://user:pw@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));

    expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Name")).toHaveAccessibleDescription("Enter a name.");
    expect(screen.getByLabelText("URL")).toHaveAccessibleDescription(
      "Use an https:// URL. HTTPS only. Private and internal addresses are not allowed.",
    );
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(false);
  });

  it("shows server field errors next to the fields", async () => {
    mockApi({
      ...signedIn,
      "POST /api/v1/monitors": () =>
        problem(422, "Validation failed", {
          fieldErrors: [{ field: "url", message: "host resolves to a private address" }],
        }),
    });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Internal");
    await userEvent.type(screen.getByLabelText("URL"), "https://intranet.example");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));

    await waitFor(() =>
      expect(screen.getByLabelText("URL")).toHaveAttribute("aria-invalid", "true"),
    );
    expect(screen.getByText("Host resolves to a private address")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("Check the highlighted fields.");
  });

  it("creates a monitor and opens it", async () => {
    let body: unknown;
    const created = detail({ id: "new1", name: "Website", status: "unknown" });
    mockApi({
      ...signedIn,
      "POST /api/v1/monitors": async (req) => {
        body = await req.json();
        return json(created, 201);
      },
      "GET /api/v1/monitors/new1": () => json(created),
    });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Website");
    await userEvent.type(screen.getByLabelText("URL"), "https://example.com/");
    await userEvent.click(screen.getByLabelText("Exact code"));
    await userEvent.clear(screen.getByLabelText("Status code"));
    await userEvent.type(screen.getByLabelText("Status code"), "204");
    await userEvent.selectOptions(screen.getByLabelText("Alert after"), "3");
    await userEvent.click(screen.getByLabelText("Notify Slack"));
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));

    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(body).toEqual({
      name: "Website",
      url: "https://example.com/",
      expectedStatus: "204",
      failureThreshold: 3,
      notifySlack: false,
    });
    expect(screen.getByRole("status")).toHaveTextContent("Monitor created");
  });

  it("explains a plan-limit refusal and refreshes the usage", async () => {
    let usageCalls = 0;
    mockApi({
      ...signedIn,
      "GET /api/v1/usage": () => {
        usageCalls++;
        return json(usageOf(3, 3));
      },
      "POST /api/v1/monitors": () =>
        problem(409, "Plan limit reached", {
          type: "urn:maiak:problem:plan-limit-reached",
          detail: "The Free plan allows at most 3 monitors.",
          limit: "monitors",
          max: 3,
        }),
    });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Website");
    await userEvent.type(screen.getByLabelText("URL"), "https://example.com/");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Your plan allows at most 3 monitors, and the limit is reached. Delete a monitor to add a new one.",
    );
    // The form does not show usage, so the invalidated query is only marked stale.
    expect(usageCalls).toBe(0);
  });

  it("rejects 1xx and 3xx expected status codes before calling the API", async () => {
    const fetchMock = mockApi({ ...signedIn });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Website");
    await userEvent.type(screen.getByLabelText("URL"), "https://example.com/");
    await userEvent.click(screen.getByLabelText("Exact code"));
    for (const code of ["101", "301"]) {
      await userEvent.clear(screen.getByLabelText("Status code"));
      await userEvent.type(screen.getByLabelText("Status code"), code);
      await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));
      expect(screen.getByLabelText("Status code")).toHaveAccessibleDescription(
        "Use 2xx, a code from 200 to 299, or a code from 400 to 599",
      );
    }
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(false);
  });

  it("shows the not-found view when the monitor to edit does not exist", async () => {
    mockApi({ ...signedIn, "GET /api/v1/monitors/gone": () => problem(404, "Not found") });
    renderApp("/monitors/gone/edit");
    expect(await screen.findByRole("heading", { name: "Monitor not found" })).toBeInTheDocument();
  });

  it("loads the monitor for editing", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail({ expectedStatus: "204" })),
    });
    renderApp("/monitors/m1/edit");
    expect(await screen.findByLabelText("Name")).toHaveValue("Website");
    expect(screen.getByLabelText("Status code")).toHaveValue("204");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeInTheDocument();
  });
});

describe("monitor details", () => {
  it("shows the monitor while stats, checks and incidents fail with 501", async () => {
    mockApi({ ...signedIn, "GET /api/v1/monitors/m1": () => json(detail()) });
    renderApp("/monitors/m1");
    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Up for 1 h · since", { exact: false })).toBeInTheDocument();
    expect(screen.getByText("99.5%")).toBeInTheDocument();
    expect(screen.getByText("in 20 days")).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByRole("alert")).toHaveLength(3));
    expect(screen.getByText("Cannot load response times")).toBeInTheDocument();
    expect(screen.getByText("Cannot load incidents")).toBeInTheDocument();
    expect(screen.getByText("Cannot load checks")).toBeInTheDocument();
  });

  it("draws the chart and switches the range", async () => {
    const ranges: string[] = [];
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail()),
      "GET /api/v1/monitors/m1/stats": (req) => {
        const range = new URL(req.url).searchParams.get("range") ?? "24h";
        ranges.push(range);
        return json({
          range,
          avgMs: 120,
          p95Ms: 300,
          series: [
            { t: "2026-10-20T10:00:00Z", avgMs: 100, okRatio: 1 },
            { t: "2026-10-20T11:00:00Z", avgMs: 140, okRatio: 1 },
          ],
        });
      },
      "GET /api/v1/monitors/m1/checks": () =>
        json({
          items: [
            {
              checkedAt: "2026-10-20T11:00:00Z",
              location: "home",
              ok: false,
              httpStatus: 503,
              responseMs: 80,
              errorKind: "unexpected_status",
            },
          ],
        }),
      "GET /api/v1/monitors/m1/incidents": () =>
        json({
          items: [
            {
              id: "i1",
              startedAt: "2026-10-20T10:00:00Z",
              resolvedAt: "2026-10-20T10:12:00Z",
              reason: "timeout",
            },
          ],
        }),
    });
    renderApp("/monitors/m1");
    expect(
      await screen.findByRole("img", { name: /Response time, last 24 hours/ }),
    ).toBeInTheDocument();
    expect(screen.getByText("Average 120 ms · p95 300 ms")).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "12 min" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Failed: Unexpected status" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "7 d" }));
    expect(
      await screen.findByRole("img", { name: /Response time, last 7 days/ }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "7 d" })).toHaveAttribute("aria-pressed", "true");
    expect(ranges).toEqual(["24h", "7d"]);
  });

  it("deletes only after confirmation", async () => {
    const fetchMock = mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail()),
      "DELETE /api/v1/monitors/m1": () => new Response(null, { status: 204 }),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    const deletes = () =>
      fetchMock.mock.calls.filter(([r]) => (r as Request).method === "DELETE").length;
    renderApp("/monitors/m1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    expect(screen.getByRole("dialog", { name: "Delete Website?" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(deletes()).toBe(0);

    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete monitor" }));
    expect(await screen.findByRole("heading", { name: "Start your first monitor" }));
    expect(deletes()).toBe(1);
    expect(screen.getByRole("status")).toHaveTextContent("Monitor deleted");
  });

  it("pauses a monitor", async () => {
    let paused = false;
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail({ paused })),
      "POST /api/v1/monitors/m1/pause": () => {
        paused = true;
        return json(detail({ paused }));
      },
    });
    renderApp("/monitors/m1");
    await userEvent.click(await screen.findByRole("button", { name: "Pause" }));
    expect(await screen.findByRole("button", { name: "Resume" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Monitor paused");
  });

  it("keeps the data and notes a failed refresh", async () => {
    let fail = false;
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => (fail ? problem(500, "Boom") : json(detail())),
    });
    renderApp("/monitors/m1");
    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(screen.queryByText("Last update failed")).not.toBeInTheDocument();

    fail = true;
    act(() => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
    expect(await screen.findByText("Last update failed")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(screen.getByText(/Last updated/)).toBeInTheDocument();
  });

  it("switches to the not-found view when a refresh answers 404 after data existed", async () => {
    let gone = false;
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => (gone ? problem(404, "Not found") : json(detail())),
    });
    renderApp("/monitors/m1");
    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();

    gone = true;
    act(() => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
    expect(await screen.findByRole("heading", { name: "Monitor not found" })).toBeInTheDocument();
  });

  it("refreshes the monitor list after a pause", async () => {
    let paused = false;
    const fetchMock = mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () =>
        json({
          summary: { up: paused ? 0 : 1, down: 0, paused: paused ? 1 : 0, unknown: 0 },
          items: [listItem({ id: "m1", paused })],
        }),
      "GET /api/v1/monitors/m1": () => json(detail({ paused })),
      "POST /api/v1/monitors/m1/pause": () => {
        paused = true;
        return json(detail({ paused }));
      },
    });
    const listCalls = () =>
      fetchMock.mock.calls.filter(
        ([r]) =>
          (r as Request).method === "GET" &&
          new URL((r as Request).url).pathname === "/api/v1/monitors",
      ).length;
    renderApp("/");
    expect(await screen.findByText("1 monitor · 1 up · 0 down · 0 paused")).toBeInTheDocument();
    await userEvent.click(screen.getByText("https://example.com/"));
    await userEvent.click(await screen.findByRole("button", { name: "Pause" }));
    expect(await screen.findByRole("button", { name: "Resume" })).toBeInTheDocument();

    const before = listCalls();
    await userEvent.click(screen.getByRole("link", { name: "Monitors" }));
    expect(await screen.findByText("1 monitor · 0 up · 0 down · 1 paused")).toBeInTheDocument();
    expect(listCalls()).toBeGreaterThan(before);
  });

  it("says when the monitor does not exist", async () => {
    mockApi({ ...signedIn, "GET /api/v1/monitors/gone": () => problem(404, "Not found") });
    renderApp("/monitors/gone");
    expect(await screen.findByRole("heading", { name: "Monitor not found" })).toBeInTheDocument();
  });
});

describe("settings", () => {
  it("shows the plan and the usage against its limits", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/settings/slack": () => json({ configured: true }),
      "GET /api/v1/usage": () => json(usageOf(2, 3)),
    });
    renderApp("/settings");
    expect(await screen.findByText(/Your tenant is on the/)).toHaveTextContent(
      "Your tenant is on the Free plan.",
    );
    const table = screen.getByRole("table");
    const rows = within(table)
      .getAllByRole("row")
      .map((r) => r.textContent);
    expect(rows).toEqual([
      "ResourceUsedLimit",
      "Monitors23",
      "Heartbeat monitors01",
      "Notification channels11 (limit reached)",
    ]);
  });

  it("shows an error for the plan without hiding the other settings", async () => {
    mockApi({ ...signedIn, "GET /api/v1/settings/slack": () => json({ configured: false }) });
    renderApp("/settings");
    expect(await screen.findByText("Cannot load plan and usage")).toBeInTheDocument();
    expect(screen.getByText(/No webhook is configured/)).toBeInTheDocument();
  });

  it("saves the webhook without showing it again", async () => {
    let configured = false;
    let sent: unknown;
    mockApi({
      ...signedIn,
      "GET /api/v1/settings/slack": () => json({ configured }),
      "PUT /api/v1/settings/slack": async (req) => {
        sent = await req.json();
        configured = true;
        return json({ configured: true });
      },
    });
    renderApp("/settings");
    expect(await screen.findByText(/No webhook is configured/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Send test message" })).toBeDisabled();
    expect(screen.getByText(/Signed in as/)).toHaveTextContent(
      "Signed in as admin. The password is managed outside the app.",
    );

    const input = screen.getByLabelText("Slack webhook URL");
    await userEvent.type(input, "https://hooks.slack.com/services/T0/B0/secret");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/A webhook is configured/)).toBeInTheDocument();
    expect(input).toHaveValue("");
    expect(sent).toEqual({ webhookUrl: "https://hooks.slack.com/services/T0/B0/secret" });
    expect(screen.getByRole("button", { name: "Send test message" })).toBeEnabled();
    expect(screen.getByRole("status")).toHaveTextContent("Webhook saved");
  });

  it("rejects a URL that is not a Slack webhook before sending it", async () => {
    mockApi({ ...signedIn, "GET /api/v1/settings/slack": () => json({ configured: false }) });
    renderApp("/settings");
    await userEvent.type(await screen.findByLabelText("Slack webhook URL"), "https://example.com");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText("Slack webhook URL")).toHaveAttribute("aria-invalid", "true");
  });

  it("sends no PUT for a rejected URL", async () => {
    const fetchMock = mockApi({
      ...signedIn,
      "GET /api/v1/settings/slack": () => json({ configured: false }),
    });
    renderApp("/settings");
    await userEvent.type(await screen.findByLabelText("Slack webhook URL"), "https://example.com");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText("Slack webhook URL")).toHaveAttribute("aria-invalid", "true");
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(false);
  });

  it("masks the webhook as text without autofill", async () => {
    mockApi({ ...signedIn, "GET /api/v1/settings/slack": () => json({ configured: false }) });
    renderApp("/settings");
    const input = await screen.findByLabelText("Slack webhook URL");
    expect(input).toHaveAttribute("type", "text");
    expect(input).toHaveAttribute("autocomplete", "off");
    expect(input).toHaveAttribute("spellcheck", "false");
    expect(input).toHaveClass("secret-input");
  });

  it("shows an error toast when the Slack test answers 409", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/settings/slack": () => json({ configured: true }),
      "POST /api/v1/settings/slack/test": () =>
        problem(409, "Conflict", { detail: "No Slack webhook is configured." }),
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Send test message" }));
    expect(await screen.findByText(/Test message failed/)).toHaveTextContent(
      "No Slack webhook is configured.",
    );
  });

  it("reports a failed test message in a toast", async () => {
    mockApi({ ...signedIn, "GET /api/v1/settings/slack": () => json({ configured: true }) });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Send test message" }));
    expect(await screen.findByText(/Test message failed/)).toBeInTheDocument();
  });
});
