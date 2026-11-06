import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type {
  MonitorListItem,
  StatusPage,
  StatusPageComponent,
  StatusPageSnapshot,
} from "../api/endpoints";
import { json, mockApi, problem, renderApp, session } from "../test/utils";

const as = (role: string) => ({
  "GET /api/v1/auth/me": () => json({ ...session, role }),
});

const page = (over: Partial<StatusPage> = {}): StatusPage => ({
  id: "sp",
  slug: "acme",
  title: "Acme status",
  description: "",
  logoUrl: null,
  accentColor: null,
  published: false,
  passwordProtected: false,
  publicUrl: "https://maiak.example.com/status/acme",
  componentCount: 0,
  createdAt: "2026-11-01T00:00:00Z",
  updatedAt: "2026-11-01T00:00:00Z",
  ...over,
});

const days = Array.from({ length: 90 }, (_, i) => ({
  date: new Date(Date.UTC(2026, 7, 9 + i)).toISOString().slice(0, 10),
  status: "operational" as const,
  uptime: 100,
}));

const snapshot = (over: Partial<StatusPageSnapshot> = {}): StatusPageSnapshot => ({
  version: 1,
  page: { slug: "acme", title: "Acme status", description: "", logoUrl: null, accentColor: null },
  status: "operational",
  generatedAt: "2026-11-06T12:00:00Z",
  components: [{ name: "API", group: "Core", status: "operational", uptime: 99.98, days }],
  activeIncidents: [],
  pastIncidents: [],
  ...over,
});

const monitor = (id: string, name: string, type: "http" | "heartbeat" = "http") =>
  ({ id, name, type, projectId: "p" }) as MonitorListItem;

const component = (over: Partial<StatusPageComponent>): StatusPageComponent => ({
  monitorId: "m1",
  monitorName: "api-internal",
  monitorType: "http",
  projectId: "p",
  publicName: "API",
  groupName: "Core",
  position: 0,
  ...over,
});

describe("status page settings", () => {
  it("lets an admin create the page with a slug from the title", async () => {
    let items: StatusPage[] = [];
    let body: unknown;
    mockApi({
      ...as("admin"),
      "GET /api/v1/status-pages": () => json({ items }),
      "POST /api/v1/status-pages": async (r) => {
        body = await r.json();
        items = [page()];
        return json(items[0], 201);
      },
      "GET /api/v1/status-pages/sp/preview": () => json(snapshot()),
      "GET /api/v1/status-pages/sp/components": () => json({ items: [] }),
      "GET /api/v1/monitors": () => json({ items: [] }),
    });
    renderApp("/status-page");
    await userEvent.type(await screen.findByLabelText("Title"), "Acme Status");
    expect(screen.getByLabelText("Address")).toHaveValue("acme-status");
    await userEvent.clear(screen.getByLabelText("Address"));
    await userEvent.type(screen.getByLabelText("Address"), "status");
    await userEvent.click(screen.getByRole("button", { name: "Create status page" }));
    expect(await screen.findByText(/This address is reserved/)).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Address"));
    await userEvent.type(screen.getByLabelText("Address"), "acme");
    await userEvent.click(screen.getByRole("button", { name: "Create status page" }));
    expect(await screen.findByRole("button", { name: "Publish" })).toBeInTheDocument();
    expect(body).toEqual({ title: "Acme Status", slug: "acme" });
    expect(screen.getByText("Not published")).toBeInTheDocument();
  });

  it("shows viewers the link only, without settings", async () => {
    mockApi({
      ...as("viewer"),
      "GET /api/v1/status-pages": () => json({ items: [page({ published: true })] }),
    });
    renderApp("/status-page");
    expect(await screen.findByLabelText("Public link")).toHaveValue(
      "https://maiak.example.com/status/acme",
    );
    expect(screen.getByText(/Only admins and owners can change or publish/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Publish/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("complementary", { name: "Live preview" })).not.toBeInTheDocument();
  });

  it(
    "edits components from any project, saves them in order and publishes",
    { timeout: 20_000 },
    async () => {
      let saved: unknown;
      let current = page();
      mockApi({
        ...as("owner"),
        "GET /api/v1/status-pages": () => json({ items: [current] }),
        "GET /api/v1/status-pages/sp/preview": () => json(snapshot()),
        "GET /api/v1/status-pages/sp/components": () => json({ items: [component({})] }),
        "GET /api/v1/monitors": () =>
          json({
            items: [monitor("m1", "api-internal"), monitor("m2", "nightly-cron", "heartbeat")],
          }),
        "PUT /api/v1/status-pages/sp/components": async (r) => {
          saved = await r.json();
          return json({
            items: [
              component({
                monitorId: "m2",
                monitorName: "nightly-cron",
                monitorType: "heartbeat",
                publicName: "Nightly job",
                groupName: null,
                position: 0,
              }),
              component({ position: 1 }),
            ],
          });
        },
        "PATCH /api/v1/status-pages/sp": async (r) => {
          const b = (await r.json()) as { published?: boolean };
          current = page({ published: b.published ?? current.published });
          return json(current);
        },
      });
      renderApp("/status-page");
      const preview = await screen.findByRole("complementary", { name: "Live preview" });
      expect(await within(preview).findByText("All systems operational")).toBeInTheDocument();
      expect(within(preview).queryByText("api-internal")).not.toBeInTheDocument();

      await userEvent.selectOptions(
        await screen.findByLabelText("Add a monitor or heartbeat"),
        "m2",
      );
      await userEvent.click(screen.getByRole("button", { name: "Add component" }));
      await userEvent.click(screen.getByRole("button", { name: "Save components" }));
      expect(await screen.findByText("Enter a public name.")).toBeInTheDocument();
      const names = screen.getAllByLabelText("Public name");
      await userEvent.type(names[1]!, "Nightly job");
      await userEvent.click(screen.getByRole("button", { name: "Move Nightly job up" }));
      await userEvent.click(screen.getByRole("button", { name: "Save components" }));
      await waitFor(() =>
        expect(saved).toEqual({
          items: [
            { monitorId: "m2", publicName: "Nightly job" },
            { monitorId: "m1", publicName: "API", groupName: "Core" },
          ],
        }),
      );

      await userEvent.click(screen.getByRole("button", { name: "Publish" }));
      expect(await screen.findByRole("button", { name: "Unpublish" })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: "Open public page" })).toHaveAttribute(
        "href",
        "https://maiak.example.com/status/acme",
      );
      expect(screen.getByLabelText("Badge script")).toHaveValue(
        '<script src="https://maiak.example.com/status/acme/embed.js" async></script>',
      );
    },
  );

  it(
    "applies unsaved branding to the preview and shows server field errors",
    { timeout: 20_000 },
    async () => {
      mockApi({
        ...as("admin"),
        "GET /api/v1/status-pages": () => json({ items: [page()] }),
        "GET /api/v1/status-pages/sp/preview": () => json(snapshot({ status: "partial_outage" })),
        "GET /api/v1/status-pages/sp/components": () => json({ items: [] }),
        "GET /api/v1/monitors": () => json({ items: [] }),
        "PATCH /api/v1/status-pages/sp": () =>
          problem(409, "This address is already used by another status page", {
            type: "urn:maiak:problem:slug-taken",
          }),
      });
      renderApp("/status-page");
      const preview = await screen.findByRole("complementary", { name: "Live preview" });
      expect(await within(preview).findByText("Partial outage")).toBeInTheDocument();
      const title = screen.getByLabelText("Title");
      await userEvent.clear(title);
      await userEvent.type(title, "Shiny <b>status</b>");
      expect(within(preview).getByText("Shiny <b>status</b>")).toBeInTheDocument();
      await userEvent.type(screen.getByLabelText("Logo URL"), "http://cdn.example.com/x.png");
      await userEvent.type(screen.getByLabelText("Accent colour"), "red");
      await userEvent.click(screen.getByRole("button", { name: "Save page" }));
      expect(await screen.findByText(/Use an https:\/\/ address/)).toBeInTheDocument();
      expect(screen.getByText("Use a colour like #1f6feb.")).toBeInTheDocument();
      expect(within(preview).queryByText(/logo ·/)).not.toBeInTheDocument();

      await userEvent.clear(screen.getByLabelText("Logo URL"));
      await userEvent.type(screen.getByLabelText("Logo URL"), "https://cdn.example.com/x.png");
      await userEvent.clear(screen.getByLabelText("Accent colour"));
      await userEvent.type(screen.getByLabelText("Accent colour"), "#1f6feb");
      expect(within(preview).getByText("logo · cdn.example.com")).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Save page" }));
      expect(await screen.findByText(/Save failed/)).toBeInTheDocument();
    },
  );

  it(
    "sets a write-only password and removes it after confirming",
    { timeout: 20_000 },
    async () => {
      let current = page({ published: true });
      let sent: unknown;
      mockApi({
        ...as("admin"),
        "GET /api/v1/status-pages": () => json({ items: [current] }),
        "GET /api/v1/status-pages/sp/preview": () => json(snapshot()),
        "GET /api/v1/status-pages/sp/components": () => json({ items: [] }),
        "GET /api/v1/monitors": () => json({ items: [] }),
        "PUT /api/v1/status-pages/sp/password": async (r) => {
          sent = await r.json();
          current = page({ published: true, passwordProtected: true });
          return new Response(null, { status: 204 });
        },
        "DELETE /api/v1/status-pages/sp/password": () => {
          current = page({ published: true });
          return new Response(null, { status: 204 });
        },
      });
      renderApp("/status-page");
      await userEvent.type(await screen.findByLabelText("New password"), "short");
      await userEvent.click(screen.getByRole("button", { name: "Set password" }));
      expect(await screen.findByText("Use at least 12 characters.")).toBeInTheDocument();
      await userEvent.type(screen.getByLabelText("New password"), " but now long enough");
      await userEvent.click(screen.getByRole("button", { name: "Set password" }));
      expect(await screen.findByText("Configured, hidden")).toBeInTheDocument();
      expect(sent).toEqual({ password: "short but now long enough" });
      expect(await screen.findByText(/A protected page has no badge/)).toBeInTheDocument();
      expect(screen.queryByLabelText("Badge script")).not.toBeInTheDocument();

      await userEvent.click(screen.getByRole("button", { name: "Remove password" }));
      const dialog = await screen.findByRole("dialog");
      await userEvent.click(within(dialog).getByRole("button", { name: "Remove password" }));
      expect(await screen.findByLabelText("New password")).toBeInTheDocument();
    },
  );
});
