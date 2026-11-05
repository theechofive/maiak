import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Invitation, Member, MonitorDetail } from "../api/endpoints";
import { json, mockApi, problem, renderApp, session } from "../test/utils";

const as = (role: string, over: object = {}) => ({
  "GET /api/v1/auth/me": () => json({ ...session, role, ...over }),
});

const member = (over: Partial<Member> = {}): Member => ({
  userId: "u2",
  login: "bob@example.com",
  displayName: "",
  role: "member",
  joinedAt: "2026-10-02T00:00:00Z",
  isPlatformAdmin: false,
  ...over,
});

const me = member({ userId: "u1", login: "admin", role: "owner" });

const invitation = (over: Partial<Invitation> = {}): Invitation => ({
  id: "i1",
  role: "viewer",
  label: "Carol",
  status: "open",
  createdAt: "2026-11-01T00:00:00Z",
  expiresAt: "2026-11-08T00:00:00Z",
  createdBy: "admin",
  acceptedBy: null,
  acceptedAt: null,
  revokedAt: null,
  ...over,
});

const requests = (mock: ReturnType<typeof mockApi>, method: string, path: string) =>
  mock.mock.calls
    .map(([r]) => r as Request)
    .filter((r) => r.method === method && new URL(r.url).pathname === path);

describe("members page", () => {
  it("lets an owner change a role, remove a member and invite with a link shown once", async () => {
    let members = [me, member()];
    const fetchMock = mockApi({
      ...as("owner"),
      "GET /api/v1/members": () => json({ items: members }),
      "GET /api/v1/invitations": () => json({ items: [invitation()] }),
      "PATCH /api/v1/members/u2": async (r) => {
        const body = (await r.json()) as { role: Member["role"] };
        members = [me, member({ role: body.role })];
        return json(members[1]);
      },
      "DELETE /api/v1/members/u2": () => {
        members = [me];
        return new Response(null, { status: 204 });
      },
      "POST /api/v1/invitations": () =>
        json(
          {
            invitation: invitation({ id: "i2", role: "admin", label: "" }),
            inviteUrl: "https://maiak.example.com/invite#SECRET-TOKEN",
          },
          201,
        ),
      "POST /api/v1/invitations/i1/revoke": () => new Response(null, { status: 204 }),
    });
    renderApp("/members");
    expect(await screen.findByRole("heading", { name: "Members", level: 1 })).toBeInTheDocument();
    const role = await screen.findByLabelText("Role of bob@example.com");
    expect(
      within(role)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Owner", "Admin", "Member", "Viewer"]);
    // The owner's own row: a select (owners manage owners) but no Remove for oneself.
    expect(screen.getByText("You")).toBeInTheDocument();

    await userEvent.selectOptions(role, "viewer");
    await waitFor(() => expect(requests(fetchMock, "PATCH", "/api/v1/members/u2")).toHaveLength(1));
    expect(await screen.findByText("bob@example.com is now viewer")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove bob@example.com?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove member" }));
    await waitFor(() => expect(screen.queryByText("bob@example.com")).toBeNull());

    // Invite: the dialog asks for the role and a note; the link appears once.
    await userEvent.click(screen.getByRole("button", { name: "Invite someone" }));
    const invite = await screen.findByRole("dialog", { name: "Invite someone" });
    await userEvent.selectOptions(within(invite).getByLabelText("Role"), "admin");
    await userEvent.click(within(invite).getByRole("button", { name: "Create link" }));
    const link = await screen.findByLabelText("Invitation link");
    expect(link).toHaveValue("https://maiak.example.com/invite#SECRET-TOKEN");
    expect(screen.getByText("Copy it now: it is shown only once.")).toBeInTheDocument();
    const sent = requests(fetchMock, "POST", "/api/v1/invitations")[0]!;
    expect(await sent.clone().json()).toEqual({ role: "admin" });
    await userEvent.click(screen.getByRole("button", { name: "I sent it, hide the link" }));
    expect(screen.queryByDisplayValue(/SECRET-TOKEN/)).toBeNull();

    // Revoke an open invitation.
    const row = screen.getByText("Carol").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    const revoke = await screen.findByRole("dialog", { name: "Revoke this invitation?" });
    await userEvent.click(within(revoke).getByRole("button", { name: "Revoke invitation" }));
    await waitFor(() =>
      expect(requests(fetchMock, "POST", "/api/v1/invitations/i1/revoke")).toHaveLength(1),
    );
  });

  it("explains a full plan when inviting", async () => {
    mockApi({
      ...as("admin"),
      "GET /api/v1/members": () => json({ items: [me, member({ userId: "u1", role: "admin" })] }),
      "GET /api/v1/invitations": () => json({ items: [] }),
      "POST /api/v1/invitations": () =>
        problem(409, "Plan limit reached", {
          type: "urn:maiak:problem:plan-limit-reached",
          limit: "members",
          max: 3,
        }),
    });
    renderApp("/members");
    await userEvent.click(await screen.findByRole("button", { name: "Invite someone" }));
    const dialog = await screen.findByRole("dialog", { name: "Invite someone" });
    // An admin cannot invite an owner, and the select never offers it.
    expect(
      within(within(dialog).getByLabelText("Role"))
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Admin", "Member", "Viewer"]);
    await userEvent.click(within(dialog).getByRole("button", { name: "Create link" }));
    expect(await within(dialog).findByText(/at most 3 members/)).toBeInTheDocument();
  });

  it("keeps admins away from owners", async () => {
    mockApi({
      ...as("admin", { user: { ...session.user, id: "u3" } }),
      "GET /api/v1/members": () =>
        json({
          items: [me, member(), member({ userId: "u3", login: "me@example.com", role: "admin" })],
        }),
      "GET /api/v1/invitations": () => json({ items: [] }),
    });
    renderApp("/members");
    expect(await screen.findByLabelText("Role of bob@example.com")).toBeInTheDocument();
    expect(screen.queryByLabelText("Role of admin")).toBeNull();
    expect(screen.getByText("Only an owner can change an owner.")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Delete organization" })).toBeNull();
  });

  it("is read-only for a viewer", async () => {
    const fetchMock = mockApi({
      ...as("viewer"),
      "GET /api/v1/members": () => json({ items: [me, member()] }),
    });
    renderApp("/members");
    expect(await screen.findByText("bob@example.com")).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Invite someone" })).toBeNull();
    expect(
      screen.getAllByText("Only admins and owners can invite people or change roles.").length,
    ).toBeGreaterThan(0);
    // Invitations are never even requested.
    expect(requests(fetchMock, "GET", "/api/v1/invitations")).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Leave organization" })).toBeInTheDocument();
  });

  it("shows why the last owner cannot leave", async () => {
    mockApi({
      ...as("owner"),
      "GET /api/v1/members": () => json({ items: [me] }),
      "GET /api/v1/invitations": () => json({ items: [] }),
      "POST /api/v1/organization/leave": () =>
        problem(409, "The organization needs an owner", {
          type: "urn:maiak:problem:last-owner",
          detail: "Make another member an owner first.",
        }),
    });
    renderApp("/members");
    await userEvent.click(await screen.findByRole("button", { name: "Leave organization" }));
    const dialog = await screen.findByRole("dialog", { name: "Leave Acme?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Leave" }));
    expect(await screen.findByText("Make another member an owner first.")).toBeInTheDocument();
  });

  it("lets an owner delete the organization and signs out", async () => {
    const fetchMock = mockApi({
      ...as("owner"),
      "GET /api/v1/members": () => json({ items: [me] }),
      "GET /api/v1/invitations": () => json({ items: [] }),
      "DELETE /api/v1/organization": () => new Response(null, { status: 204 }),
    });
    renderApp("/members");
    await userEvent.click(await screen.findByRole("button", { name: "Delete organization" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete Acme?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete organization" }));
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(requests(fetchMock, "DELETE", "/api/v1/organization")).toHaveLength(1);
  });
});

describe("role-aware pages", () => {
  const monitor: MonitorDetail = {
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
    statusSince: "2026-11-01T00:00:00Z",
    lastErrorKind: null,
    lastCheckedAt: null,
    tlsExpiresAt: null,
    ongoingIncident: null,
    lastPingAt: null,
    expectedIntervalSeconds: null,
    graceSeconds: null,
    uptime: { h24: null, d7: null, d30: null },
    channelIds: [],
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-01T00:00:00Z",
  };

  it("hides monitor changes from a viewer and says why", async () => {
    mockApi({
      ...as("viewer"),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
      "GET /api/v1/monitors/m1": () => json(monitor),
    });
    const view = renderApp("/");
    expect(
      (await screen.findAllByText(/Viewers can look but not change monitors/)).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: "Add monitor" })).toBeNull();
    view.unmount();

    renderApp("/monitors/m1");
    expect(await screen.findByRole("heading", { name: "Website", level: 1 })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("shows the monitor form as read only to a viewer", async () => {
    mockApi({ ...as("viewer") });
    renderApp("/monitors/new");
    expect(await screen.findByText("Read only.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Create/ })).toBeNull();
  });

  it("lets a member edit monitors but not channels", async () => {
    mockApi({
      ...as("member"),
      "GET /api/v1/monitors/m1": () => json(monitor),
      "GET /api/v1/channels": () =>
        json({
          items: [
            {
              id: "c1",
              name: "#ops",
              type: "slack_webhook",
              events: ["down"],
              mention: [],
              reminderMinutes: 0,
              status: "active",
              createdAt: "2026-10-01T00:00:00Z",
              updatedAt: "2026-10-01T00:00:00Z",
            },
          ],
        }),
    });
    const view = renderApp("/monitors/m1");
    expect(await screen.findByRole("link", { name: "Edit" })).toBeInTheDocument();
    view.unmount();

    renderApp("/settings");
    expect(await screen.findByText("#ops")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add channel" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit #ops" })).toBeNull();
    expect(
      screen.getByText("Only admins and owners can change notification channels."),
    ).toBeInTheDocument();
  });
});

describe("organization switcher", () => {
  it("switches the session to another organization", async () => {
    const orgs = {
      items: [
        { id: "t1", slug: "acme", name: "Acme", role: "owner", status: "active", current: true },
        { id: "t2", slug: "beta", name: "Beta", role: "viewer", status: "active", current: false },
        {
          id: "t3",
          slug: "gone",
          name: "Gone",
          role: "admin",
          status: "suspended",
          current: false,
        },
      ],
    };
    const fetchMock = mockApi({
      ...as("owner"),
      "GET /api/v1/auth/organizations": () => json(orgs),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
      "POST /api/v1/auth/switch-organization": () =>
        json({
          ...session,
          tenant: { id: "t2", slug: "beta", name: "Beta" },
          role: "viewer",
          csrf_token: "csrf-2",
        }),
    });
    renderApp("/");
    await waitFor(() => expect(requests(fetchMock, "GET", "/api/v1/monitors")).toHaveLength(1));
    const banner = await screen.findByRole("banner");
    const select = await within(banner).findByLabelText("Organization");
    expect(within(select).getByRole("option", { name: "Gone · Admin (suspended)" })).toBeDisabled();
    await userEvent.selectOptions(select, "t2");
    expect(await screen.findByText("Switched to Beta")).toBeInTheDocument();
    const sent = requests(fetchMock, "POST", "/api/v1/auth/switch-organization")[0]!;
    expect(await sent.clone().json()).toEqual({ tenantId: "t2" });
    expect(sent.headers.get("X-CSRF-Token")).toBe("csrf-1");
    // The page that stayed mounted reloads its data with the new session.
    await waitFor(() => expect(requests(fetchMock, "GET", "/api/v1/monitors")).toHaveLength(2));
    // Now a viewer in Beta: the monitors page has no add link.
    expect(
      (await screen.findAllByText(/Viewers can look but not change monitors/)).length,
    ).toBeGreaterThan(0);
  });

  it("shows only the name with one organization", async () => {
    mockApi({
      ...as("owner"),
      "GET /api/v1/auth/organizations": () =>
        json({
          items: [
            {
              id: "t1",
              slug: "acme",
              name: "Acme",
              role: "owner",
              status: "active",
              current: true,
            },
          ],
        }),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/");
    expect(await within(await screen.findByRole("banner")).findByText("Acme")).toBeInTheDocument();
    expect(screen.queryByLabelText("Organization")).toBeNull();
  });
});

describe("invitation page", () => {
  const preview = { organizationName: "Acme", role: "member", expiresAt: "2026-11-12T10:00:00Z" };

  it("registers a new person through the link without a workspace of their own", async () => {
    const fetchMock = mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Not authenticated"),
      "POST /api/v1/invitations/lookup": () => json(preview),
      "POST /api/v1/auth/register": () => json({ ...session, role: "member" }, 201),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/invite#TOKEN-123");
    expect(await screen.findByRole("heading", { name: "Join Acme" })).toBeInTheDocument();
    const lookup = requests(fetchMock, "POST", "/api/v1/invitations/lookup")[0]!;
    expect(await lookup.clone().json()).toEqual({ token: "TOKEN-123" });
    expect(new URL(lookup.url).search).toBe("");

    await userEvent.type(screen.getByLabelText("Email"), "new@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "a long passphrase");
    await userEvent.type(screen.getByLabelText("Confirm password"), "a long passphrase");
    await userEvent.click(screen.getByRole("button", { name: "Create account and join" }));
    await waitFor(() =>
      expect(requests(fetchMock, "POST", "/api/v1/auth/register")).toHaveLength(1),
    );
    const sent = requests(fetchMock, "POST", "/api/v1/auth/register")[0]!;
    expect(await sent.clone().json()).toEqual({
      email: "new@example.com",
      password: "a long passphrase",
      inviteToken: "TOKEN-123",
    });
    expect(await screen.findByRole("heading", { name: "Monitors" })).toBeInTheDocument();
  });

  it("joins as the signed-in user", async () => {
    const fetchMock = mockApi({
      ...as("owner"),
      "POST /api/v1/invitations/lookup": () => json(preview),
      "POST /api/v1/invitations/accept": () =>
        json({
          ...session,
          tenant: { id: "t9", slug: "acme2", name: "Acme" },
          role: "member",
          csrf_token: "csrf-9",
        }),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/invite#TOKEN-123");
    await userEvent.click(await screen.findByRole("button", { name: "Join Acme" }));
    expect(await screen.findByText("You joined Acme")).toBeInTheDocument();
    const sent = requests(fetchMock, "POST", "/api/v1/invitations/accept")[0]!;
    expect(await sent.clone().json()).toEqual({ token: "TOKEN-123" });
    // The used link is not looked up again.
    expect(requests(fetchMock, "POST", "/api/v1/invitations/lookup")).toHaveLength(1);
  });

  it("says the same thing for every invalid link", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Not authenticated"),
      "POST /api/v1/invitations/lookup": () =>
        problem(404, "This invitation link is not valid", {
          type: "urn:maiak:problem:invitation-invalid",
        }),
    });
    renderApp("/invite#USED");
    expect(await screen.findByText("This invitation link is not valid.")).toBeInTheDocument();
    expect(screen.getByText(/expired, been used or been revoked/)).toBeInTheDocument();
  });

  it("explains a link without its token", async () => {
    mockApi({ "GET /api/v1/auth/me": () => problem(401, "Not authenticated") });
    renderApp("/invite");
    expect(await screen.findByText(/This link is incomplete/)).toBeInTheDocument();
  });
});

describe("invitation page (T-12)", () => {
  const preview = { organizationName: "Acme", role: "member", expiresAt: "2026-11-12T10:00:00Z" };

  it("removes the token from the address bar and joins with an existing account", async () => {
    window.history.replaceState(null, "", "/invite#TOKEN-123");
    let attempts = 0;
    const fetchMock = mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Not authenticated"),
      "POST /api/v1/invitations/lookup": () => json(preview),
      "POST /api/v1/auth/register": () =>
        ++attempts === 1
          ? problem(401, "Wrong login or password", {
              type: "urn:maiak:problem:invalid-credentials",
            })
          : json({ ...session, role: "member" }, 201),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/invite#TOKEN-123");
    expect(await screen.findByRole("heading", { name: "Join Acme" })).toBeInTheDocument();
    expect(window.location.hash).toBe("");
    expect(window.location.pathname).toBe("/invite");

    await userEvent.click(screen.getByRole("button", { name: "Join with it" }));
    await userEvent.type(screen.getByLabelText("Email"), "old@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "wrong one");
    await userEvent.click(screen.getByRole("button", { name: "Sign in and join Acme" }));
    expect(await screen.findByText("Wrong email or password. Try again.")).toBeInTheDocument();
    // Still on the page, not thrown out to the sign-in screen.
    expect(screen.getByRole("heading", { name: "Join Acme" })).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("Password"), "the right passphrase");
    await userEvent.click(screen.getByRole("button", { name: "Sign in and join Acme" }));
    expect(await screen.findByRole("heading", { name: "Monitors" })).toBeInTheDocument();
    const sent = requests(fetchMock, "POST", "/api/v1/auth/register")[1]!;
    expect(await sent.clone().json()).toEqual({
      email: "old@example.com",
      password: "the right passphrase",
      inviteToken: "TOKEN-123",
    });
  });

  it("explains a taken email with the wrong password on the new-account form", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Not authenticated"),
      "POST /api/v1/invitations/lookup": () => json(preview),
      "POST /api/v1/auth/register": () =>
        problem(401, "Wrong login or password", { type: "urn:maiak:problem:invalid-credentials" }),
    });
    renderApp("/invite#TOKEN-123");
    await screen.findByRole("heading", { name: "Join Acme" });
    await userEvent.type(screen.getByLabelText("Email"), "old@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "a long passphrase");
    await userEvent.type(screen.getByLabelText("Confirm password"), "a long passphrase");
    await userEvent.click(screen.getByRole("button", { name: "Create account and join" }));
    expect(
      await screen.findByText(/already has an account and the password does not match/),
    ).toBeInTheDocument();
  });
});

describe("session changes in other tabs (T-12)", () => {
  it("reloads the session when another tab announces a change", async () => {
    const fetchMock = mockApi({
      ...as("owner"),
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
    });
    renderApp("/");
    await waitFor(() => expect(requests(fetchMock, "GET", "/api/v1/monitors")).toHaveLength(1));
    const meCalls = requests(fetchMock, "GET", "/api/v1/auth/me").length;
    const other = new BroadcastChannel("maiak-session");
    other.postMessage({ type: "session-changed" });
    other.close();
    await waitFor(() =>
      expect(requests(fetchMock, "GET", "/api/v1/auth/me").length).toBeGreaterThan(meCalls),
    );
    await waitFor(() => expect(requests(fetchMock, "GET", "/api/v1/monitors")).toHaveLength(2));
  });
});
