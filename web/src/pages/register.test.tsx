import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { getCsrfToken } from "../api/client";
import { json, mockApi, problem, renderApp } from "../test/utils";

const noSession = { "GET /api/v1/auth/me": () => problem(401, "Unauthenticated") };
const config = (registrationEnabled: boolean) => ({
  "GET /api/v1/public/config": () => json({ registrationEnabled }),
});

const registered = {
  user: { id: "u2", login: "owner@example.com", display_name: "" },
  tenant: { id: "t2", slug: "owner-k3x9qa", name: "example.com" },
  role: "owner",
  csrf_token: "csrf-reg",
  expires_at: "2030-01-01T00:00:00Z",
};

const PASSWORD = "correct horse battery";

async function fill(email: string, password: string, confirm = password) {
  if (email) await userEvent.type(await screen.findByLabelText("Email"), email);
  if (password) await userEvent.type(screen.getByLabelText("Password"), password);
  if (confirm) await userEvent.type(screen.getByLabelText("Confirm password"), confirm);
  await userEvent.click(screen.getByRole("button", { name: "Create account" }));
}

const registerCalls = (fetchMock: ReturnType<typeof mockApi>) =>
  fetchMock.mock.calls.map(([r]) => r as Request).filter((r) => r.url.endsWith("/auth/register"));

describe("registration links", () => {
  it("shows Create account on the login page only when registration is enabled", async () => {
    mockApi({ ...noSession, ...config(true) });
    const { unmount } = renderApp("/login");
    expect(await screen.findByRole("link", { name: "Create account" })).toHaveAttribute(
      "href",
      "/register",
    );
    unmount();

    const disabled = mockApi({ ...noSession, ...config(false) });
    const second = renderApp("/login");
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    await waitFor(() =>
      expect(disabled.mock.calls.some(([r]) => (r as Request).url.endsWith("/public/config"))).toBe(
        true,
      ),
    );
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole("link", { name: "Create account" })).not.toBeInTheDocument();
    second.unmount();

    // The public config cannot be loaded (every unknown route answers 501): no link either.
    mockApi({ ...noSession });
    renderApp("/login");
    expect(await screen.findByLabelText("Email or login")).toHaveAttribute("type", "text");
    expect(screen.queryByRole("link", { name: "Create account" })).not.toBeInTheDocument();
  });

  it("shows Sign in on the register page when enabled, and a notice when disabled", async () => {
    mockApi({ ...noSession, ...config(true) });
    const { unmount } = renderApp("/register");
    expect(await screen.findByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/login");
    unmount();

    mockApi({ ...noSession, ...config(false) });
    renderApp("/register");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Registration is turned off. Ask the administrator for an account.",
    );
    expect(screen.queryByLabelText("Email")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Sign in" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to sign in" })).toHaveAttribute("href", "/login");
  });
});

describe("register page", () => {
  it("checks the fields on the client and does not call the server", async () => {
    const fetchMock = mockApi({ ...noSession, ...config(true) });
    renderApp("/register");
    await screen.findByRole("heading", { name: "Create account" });
    await fill("", "", "");
    expect(screen.getByRole("alert")).toHaveTextContent("Check the highlighted fields.");
    const email = screen.getByLabelText("Email");
    expect(email).toHaveAttribute("aria-invalid", "true");
    expect(email).toHaveAccessibleDescription("Enter your email address");
    expect(screen.getByLabelText("Password")).toHaveAccessibleDescription(
      "Use at least 12 characters 12 to 128 characters, not your email. A passphrase works well.",
    );

    await fill("jörg@example.com", "short");
    expect(screen.getByLabelText("Email")).toHaveAccessibleDescription(
      "Use only letters a-z, digits and . _ + - @",
    );

    await userEvent.clear(screen.getByLabelText("Email"));
    await userEvent.clear(screen.getByLabelText("Password"));
    await userEvent.clear(screen.getByLabelText("Confirm password"));
    await fill("owner@example.com", PASSWORD, "something else entirely");
    expect(screen.getByLabelText("Confirm password")).toHaveAccessibleDescription(
      "The passwords do not match",
    );
    expect(registerCalls(fetchMock)).toHaveLength(0);
  });

  it("registers, keeps the CSRF token and lands on the empty monitors page", async () => {
    let session: typeof registered | null = null;
    const fetchMock = mockApi({
      "GET /api/v1/auth/me": () => (session ? json(session) : problem(401, "Unauthenticated")),
      ...config(true),
      "POST /api/v1/auth/register": () => {
        session = registered;
        return json(registered, 201);
      },
      "GET /api/v1/monitors": () =>
        json({ summary: { up: 0, down: 0, paused: 0, unknown: 0 }, items: [] }),
      "GET /api/v1/usage": () =>
        json({
          plan: { key: "free", name: "Free" },
          limits: { monitors: 3, heartbeats: 1, channels: 1 },
          usage: { monitors: 0, heartbeats: 0, channels: 0 },
        }),
    });
    renderApp("/register");
    await fill("  Owner@Example.com ", PASSWORD);

    expect(await screen.findByRole("heading", { name: "Start your first monitor" })).toBeVisible();
    expect(await screen.findByText("Your Free plan includes 3 monitors.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add monitor" })).toHaveAttribute(
      "href",
      "/monitors/new",
    );
    expect(within(screen.getByRole("banner")).getByText("owner@example.com")).toBeInTheDocument();
    expect(getCsrfToken()).toBe("csrf-reg");

    const [call] = registerCalls(fetchMock);
    expect(await call?.json()).toEqual({ email: "owner@example.com", password: PASSWORD });
    // Like login, registration needs no CSRF header (there is no session yet).
    expect(call?.headers.get("X-CSRF-Token")).toBeNull();
  });

  it.each([
    [
      "an email that is taken",
      () =>
        problem(409, "This email is already registered", {
          type: "urn:maiak:problem:email-taken",
        }),
      "Email",
      "This email is already registered. Sign in instead.",
    ],
    [
      "a server-side policy failure",
      () =>
        problem(422, "Validation failed", {
          type: "urn:maiak:problem:validation",
          fieldErrors: [{ field: "password", message: "must not be the same as the email" }],
        }),
      "Password",
      "Password must not be the same as the email",
    ],
  ])("maps %s to the field", async (_name, answer, label, message) => {
    mockApi({ ...noSession, ...config(true), "POST /api/v1/auth/register": answer });
    renderApp("/register");
    await fill("owner@example.com", PASSWORD);
    const field = await screen.findByLabelText(label);
    await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
    expect(field.getAttribute("aria-describedby")).toContain(`${label.toLowerCase()}-error`);
    expect(screen.getByText(message)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create account" })).toBeEnabled();
  });

  it("explains a disabled registration answered by the server", async () => {
    mockApi({
      ...noSession,
      ...config(true),
      "POST /api/v1/auth/register": () =>
        problem(403, "Registration is turned off", {
          type: "urn:maiak:problem:registration-disabled",
        }),
    });
    renderApp("/register");
    await fill("owner@example.com", PASSWORD);
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Registration is turned off. Ask the administrator for an account.",
    );
    expect(screen.queryByRole("button", { name: "Create account" })).not.toBeInTheDocument();
  });

  it("shows the Retry-After of a rate-limited registration", async () => {
    mockApi({
      ...noSession,
      ...config(true),
      "POST /api/v1/auth/register": () =>
        new Response(
          JSON.stringify({
            type: "urn:maiak:problem:rate-limited",
            title: "Too many attempts",
            status: 429,
          }),
          {
            status: 429,
            headers: { "Content-Type": "application/problem+json", "Retry-After": "90" },
          },
        ),
    });
    renderApp("/register");
    await fill("owner@example.com", PASSWORD);
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Too many attempts. Try again in 1 min.",
    );
  });
});
