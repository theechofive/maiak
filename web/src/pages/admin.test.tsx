import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AdminTenant, AdminUser } from "../api/endpoints";
import { json, mockApi, problem, renderApp, session, signedIn } from "../test/utils";

const adminSession = { ...session, user: { ...session.user, isPlatformAdmin: true } };
const asAdmin = { "GET /api/v1/auth/me": () => json(adminSession) };

const tenant = (over: Partial<AdminTenant> = {}): AdminTenant => ({
  id: "t-1",
  slug: "t-abcdefghij",
  name: "alpha.example",
  planKey: "free",
  planName: "Free",
  status: "active",
  createdAt: "2026-10-01T00:00:00Z",
  deletedAt: null,
  users: 1,
  monitors: 2,
  heartbeats: 1,
  ...over,
});

const user = (over: Partial<AdminUser> = {}): AdminUser => ({
  id: "u-1",
  login: "alice@alpha.example",
  status: "active",
  role: "owner",
  createdAt: "2026-10-01T00:00:00Z",
  isPlatformAdmin: false,
  ...over,
});

const stats = {
  tenants: { total: 3, active: 2, suspended: 1, deleted: 1 },
  users: 4,
  monitors: 7,
  heartbeats: 2,
  plans: [
    { key: "free", name: "Free", tenants: 2 },
    { key: "internal", name: "Internal", tenants: 1 },
  ],
};

const requests = (mock: ReturnType<typeof mockApi>, method: string, path: string) =>
  mock.mock.calls
    .map(([r]) => r as Request)
    .filter((r) => r.method === method && new URL(r.url).pathname === path);

describe("admin area guard", () => {
  it("is not found for a user without the platform-admin flag and has no top-bar link", async () => {
    const fetchMock = mockApi({ ...signedIn });
    renderApp("/admin/tenants");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    expect(within(screen.getByRole("banner")).queryByRole("link", { name: "Admin" })).toBeNull();
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).url.includes("/api/v1/admin/"))).toBe(
      false,
    );
  });

  it("shows the top-bar link and the overview to the platform admin", async () => {
    mockApi({ ...asAdmin, "GET /api/v1/admin/stats": () => json(stats) });
    renderApp("/admin");
    expect(await screen.findByRole("heading", { name: "Admin" })).toBeInTheDocument();
    expect(within(screen.getByRole("banner")).getByRole("link", { name: "Admin" })).toHaveAttribute(
      "href",
      "/admin",
    );
    expect(await screen.findByText("3")).toBeInTheDocument();
    expect(screen.getByText("2 active · 1 suspended · 1 deleted")).toBeInTheDocument();
    const rows = within(screen.getByRole("table")).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(within(rows[2]!).getByText("Internal")).toBeInTheDocument();
  });

  it("explains a 403 from the API", async () => {
    mockApi({
      ...asAdmin,
      "GET /api/v1/admin/stats": () =>
        problem(403, "Not allowed", {
          type: "urn:maiak:problem:forbidden",
          detail: "Only the platform administrator may do this.",
        }),
    });
    renderApp("/admin");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Only the platform administrator may do this.",
    );
  });
});

describe("admin tenants list", () => {
  it("renders, searches and pages through tenants", async () => {
    const page1 = Array.from({ length: 25 }, (_, i) =>
      tenant({ id: `t-${i}`, name: `tenant-${i}.example`, slug: `t-${i}` }),
    );
    page1[1] = tenant({ id: "t-1", name: "suspended.example", status: "suspended" });
    const fetchMock = mockApi({
      ...asAdmin,
      "GET /api/v1/admin/tenants": (r) => {
        const q = new URL(r.url).searchParams;
        if (q.get("query") === "alpha")
          return json({ items: [tenant()], total: 1, limit: 25, offset: 0 });
        if (q.get("offset") === "25")
          return json({
            items: [tenant({ id: "t-last", name: "last.example" })],
            total: 26,
            limit: 25,
            offset: 25,
          });
        return json({ items: page1, total: 26, limit: 25, offset: 0 });
      },
    });
    renderApp("/admin/tenants");
    expect(await screen.findByText("tenant-0.example")).toBeInTheDocument();
    expect(screen.getByText("1–25 of 26")).toBeInTheDocument();
    expect(screen.getByText("Suspended")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByText("last.example")).toBeInTheDocument();
    expect(screen.getByText("26–26 of 26")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();

    await userEvent.type(screen.getByLabelText("Search tenants"), "alpha");
    await userEvent.click(screen.getByRole("button", { name: "Search" }));
    expect(await screen.findByText("alpha.example")).toBeInTheDocument();
    const last = requests(fetchMock, "GET", "/api/v1/admin/tenants").at(-1)!;
    const params = new URL(last.url).searchParams;
    expect(params.get("query")).toBe("alpha");
    expect(params.get("offset")).toBe("0");
    expect(params.get("limit")).toBe("25");
    expect(params.get("includeDeleted")).toBe("false");

    await userEvent.click(screen.getByLabelText("Show deleted tenants"));
    await waitFor(() =>
      expect(
        new URL(requests(fetchMock, "GET", "/api/v1/admin/tenants").at(-1)!.url).searchParams.get(
          "includeDeleted",
        ),
      ).toBe("true"),
    );
  });

  it("opens the details when a row is clicked", async () => {
    mockApi({
      ...asAdmin,
      "GET /api/v1/admin/tenants": () =>
        json({ items: [tenant()], total: 1, limit: 25, offset: 0 }),
      "GET /api/v1/admin/tenants/t-1": () => json(tenant()),
      "GET /api/v1/admin/tenants/t-1/users": () =>
        json({ items: [user()], total: 1, limit: 100, offset: 0 }),
      "GET /api/v1/admin/stats": () => json(stats),
    });
    renderApp("/admin/tenants");
    await userEvent.click(await screen.findByText("Free"));
    expect(
      await screen.findByRole("heading", { name: "alpha.example", level: 1 }),
    ).toBeInTheDocument();
  });

  it("says when nothing matches", async () => {
    mockApi({
      ...asAdmin,
      "GET /api/v1/admin/tenants": () => json({ items: [], total: 0, limit: 25, offset: 0 }),
    });
    renderApp("/admin/tenants?query=zzz");
    expect(await screen.findByText("No tenants match “zzz”.")).toBeInTheDocument();
  });
});

describe("admin tenant details", () => {
  let patchBodies: unknown[] = [];
  function details(extra: Record<string, (r: Request) => Response | Promise<Response>> = {}) {
    let current = tenant();
    patchBodies = [];
    return mockApi({
      ...asAdmin,
      "GET /api/v1/admin/stats": () => json(stats),
      "GET /api/v1/admin/tenants/t-1": () => json(current),
      "GET /api/v1/admin/tenants/t-1/users": () =>
        json({
          items: [user(), user({ id: "u-admin", login: "admin", isPlatformAdmin: true })],
          total: 2,
          limit: 100,
          offset: 0,
        }),
      "PATCH /api/v1/admin/tenants/t-1": async (r) => {
        const body = (await r.json()) as { planKey?: string; status?: "active" | "suspended" };
        patchBodies.push(body);
        current = { ...current, ...(body.status ? { status: body.status } : {}) };
        if (body.planKey) current = { ...current, planKey: body.planKey, planName: "Internal" };
        return json(current);
      },
      "DELETE /api/v1/admin/tenants/t-1": () => {
        current = { ...current, status: "suspended", deletedAt: "2026-10-21T10:00:00Z" };
        return new Response(null, { status: 204 });
      },
      ...extra,
    });
  }

  it("suspends and unsuspends only after confirming", async () => {
    const fetchMock = details();
    renderApp("/admin/tenants/t-1");
    await userEvent.click(await screen.findByRole("button", { name: "Suspend" }));
    const dialog = screen.getByRole("dialog", { name: "Suspend alpha.example?" });
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(requests(fetchMock, "PATCH", "/api/v1/admin/tenants/t-1")).toHaveLength(0);

    await userEvent.click(screen.getByRole("button", { name: "Suspend" }));
    await userEvent.click(screen.getByRole("button", { name: "Suspend tenant" }));
    expect(await screen.findByText("Tenant suspended")).toBeInTheDocument();
    const patch = requests(fetchMock, "PATCH", "/api/v1/admin/tenants/t-1")[0]!;
    expect(patchBodies).toEqual([{ status: "suspended" }]);
    expect(patch.headers.get("X-CSRF-Token")).toBe("csrf-1");

    await userEvent.click(await screen.findByRole("button", { name: "Unsuspend" }));
    await userEvent.click(screen.getByRole("button", { name: "Unsuspend tenant" }));
    expect(await screen.findByText("Tenant unsuspended")).toBeInTheDocument();
  });

  it("deletes after confirming and then offers no more changes", async () => {
    const fetchMock = details();
    renderApp("/admin/tenants/t-1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete tenant" }));
    expect(await screen.findByText("Tenant deleted")).toBeInTheDocument();
    expect(requests(fetchMock, "DELETE", "/api/v1/admin/tenants/t-1")).toHaveLength(1);
    expect(await screen.findByText(/A deleted tenant cannot be changed/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Suspend" })).toBeNull();
  });

  it("changes the plan and shows a 422 next to the selector", async () => {
    let fail = true;
    const fetchMock = details({
      "PATCH /api/v1/admin/tenants/t-1": async () =>
        fail
          ? problem(422, "Validation failed", {
              type: "urn:maiak:problem:validation",
              fieldErrors: [{ field: "planKey", message: "unknown plan" }],
            })
          : json(tenant({ planKey: "internal", planName: "Internal" })),
    });
    renderApp("/admin/tenants/t-1");
    const select = await screen.findByLabelText("Plan");
    await waitFor(() => expect(within(select).getAllByRole("option")).toHaveLength(2));
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    await userEvent.selectOptions(select, "internal");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Plan: unknown plan.")).toBeInTheDocument();
    expect(select).toHaveAttribute("aria-invalid", "true");

    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Plan changed to Internal")).toBeInTheDocument();
    const last = requests(fetchMock, "PATCH", "/api/v1/admin/tenants/t-1").at(-1)!;
    expect(await last.json()).toEqual({ planKey: "internal" });
  });

  it("shows a 409 guard rail as a toast", async () => {
    details({
      "PATCH /api/v1/admin/tenants/t-1": () =>
        problem(409, "This tenant cannot be suspended or deleted", {
          type: "urn:maiak:problem:protected-tenant",
          detail: "The default tenant and the tenant you belong to cannot be suspended or deleted.",
        }),
    });
    renderApp("/admin/tenants/t-1");
    await userEvent.click(await screen.findByRole("button", { name: "Suspend" }));
    await userEvent.click(screen.getByRole("button", { name: "Suspend tenant" }));
    expect(
      await screen.findByText(
        "The default tenant and the tenant you belong to cannot be suspended or deleted.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows not found for an unknown tenant", async () => {
    details({ "GET /api/v1/admin/tenants/t-1": () => problem(404, "Not found") });
    renderApp("/admin/tenants/t-1");
    expect(await screen.findByRole("heading", { name: "Tenant not found" })).toBeInTheDocument();
  });

  it("shows a temporary password once, only in memory, and never for the system admin", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const fetchMock = details({
      "POST /api/v1/admin/users/u-1/reset-password": () =>
        json({ temporaryPassword: "abcdefghijkmnpqrstuvwxyz" }),
      "GET /api/v1/admin/tenants": () =>
        json({ items: [tenant()], total: 1, limit: 25, offset: 0 }),
    });
    renderApp("/admin/tenants/t-1");
    const rows = within(await screen.findByRole("table")).getAllByRole("row");
    const adminRow = rows.find((r) => within(r).queryByText("admin"))!;
    const adminReset = within(adminRow).getByRole("button", { name: "Reset password" });
    expect(adminReset).toBeDisabled();
    expect(adminReset).toHaveAccessibleDescription("Managed by the maiak-admin Secret, not here.");

    const aliceRow = rows.find((r) => within(r).queryByText("alice@alpha.example"))!;
    await userEvent.click(within(aliceRow).getByRole("button", { name: "Reset password" }));
    const dialog = screen.getByRole("dialog", {
      name: "Reset the password of alice@alpha.example?",
    });
    await userEvent.click(within(dialog).getByRole("button", { name: "Reset password" }));

    const field = await screen.findByLabelText("Temporary password");
    expect(field).toHaveValue("abcdefghijkmnpqrstuvwxyz");
    expect(field).toHaveAccessibleDescription(/shown only once/);
    expect(requests(fetchMock, "POST", "/api/v1/admin/users/u-1/reset-password")).toHaveLength(1);

    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await userEvent.click(screen.getByRole("button", { name: "Copy password" }));
    expect(writeText).toHaveBeenCalledWith("abcdefghijkmnpqrstuvwxyz");
    expect(await screen.findByText("Password copied")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "I handed it over, hide it" }));
    expect(screen.queryByLabelText("Temporary password")).toBeNull();

    // Shown again after another reset, then gone after leaving the page.
    await userEvent.click(within(aliceRow).getByRole("button", { name: "Reset password" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Reset password" }),
    );
    expect(await screen.findByLabelText("Temporary password")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: "← Tenants" }));
    expect(await screen.findByRole("heading", { name: "Tenants" })).toBeInTheDocument();
    await userEvent.click(await screen.findByText("alpha.example"));
    expect(
      await screen.findByRole("heading", { name: "alpha.example", level: 1 }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Temporary password")).toBeNull();
    expect(document.body.textContent).not.toContain("abcdefghijkmnpqrstuvwxyz");
    expect(setItem.mock.calls.flat().join(" ")).not.toContain("abcdefghijkmnpqrstuvwxyz");
  });

  it("shows a 409 for the system admin and other reset errors as a toast", async () => {
    details({
      "POST /api/v1/admin/users/u-1/reset-password": () =>
        problem(409, "The system administrator's password cannot be reset here", {
          type: "urn:maiak:problem:system-admin",
          detail: "Its credentials come from the maiak-admin Secret.",
        }),
    });
    renderApp("/admin/tenants/t-1");
    const rows = within(await screen.findByRole("table")).getAllByRole("row");
    const aliceRow = rows.find((r) => within(r).queryByText("alice@alpha.example"))!;
    await userEvent.click(within(aliceRow).getByRole("button", { name: "Reset password" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Reset password" }),
    );
    expect(
      await screen.findByText("Its credentials come from the maiak-admin Secret."),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Temporary password")).toBeNull();
  });
});

describe("login of a suspended account", () => {
  it("explains the suspension after a correct password", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Unauthenticated"),
      "POST /api/v1/auth/login": () =>
        problem(403, "Account suspended", { type: "urn:maiak:problem:account-suspended" }),
    });
    renderApp("/login");
    await userEvent.type(await screen.findByLabelText("Email or login"), "alice@alpha.example");
    await userEvent.type(screen.getByLabelText("Password"), "right password");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This account is suspended. Contact the administrator of this Maiak installation.",
    );
  });
});
