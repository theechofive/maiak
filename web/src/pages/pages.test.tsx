import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Channel, MonitorDetail, MonitorListItem } from "../api/endpoints";
import { json, mockApi, problem, renderApp, signedIn } from "../test/utils";

const listItem = (over: Partial<MonitorListItem>): MonitorListItem => ({
  id: "m1",
  name: "Website",
  type: "http",
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
  lastPingAt: null,
  expectedIntervalSeconds: null,
  graceSeconds: null,
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
    channelIds: [],
    tlsExpiresAt: new Date(Date.now() + 20.5 * 86_400_000).toISOString(),
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-01T00:00:00Z",
    ...over,
  };
};

const channel = (over: Partial<Channel> = {}): Channel => ({
  id: "c1",
  name: "#ops",
  type: "slack_webhook",
  events: ["down", "recovered", "tls_expiring"],
  mention: [],
  reminderMinutes: 0,
  status: "active",
  createdAt: "2026-10-01T00:00:00Z",
  updatedAt: "2026-10-01T00:00:00Z",
  ...over,
});

const usageOf = (
  monitors: number,
  maxMonitors: number,
  plan = { key: "free", name: "Free" },
  heartbeats = 0,
  maxHeartbeats = 1,
) => ({
  plan,
  limits: { monitors: maxMonitors, heartbeats: maxHeartbeats, channels: 1, members: 3 },
  usage: { monitors, heartbeats, channels: 1, members: 1 },
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

  it("puts the newest outage first and works from the keyboard", async () => {
    mockApi({ ...signedIn, "GET /api/v1/monitors": () => json(list) });
    renderApp("/");
    const filter = await screen.findByLabelText("Filter by name or URL");
    const links = within(screen.getByRole("table")).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["API", "Website", "Billing"]);
    expect(document.title).toBe("▲ 1 down · Maiak");

    await userEvent.keyboard("/");
    expect(filter).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(links[0]).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(links[1]).toHaveFocus();
    await userEvent.keyboard("{ArrowUp}{ArrowUp}");
    expect(filter).toHaveFocus();

    await userEvent.keyboard("web");
    expect(screen.getByText("API is down and hidden by this filter.")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(filter).toHaveValue("");
  });

  it("opens the details when a row is clicked", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () => json(list),
      "GET /api/v1/monitors/a": () => json(detail({ id: "a", name: "Website" })),
    });
    renderApp("/");
    await userEvent.click(await screen.findByText("example.com/"));
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
    expect(screen.getByRole("link", { name: "Add monitor" })).toHaveAttribute(
      "href",
      "/monitors/new",
    );
  });

  it("replaces the add button when both of the plan's limits are reached", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () => json(list),
      "GET /api/v1/usage": () => json(usageOf(3, 3, undefined, 1, 1)),
    });
    renderApp("/");
    const note = await screen.findByText(
      "The Free plan allows 3 monitors and 1 watchdog. Delete one to add a new one.",
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
      expect(screen.getByRole("link", { name: "Add monitor" })).toHaveAttribute(
        "href",
        "/monitors/new",
      ),
    );
    unmount();

    usage = undefined;
    renderApp("/");
    expect(await screen.findByText("3 monitors · 1 up · 1 down · 1 paused")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add monitor" })).toHaveAttribute(
      "href",
      "/monitors/new",
    );
  });

  it("explains the limit in the empty state of a plan without monitors", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
      "GET /api/v1/usage": () => json(usageOf(0, 0, { key: "none", name: "Suspended" }, 0, 0)),
    });
    renderApp("/");
    expect(
      await screen.findByText(
        "The Suspended plan allows 0 monitors and 0 watchdogs. Delete one to add a new one.",
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
    expect(screen.getByLabelText("URL")).toHaveAccessibleDescription("Use an https:// URL.");
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
    await userEvent.click(screen.getByRole("radio", { name: "3" }));
    // The plain-English summary is split into spans; match its whole paragraph.
    expect(
      screen.getByText(
        (_, el) =>
          el?.tagName === "P" &&
          el.textContent ===
            "Checks example.com every minute, expects HTTP 204, and alerts Slack after 3 failures in a row.",
      ),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("Notify Slack"));
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));

    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(body).toEqual({
      name: "Website",
      url: "https://example.com/",
      expectedStatus: "204",
      failureThreshold: 3,
      notifySlack: false,
      channelIds: [],
    });
    expect(screen.getByRole("status")).toHaveTextContent("Monitor created");
  });

  it("shows a server error about Slack under the Notify Slack choice", async () => {
    mockApi({
      ...signedIn,
      "POST /api/v1/monitors": () =>
        problem(422, "Validation failed", {
          fieldErrors: [{ field: "notifySlack", message: "no Slack webhook is configured" }],
        }),
    });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Website");
    await userEvent.type(screen.getByLabelText("URL"), "example.com");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));
    const checkbox = await screen.findByLabelText("Notify Slack");
    await waitFor(() => expect(checkbox).toHaveAttribute("aria-invalid", "true"));
    expect(checkbox).toHaveAccessibleDescription(
      expect.stringContaining("No Slack webhook is configured"),
    );
  });

  it("selects notification channels for a monitor", async () => {
    let body: unknown;
    const created = detail({ id: "new2", name: "Shop", channelIds: ["c2"] });
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () =>
        json({ items: [channel(), channel({ id: "c2", name: "#on-call", status: "failed" })] }),
      "POST /api/v1/monitors": async (req) => {
        body = await req.json();
        return json(created, 201);
      },
      "GET /api/v1/monitors/new2": () => json(created),
    });
    renderApp("/monitors/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Shop");
    await userEvent.type(screen.getByLabelText("URL"), "shop.example.com");
    const picker = await screen.findByRole("group", { name: "Notification channels" });
    expect(picker).toHaveAccessibleDescription(/every active channel/);
    expect(within(picker).getByLabelText("#on-call")).toHaveAccessibleDescription(/Webhook failed/);
    await userEvent.click(within(picker).getByLabelText("#on-call"));
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));
    await screen.findByRole("heading", { name: "Shop", level: 1 });
    expect(body).toMatchObject({ name: "Shop", notifySlack: true, channelIds: ["c2"] });
  });

  it("hides the channel picker when Slack is off and links to Settings without channels", async () => {
    mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/monitors/new");
    expect(await screen.findByText(/No notification channels yet/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("Notify Slack"));
    expect(screen.queryByText(/No notification channels yet/)).not.toBeInTheDocument();
  });

  it("drops a selected channel that was deleted before saving", async () => {
    let body: unknown;
    const current = detail({ id: "m7", name: "Shop", channelIds: ["gone", "c1"] });
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: [channel()] }),
      "GET /api/v1/monitors/m7": () => json(current),
      "PATCH /api/v1/monitors/m7": async (req) => {
        body = await req.json();
        return json(current);
      },
    });
    renderApp("/monitors/m7/edit");
    const picker = await screen.findByRole("group", { name: "Notification channels" });
    expect(within(picker).getByLabelText("#ops")).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(body).toMatchObject({ channelIds: ["c1"] }));
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
    // The form shows which type is full, so the stale usage is fetched again.
    await waitFor(() => expect(usageCalls).toBeGreaterThanOrEqual(2));
    expect(screen.getByText("Your plan's HTTP monitor limit is reached.")).toBeInTheDocument();
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
      await screen.findByRole("group", { name: /Response time, last 24 hours/ }),
    ).toBeInTheDocument();
    expect(screen.getByText("Average 120 ms · p95 300 ms")).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "12 min" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Unexpected status" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Failed" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("radio", { name: "7d" }));
    expect(
      await screen.findByRole("group", { name: /Response time, last 7 days/ }),
    ).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "7d" })).toBeChecked();
    expect(ranges).toEqual(["24h", "7d"]);
  });

  it("explains an outage from the latest checks and warns about the TLS certificate", async () => {
    const at = (min: number) => new Date(Date.now() - min * 60_000).toISOString();
    const check = (min: number, ok: boolean) => ({
      checkedAt: at(min),
      location: "home",
      ok,
      httpStatus: null,
      responseMs: ok ? 120 : null,
      errorKind: ok ? null : "timeout",
    });
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () =>
        json(
          detail({
            status: "down",
            statusSince: at(3),
            lastErrorKind: "timeout",
            tlsExpiresAt: new Date(Date.now() + 9.5 * 86_400_000).toISOString(),
          }),
        ),
      "GET /api/v1/monitors/m1/checks": () =>
        json({ items: [check(1, false), check(2, false), check(3, false), check(4, true)] }),
    });
    renderApp("/monitors/m1");
    const brief = await screen.findByRole("region", { name: "Ongoing incident" });
    expect(brief).toHaveTextContent("Timed out after 10 s");
    expect(
      await within(brief).findByText(/3 failed checks in a row\. Last success at/),
    ).toBeInTheDocument();
    expect(screen.getByText(/The TLS certificate expires in 9 days/)).toBeInTheDocument();
  });

  it("says a paused monitor is paused, not new, when its range is empty", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail({ paused: true, status: "unknown" })),
      "GET /api/v1/monitors/m1/stats": () =>
        json({
          range: "24h",
          avgMs: null,
          p95Ms: null,
          series: [{ t: "2026-10-20T10:00:00Z", avgMs: null, okRatio: null }],
        }),
    });
    renderApp("/monitors/m1");
    expect(await screen.findByText("Paused: no checks ran in this range.")).toBeInTheDocument();
    expect(screen.queryByText(/No checks yet/)).not.toBeInTheDocument();
  });

  it("keeps the labels of the shown range until the new range arrives", async () => {
    let release: (() => void) | undefined;
    const stats = (range: string) => ({
      range,
      avgMs: 120,
      p95Ms: 300,
      series: [{ t: "2026-10-20T10:00:00Z", avgMs: 100, okRatio: 1 }],
    });
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/m1": () => json(detail()),
      "GET /api/v1/monitors/m1/stats": (req) => {
        const range = new URL(req.url).searchParams.get("range") ?? "24h";
        if (range === "24h") return json(stats(range));
        return new Promise<Response>((resolve) => (release = () => resolve(json(stats(range)))));
      },
    });
    renderApp("/monitors/m1");
    await screen.findByRole("group", { name: /last 24 hours/ });
    await userEvent.click(screen.getByRole("radio", { name: "7d" }));
    expect(screen.getByRole("radio", { name: "7d" })).toBeChecked();
    expect(screen.getByRole("group", { name: /last 24 hours/ })).toBeInTheDocument();
    expect(screen.getByText("average per 15 min bucket")).toBeInTheDocument();
    await waitFor(() => expect(release).toBeDefined());
    act(() => release!());
    expect(await screen.findByRole("group", { name: /last 7 days/ })).toBeInTheDocument();
    expect(screen.getByText("average per hour bucket")).toBeInTheDocument();
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
    await userEvent.click(screen.getByText("example.com/"));
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
      "GET /api/v1/channels": () => json({ items: [channel()] }),
      "GET /api/v1/usage": () => json(usageOf(2, 3)),
    });
    renderApp("/settings");
    expect(await screen.findByText(/Your tenant is on the/)).toHaveTextContent(
      "Your tenant is on the Free plan.",
    );
    const meters = screen.getAllByRole("meter");
    expect(
      meters.map((m) => [
        m.getAttribute("aria-label"),
        m.getAttribute("aria-valuenow"),
        m.getAttribute("aria-valuemax"),
      ]),
    ).toEqual([
      ["Monitors", "2", "3"],
      ["Heartbeat monitors", "0", "1"],
      ["Notification channels", "1", "1"],
      ["Members and open invitations", "1", "3"],
    ]);
    expect(screen.getByText("Limit reached")).toBeInTheDocument();
  });

  it("shows an error for the plan without hiding the other settings", async () => {
    mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/settings");
    expect(await screen.findByText("Cannot load plan and usage")).toBeInTheDocument();
    expect(screen.getByText(/No notification channels yet/)).toBeInTheDocument();
    expect(screen.getByText(/Signed in as/)).toHaveTextContent(
      "Signed in as admin. The password is managed outside the app.",
    );
  });

  it("lists channels with their events, mention, reminder and status", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () =>
        json({
          items: [
            channel(),
            channel({
              id: "c2",
              name: "#on-call",
              events: ["down"],
              mention: ["@here"],
              reminderMinutes: 60,
              status: "failed",
            }),
          ],
        }),
    });
    renderApp("/settings");
    const oncall = await screen.findByRole("row", { name: /#on-call/ });
    expect(oncall).toHaveTextContent("#on-callDown@hereEvery 1 hFailed");
    expect(screen.getByRole("row", { name: /#ops/ })).toHaveTextContent(
      "#opsDown, Recovered, TLS certificate expiringNoneOffActive",
    );
    expect(screen.getByText(/refused by Slack/)).toBeInTheDocument();
  });

  it("adds a channel without showing the webhook again", async () => {
    let items: Channel[] = [];
    let sent: unknown;
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items }),
      "POST /api/v1/channels": async (req) => {
        sent = await req.json();
        items = [channel({ name: "#alerts", mention: ["@here"], reminderMinutes: 30 })];
        return json(items[0], 201);
      },
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Add channel" }));
    const url = screen.getByLabelText("Slack webhook URL");
    expect(url).toHaveAttribute("type", "text");
    expect(url).toHaveAttribute("autocomplete", "off");
    expect(url).toHaveAttribute("spellcheck", "false");
    expect(url).toHaveClass("secret-text");
    expect(screen.getByLabelText("Name")).toHaveFocus();
    await userEvent.type(screen.getByLabelText("Name"), " #alerts ");
    await userEvent.type(url, "https://hooks.slack.com/services/T0/B0/secret");
    await userEvent.click(screen.getByLabelText("TLS certificate expiring"));
    await userEvent.type(screen.getByLabelText("Mention (optional)"), "@here @here");
    await userEvent.clear(screen.getByLabelText("Reminder every (minutes)"));
    await userEvent.type(screen.getByLabelText("Reminder every (minutes)"), "30");
    await userEvent.click(screen.getByRole("button", { name: "Add channel" }));

    expect(await screen.findByRole("row", { name: /#alerts/ })).toBeInTheDocument();
    expect(sent).toEqual({
      name: "#alerts",
      webhookUrl: "https://hooks.slack.com/services/T0/B0/secret",
      events: ["down", "recovered"],
      mention: ["@here"],
      reminderMinutes: 30,
    });
    expect(screen.queryByLabelText("Slack webhook URL")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Channel #alerts added");
  });

  it("checks the form before sending it", async () => {
    const fetchMock = mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Add channel" }));
    await userEvent.type(screen.getByLabelText("Name"), "x");
    await userEvent.type(screen.getByLabelText("Slack webhook URL"), "https://example.com");
    await userEvent.type(screen.getByLabelText("Mention (optional)"), "@everyone");
    await userEvent.clear(screen.getByLabelText("Reminder every (minutes)"));
    await userEvent.type(screen.getByLabelText("Reminder every (minutes)"), "3");
    await userEvent.click(screen.getByRole("button", { name: "Add channel" }));
    for (const label of ["Slack webhook URL", "Mention (optional)", "Reminder every (minutes)"]) {
      expect(screen.getByLabelText(label)).toHaveAttribute("aria-invalid", "true");
    }
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(false);
  });

  it("disables adding at the plan limit and explains a refused create", async () => {
    let limit = 1;
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: limit === 1 ? [channel()] : [] }),
      "GET /api/v1/usage": () => json(usageOf(0, 3)),
      "POST /api/v1/channels": () =>
        problem(409, "Plan limit reached", {
          type: "urn:maiak:problem:plan-limit-reached",
          detail: "The Free plan allows at most 1 channels.",
          limit: "channels",
          max: 1,
        }),
    });
    const { queryClient } = renderApp("/settings");
    expect(await screen.findByRole("button", { name: "Add channel" })).toBeDisabled();
    expect(screen.getByText(/limit is reached/)).toBeInTheDocument();

    // Usage that is out of date: the create is refused by the server.
    limit = 0;
    await act(() => queryClient.invalidateQueries({ queryKey: ["channels"] }));
    queryClient.setQueryData(["usage"], {
      ...usageOf(0, 3),
      usage: { monitors: 0, heartbeats: 0, channels: 0, members: 1 },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Add channel" }));
    await userEvent.type(screen.getByLabelText("Name"), "x");
    await userEvent.type(
      screen.getByLabelText("Slack webhook URL"),
      "https://hooks.slack.com/services/T0/B0/x",
    );
    await userEvent.click(screen.getByRole("button", { name: "Add channel" }));
    expect(
      await screen.findByText(/Your plan allows at most 1 notification channel/),
    ).toBeInTheDocument();
  });

  it("edits a channel and keeps the stored webhook unless it is replaced", async () => {
    let sent: unknown;
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: [channel()] }),
      "PATCH /api/v1/channels/c1": async (req) => {
        sent = await req.json();
        return json(channel({ name: "#ops2" }));
      },
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Edit #ops" }));
    expect(screen.queryByLabelText(/webhook URL/)).not.toBeInTheDocument();
    expect(screen.getByText("Configured, hidden")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Name"));
    await userEvent.type(screen.getByLabelText("Name"), "#ops2");
    await userEvent.click(screen.getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Changes saved"));
    expect(sent).toEqual({
      name: "#ops2",
      events: ["down", "recovered", "tls_expiring"],
      mention: [],
      reminderMinutes: 0,
    });
  });

  it("replaces the webhook of a channel", async () => {
    let sent: unknown;
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: [channel({ status: "failed" })] }),
      "PATCH /api/v1/channels/c1": async (req) => {
        sent = await req.json();
        return json(channel());
      },
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Edit #ops" }));
    await userEvent.click(screen.getByRole("button", { name: "Replace" }));
    const url = screen.getByLabelText("New Slack webhook URL");
    expect(url).toHaveFocus();
    await userEvent.type(url, "https://hooks.slack.com/services/T0/B0/new");
    await userEvent.click(screen.getByRole("button", { name: "Save channel" }));
    await waitFor(() =>
      expect(sent).toMatchObject({ webhookUrl: "https://hooks.slack.com/services/T0/B0/new" }),
    );
  });

  it("shows a delivered test and a failed one", async () => {
    let fail = false;
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: [channel()] }),
      "POST /api/v1/channels/c1/test": () =>
        fail
          ? problem(429, "Too many attempts", { detail: "Try again later." })
          : new Response(null, { status: 204 }),
    });
    renderApp("/settings");
    await userEvent.click(
      await screen.findByRole("button", { name: "Send a test message to #ops" }),
    );
    expect(await screen.findByText(/Test message sent to #ops at/)).toBeInTheDocument();
    fail = true;
    await userEvent.click(screen.getByRole("button", { name: "Send a test message to #ops" }));
    expect(await screen.findByText("Test message to #ops failed.")).toBeInTheDocument();
  });

  it("deletes a channel after confirmation", async () => {
    let items = [channel()];
    const fetchMock = mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items }),
      "DELETE /api/v1/channels/c1": () => {
        items = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Delete #ops" }));
    const dialog = screen.getByRole("dialog", { name: "Delete #ops?" });
    expect(dialog).toHaveTextContent(/notify every active channel/);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "DELETE")).toBe(false);

    await userEvent.click(screen.getByRole("button", { name: "Delete #ops" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete channel" }),
    );
    expect(await screen.findByText(/No notification channels yet/)).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Channel #ops deleted");
  });

  it("switches the theme and remembers it in this browser", async () => {
    mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("radio", { name: "Dark" }));
    expect(document.documentElement).toHaveClass("theme-dark");
    expect(localStorage.getItem("maiak-theme")).toBe("dark");
    await userEvent.click(screen.getByRole("radio", { name: "System" }));
    expect(document.documentElement).not.toHaveClass("theme-dark");
    expect(localStorage.getItem("maiak-theme")).toBeNull();
  });

  it("switches the theme from the top bar, in step with Settings", async () => {
    mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/settings");
    const bar = within(await screen.findByRole("banner"));
    await userEvent.click(bar.getByRole("button", { name: "Light theme" }));
    expect(document.documentElement).toHaveClass("theme-light");
    expect(bar.getByRole("button", { name: "Light theme" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("radio", { name: "Light" })).toBeChecked();
    // Pressed again: back to the system setting.
    await userEvent.click(bar.getByRole("button", { name: "Light theme" }));
    expect(document.documentElement).not.toHaveClass("theme-light");
    expect(screen.getByRole("radio", { name: "System" })).toBeChecked();
  });
});

const heartbeat = (over: Partial<MonitorDetail> = {}): MonitorDetail =>
  detail({
    id: "hb1",
    type: "heartbeat",
    name: "Alertmanager",
    url: "",
    intervalSeconds: 300,
    expectedIntervalSeconds: 300,
    graceSeconds: 60,
    failureThreshold: 1,
    expectedStatus: "2xx",
    lastPingAt: new Date(Date.now() - 40_000).toISOString(),
    tlsExpiresAt: null,
    uptime: { h24: null, d7: null, d30: null },
    ...over,
  });

describe("watchdogs", () => {
  const url = "https://maiak.example.com/api/v1/heartbeat/s3cr3t-token";

  it("marks watchdogs in the list with their expectation and last ping", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors": () =>
        json({
          summary: { up: 1, down: 0, paused: 0, unknown: 0 },
          items: [
            listItem({
              id: "hb1",
              type: "heartbeat",
              name: "Alertmanager",
              url: "",
              expectedIntervalSeconds: 300,
              graceSeconds: 60,
              lastCheckedAt: null,
              lastPingAt: new Date(Date.now() - 125_000).toISOString(),
              uptime24h: null,
              lastResponseMs: null,
            }),
          ],
        }),
    });
    renderApp("/");
    const row = (await screen.findByRole("link", { name: "Alertmanager" })).closest("tr")!;
    expect(within(row).getByText("Watchdog")).toBeInTheDocument();
    expect(within(row).getByText("Ping every 5 min, grace 1 min")).toBeInTheDocument();
    // Twice in the DOM: the column on wide screens, the row's second line on phones.
    expect(within(row).getAllByText("2 min ago")).toHaveLength(2);
  });

  it("creates a watchdog and shows its URL once with the Alertmanager snippet", async () => {
    let body: unknown;
    mockApi({
      ...signedIn,
      "POST /api/v1/monitors": async (req) => {
        body = await req.json();
        return json(
          heartbeat({
            id: "hb1",
            heartbeatUrl: url,
            expectedIntervalSeconds: 900,
            intervalSeconds: 900,
          }),
          201,
        );
      },
      "GET /api/v1/monitors/hb1": () => json(heartbeat()),
      "GET /api/v1/monitors/hb1/incidents": () => json({ items: [] }),
    });
    renderApp("/monitors/new");
    await userEvent.click(await screen.findByLabelText("Watchdog (heartbeat)"));
    expect(screen.queryByLabelText("URL")).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Name"), "Alertmanager");
    await userEvent.click(screen.getByRole("button", { name: "15 min" }));
    expect(screen.getByLabelText("Expected interval (seconds)")).toHaveValue(900);
    await userEvent.clear(screen.getByLabelText("Grace (seconds)"));
    await userEvent.type(screen.getByLabelText("Grace (seconds)"), "120");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));

    expect(await screen.findByRole("heading", { name: "Your heartbeat URL" })).toBeInTheDocument();
    expect(body).toEqual({
      type: "heartbeat",
      name: "Alertmanager",
      expectedIntervalSeconds: 900,
      graceSeconds: 120,
      notifySlack: true,
      channelIds: [],
    });
    expect(screen.getByLabelText("Heartbeat URL")).toHaveValue(url);
    expect(screen.getByLabelText("Heartbeat URL")).toHaveAccessibleDescription(/shown only once/);
    const snippet = screen.getByLabelText("Alertmanager configuration");
    expect(snippet).toHaveTextContent('alertname = "Watchdog"');
    expect(snippet).toHaveTextContent("repeat_interval: 450s");
    expect(snippet).toHaveTextContent("url_file:");
    expect(snippet).not.toHaveTextContent(url);
    const secret = screen.getByLabelText("Secret with the URL");
    expect(secret).toHaveTextContent("--from-file=url=/dev/stdin");
    expect(secret).not.toHaveTextContent(url);

    await userEvent.click(screen.getByRole("link", { name: "I saved the URL, open the monitor" }));
    expect(
      await screen.findByRole("heading", { name: "Alertmanager", level: 1 }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Heartbeat URL")).not.toBeInTheDocument();
  });

  it("validates the interval and grace before calling the API", async () => {
    const fetchMock = mockApi({ ...signedIn });
    renderApp("/monitors/new");
    await userEvent.click(await screen.findByLabelText("Watchdog (heartbeat)"));
    await userEvent.type(screen.getByLabelText("Name"), "x");
    await userEvent.clear(screen.getByLabelText("Expected interval (seconds)"));
    await userEvent.type(screen.getByLabelText("Expected interval (seconds)"), "30");
    await userEvent.clear(screen.getByLabelText("Grace (seconds)"));
    await userEvent.type(screen.getByLabelText("Grace (seconds)"), "3601");
    await userEvent.click(screen.getByRole("button", { name: "Create monitor" }));
    expect(screen.getByLabelText("Expected interval (seconds)")).toHaveAccessibleDescription(
      expect.stringContaining("Enter whole seconds from 60 to 86400."),
    );
    expect(screen.getByLabelText("Grace (seconds)")).toHaveAccessibleDescription(
      expect.stringContaining("Enter whole seconds from 0 to 3600."),
    );
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(false);
  });

  it("shows the last ping and incidents, no response times, and regenerates after confirmation", async () => {
    const fetchMock = mockApi({
      ...signedIn,
      "GET /api/v1/monitors/hb1": () =>
        json(
          heartbeat({
            status: "down",
            statusSince: new Date(Date.now() - 10 * 60_000).toISOString(),
            lastErrorKind: "heartbeat_missed",
          }),
        ),
      "GET /api/v1/monitors/hb1/incidents": () =>
        json({
          items: [
            {
              id: "i1",
              startedAt: new Date(Date.now() - 10 * 60_000).toISOString(),
              resolvedAt: null,
              reason: "heartbeat_missed",
            },
          ],
        }),
      "POST /api/v1/monitors/hb1/heartbeat-token": () => json({ heartbeatUrl: url }),
    });
    renderApp("/monitors/hb1");
    expect(await screen.findByText(/Down for 10 min · no ping in time/)).toBeInTheDocument();
    expect(screen.getByText("Watchdog · Ping every 5 min, grace 1 min")).toBeInTheDocument();
    expect(await screen.findByText("Heartbeat missed")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Response time" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Recent checks" })).not.toBeInTheDocument();
    const posts = () =>
      fetchMock.mock.calls.filter(([r]) => (r as Request).url.endsWith("/heartbeat-token")).length;

    await userEvent.click(screen.getByRole("button", { name: "Regenerate URL" }));
    expect(
      screen.getByRole("dialog", { name: "Regenerate the URL of Alertmanager?" }),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(posts()).toBe(0);

    await userEvent.click(screen.getByRole("button", { name: "Regenerate URL" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Regenerate URL" }),
    );
    expect(await screen.findByLabelText("Heartbeat URL")).toHaveValue(url);
    expect(posts()).toBe(1);
    await userEvent.click(screen.getByRole("button", { name: "I saved the URL, hide it" }));
    expect(screen.queryByLabelText("Heartbeat URL")).not.toBeInTheDocument();
  });

  it("edits a watchdog without http fields", async () => {
    let body: unknown;
    mockApi({
      ...signedIn,
      "GET /api/v1/monitors/hb1": () => json(heartbeat()),
      "GET /api/v1/monitors/hb1/incidents": () => json({ items: [] }),
      "PATCH /api/v1/monitors/hb1": async (req) => {
        body = await req.json();
        return json(heartbeat({ expectedIntervalSeconds: 60 }));
      },
    });
    renderApp("/monitors/hb1/edit");
    await userEvent.click(await screen.findByRole("button", { name: "1 min" }));
    expect(screen.queryByLabelText("Watchdog (heartbeat)")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByRole("heading", { name: "Alertmanager", level: 1 });
    expect(body).toEqual({
      name: "Alertmanager",
      expectedIntervalSeconds: 60,
      graceSeconds: 60,
      notifySlack: true,
      channelIds: [],
    });
  });
});
