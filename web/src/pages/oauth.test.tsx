import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { browser } from "../lib/oauth";
import { json, mockApi, problem, renderApp, session, signedIn } from "../test/utils";

const noSession = { "GET /api/v1/auth/me": () => problem(401, "Not authenticated") };
const providers = (passwordLoginEnabled = true) => ({
  "GET /api/v1/auth/providers": () =>
    json({
      providers: [
        { id: "google", name: "Google" },
        { id: "github", name: "GitHub" },
      ],
      passwordLoginEnabled,
      registrationEnabled: false,
    }),
});
const started = (provider: string) => ({
  [`POST /api/v1/auth/oauth/${provider}/start`]: () =>
    json({ authorizationUrl: `https://idp.example/${provider}?state=s` }),
});

const requests = (mock: ReturnType<typeof mockApi>, method: string, path: string) =>
  mock.mock.calls
    .map(([r]) => r as Request)
    .filter((r) => r.method === method && new URL(r.url).pathname === path);

let go: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  go = vi.spyOn(browser, "go").mockImplementation(() => {});
});
afterEach(() => go.mockRestore());

describe("login with a provider (O-02)", () => {
  it("offers the configured providers and leaves for the provider's URL", async () => {
    const fetchMock = mockApi({ ...noSession, ...providers(), ...started("github") });
    renderApp("/login");
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with GitHub" }));
    await waitFor(() => expect(go).toHaveBeenCalledWith("https://idp.example/github?state=s"));
    const [start] = requests(fetchMock, "POST", "/api/v1/auth/oauth/github/start");
    expect(await start!.clone().json()).toEqual({});
    // The password form is still there.
    expect(screen.getByLabelText("Email or login")).toBeInTheDocument();
  });

  it("explains the callback's error code in plain words, never the provider's text", async () => {
    mockApi({ ...noSession, ...providers() });
    const { unmount } = renderApp("/login?error=account_exists");
    expect(
      await screen.findByText(
        /An account with this email already exists\. Sign in with its password/,
      ),
    ).toBeInTheDocument();
    unmount();

    mockApi({ ...noSession, ...providers() });
    renderApp("/login?error=%3Cscript%3Ealert(1)%3C%2Fscript%3E");
    expect(
      await screen.findByText("Sign-in with the provider failed. Try again."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/script/)).not.toBeInTheDocument();
  });

  it("hides the password form when it is off, except on /login?admin", async () => {
    mockApi({ ...noSession, ...providers(false) });
    const { unmount } = renderApp("/login");
    expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Email or login")).not.toBeInTheDocument();
    unmount();

    mockApi({ ...noSession, ...providers(false) });
    renderApp("/login?admin");
    expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
    expect(screen.getByLabelText("Email or login")).toBeInTheDocument();
  });

  it("keeps the password form when the providers cannot be loaded", async () => {
    mockApi({ ...noSession });
    renderApp("/login");
    expect(await screen.findByLabelText("Email or login")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Sign in with/ })).not.toBeInTheDocument();
  });

  it("shows a start failure without leaving the page", async () => {
    mockApi({
      ...noSession,
      ...providers(),
      "POST /api/v1/auth/oauth/google/start": () => problem(503, "Unavailable"),
    });
    renderApp("/login");
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with Google" }));
    expect(
      await screen.findByText("The provider cannot be reached right now. Try again later."),
    ).toBeInTheDocument();
    expect(go).not.toHaveBeenCalled();
  });
});

describe("invitation with a provider (O-02)", () => {
  const preview = { organizationName: "Acme", role: "member", expiresAt: "2026-11-12T10:00:00Z" };

  it("sends the token only in the start body and removes it from the address bar", async () => {
    window.history.replaceState(null, "", "/invite#TOKEN-OAUTH");
    const fetchMock = mockApi({
      ...noSession,
      ...providers(),
      ...started("google"),
      "POST /api/v1/invitations/lookup": () => json(preview),
    });
    renderApp("/invite#TOKEN-OAUTH");
    expect(await screen.findByRole("heading", { name: "Join Acme" })).toBeInTheDocument();
    expect(window.location.hash).toBe("");
    await userEvent.click(await screen.findByRole("button", { name: "Join with Google" }));
    await waitFor(() => expect(go).toHaveBeenCalledWith("https://idp.example/google?state=s"));
    const [start] = requests(fetchMock, "POST", "/api/v1/auth/oauth/google/start");
    expect(await start!.clone().json()).toEqual({ inviteToken: "TOKEN-OAUTH" });
    expect(start!.url).not.toContain("TOKEN");
    expect(go.mock.calls[0]![0]).not.toContain("TOKEN");
  });

  it("offers only the providers when password accounts are off", async () => {
    mockApi({
      ...noSession,
      ...providers(false),
      "POST /api/v1/invitations/lookup": () => json(preview),
    });
    renderApp("/invite#TOKEN-2");
    expect(await screen.findByRole("button", { name: "Join with GitHub" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Email")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
  });
});

describe("profile sign-in methods (O-02)", () => {
  const identities = (items: object[], passwordSet = true, passwordLoginEnabled = true) => ({
    "GET /api/v1/auth/identities": () => json({ items, passwordSet, passwordLoginEnabled }),
  });
  const google = {
    provider: "google",
    email: "ada@example.com",
    createdAt: "2026-11-01T10:00:00Z",
    lastLoginAt: "2026-11-06T10:00:00Z",
  };

  it("lists identities, links another provider and unlinks one", async () => {
    let linked = [google];
    const fetchMock = mockApi({
      ...signedIn,
      ...providers(),
      ...started("github"),
      "GET /api/v1/auth/identities": () =>
        json({ items: linked, passwordSet: true, passwordLoginEnabled: true }),
      "DELETE /api/v1/auth/identities/google": () => {
        linked = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/profile?linked=github");
    expect(await screen.findByText("GitHub linked.")).toBeInTheDocument();
    const table = await screen.findByRole("table");
    expect(within(table).getByText("ada@example.com")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Link GitHub" }));
    await waitFor(() => expect(go).toHaveBeenCalledWith("https://idp.example/github?state=s"));
    const [start] = requests(fetchMock, "POST", "/api/v1/auth/oauth/github/start");
    expect(await start!.clone().json()).toEqual({ intent: "link" });
    expect(start!.headers.get("X-CSRF-Token")).toBe("csrf-1");

    await userEvent.click(screen.getByRole("button", { name: "Unlink Google" }));
    const dialog = await screen.findByRole("dialog", { name: "Unlink Google?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Unlink" }));
    expect(await screen.findByText("No provider is linked to your account.")).toBeInTheDocument();
  });

  it("guards the last way to sign in and shows the server's refusal too", async () => {
    mockApi({
      ...signedIn,
      ...providers(),
      ...identities([google], false),
    });
    const { unmount } = renderApp("/profile");
    const unlink = await screen.findByRole("button", { name: "Unlink Google" });
    expect(unlink).toBeDisabled();
    expect(unlink).toHaveAccessibleDescription(/This is your only way to sign in/);
    unmount();

    // A password that cannot be used does not count; the server answers 409 if the UI is stale.
    mockApi({
      ...signedIn,
      ...providers(),
      ...identities([google, { ...google, provider: "github" }], true, false),
      "DELETE /api/v1/auth/identities/github": () =>
        problem(409, "Last", { type: "urn:maiak:problem:last-login-method" }),
    });
    renderApp("/profile");
    await userEvent.click(await screen.findByRole("button", { name: "Unlink GitHub" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Unlink" }));
    expect(await screen.findByText(/This is your only way to sign in/)).toBeInTheDocument();
  });

  it("shows the callback's error code", async () => {
    mockApi({ ...signedIn, ...providers(), ...identities([google]) });
    renderApp("/profile?error=identity_in_use");
    expect(
      await screen.findByText("This provider account is already linked to another Maiak user."),
    ).toBeInTheDocument();
  });
});

describe("sign-in domains (O-02)", () => {
  const domain = {
    id: "d1",
    domain: "example.com",
    defaultRole: "viewer",
    enabled: true,
    createdAt: "2026-11-06T10:00:00Z",
    updatedAt: "2026-11-06T10:00:00Z",
  };
  const settingsBase = {
    "GET /api/v1/usage": () => problem(501, "Not implemented yet"),
    "GET /api/v1/channels": () => json({ items: [] }),
    "GET /api/v1/projects": () => json({ items: [] }),
  };

  it("is shown to owners only", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => json({ ...session, role: "admin" }),
      ...settingsBase,
      ...providers(),
    });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Appearance" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Sign-in domains" })).not.toBeInTheDocument();
  });

  it("claims, explains refusals, changes the role and auto-join, and releases", async () => {
    let items: (typeof domain)[] = [];
    let attempt = 0;
    const fetchMock = mockApi({
      ...signedIn,
      ...settingsBase,
      ...providers(),
      ...started("google"),
      "GET /api/v1/organization/auth-domains": () => json({ items }),
      "POST /api/v1/organization/auth-domains": () => {
        attempt++;
        if (attempt === 1)
          return problem(403, "No", { type: "urn:maiak:problem:domain-not-allowed" });
        if (attempt === 2) return problem(409, "Taken", { type: "urn:maiak:problem:domain-taken" });
        if (attempt === 3)
          return problem(403, "Proof", { type: "urn:maiak:problem:domain-not-verified" });
        items = [domain];
        return json(domain, 201);
      },
      "PATCH /api/v1/organization/auth-domains/d1": async (r) => {
        const body = (await r.json()) as Partial<typeof domain>;
        items = [{ ...domain, ...body }];
        return json(items[0]);
      },
      "DELETE /api/v1/organization/auth-domains/d1": () => {
        items = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Sign-in domains" })).toBeInTheDocument();
    expect(await screen.findByText(/No domain is claimed/)).toBeInTheDocument();

    // Step 1: the fresh Google proof goes through the link flow.
    await userEvent.click(screen.getByRole("button", { name: "Sign in with Google again" }));
    await waitFor(() => expect(go).toHaveBeenCalled());
    const [start] = requests(fetchMock, "POST", "/api/v1/auth/oauth/google/start");
    expect(await start!.clone().json()).toEqual({ intent: "link" });

    const input = screen.getByLabelText("Domain");
    await userEvent.type(input, "Example.com");
    const claim = screen.getByRole("button", { name: "Claim domain" });
    await userEvent.click(claim);
    expect(await screen.findByText(/does not allow claiming that domain/)).toBeInTheDocument();
    await userEvent.click(claim);
    expect(await screen.findByText(/already claimed this domain/)).toBeInTheDocument();
    await userEvent.click(claim);
    expect(
      await screen.findByText(/No recent Google sign-in with an account of this domain/),
    ).toBeInTheDocument();
    await userEvent.click(claim);
    const row = await screen.findByRole("row", { name: /example\.com/ });
    const [sent] = requests(fetchMock, "POST", "/api/v1/organization/auth-domains");
    expect(await sent!.clone().json()).toEqual({ domain: "example.com" });

    await userEvent.selectOptions(within(row).getByLabelText("Role for example.com"), "member");
    await waitFor(() =>
      expect(screen.getByLabelText("Role for example.com")).toHaveValue("member"),
    );
    await userEvent.click(screen.getByLabelText("Auto-join for example.com"));
    await waitFor(() =>
      expect(screen.getByLabelText("Auto-join for example.com")).not.toBeChecked(),
    );

    await userEvent.click(screen.getByRole("button", { name: "Release example.com" }));
    const dialog = await screen.findByRole("dialog", { name: "Release example.com?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Release" }));
    expect(await screen.findByText(/No domain is claimed/)).toBeInTheDocument();
  });
});
