import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { getCsrfToken } from "./api/client";
import { listMonitors } from "./api/endpoints";
import { redirectTarget } from "./auth/RequireAuth";
import { json, mockApi, problem, renderApp, session, signedIn } from "./test/utils";

describe("auth guard", () => {
  it("redirects to login without a session and returns to the target after sign-in", async () => {
    let loggedIn = false;
    const fetchMock = mockApi({
      "GET /api/v1/auth/me": () => (loggedIn ? json(session) : problem(401, "Unauthenticated")),
      "POST /api/v1/auth/login": () => {
        loggedIn = true;
        return json(session);
      },
      "GET /api/v1/channels": () => json({ items: [] }),
    });
    renderApp("/settings");

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    const loginField = screen.getByLabelText("Email or login");
    expect(loginField).toHaveAttribute("type", "text");
    expect(loginField).toHaveAttribute("autocomplete", "username");
    expect(loginField).toHaveValue("");
    await userEvent.type(loginField, " admin ");
    await userEvent.type(screen.getByLabelText("Password"), "secret");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(getCsrfToken()).toBe("csrf-1");
    const loginCall = fetchMock.mock.calls
      .map(([r]) => r as Request)
      .find((r) => r.url.endsWith("/auth/login"));
    expect(await loginCall?.json()).toEqual({ login: "admin", password: "secret" });
  });

  it("shows the generic error on wrong credentials", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "Unauthenticated"),
      "POST /api/v1/auth/login": () => problem(401, "Invalid credentials"),
    });
    renderApp("/login");
    await userEvent.type(await screen.findByLabelText("Email or login"), "nobody");
    await userEvent.type(screen.getByLabelText("Password"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Wrong login or password. Try again.",
    );
    // The login stays, the password is cleared and gets the focus for the next try.
    expect(screen.getByLabelText("Email or login")).toHaveValue("nobody");
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(screen.getByLabelText("Password")).toHaveFocus();
  });

  it("shows and hides the password", async () => {
    mockApi({ "GET /api/v1/auth/me": () => problem(401, "Unauthenticated") });
    renderApp("/login");
    const password = await screen.findByLabelText("Password");
    expect(password).toHaveAttribute("type", "password");
    await userEvent.click(screen.getByRole("button", { name: "Show password" }));
    expect(password).toHaveAttribute("type", "text");
    await userEvent.click(screen.getByRole("button", { name: "Hide password" }));
    expect(password).toHaveAttribute("type", "password");
  });

  it("renders protected pages with a session", async () => {
    mockApi({ ...signedIn, "GET /api/v1/channels": () => json({ items: [] }) });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(getCsrfToken()).toBe("csrf-1");
    // The top bar shows the login.
    expect(within(screen.getByRole("banner")).getByText("admin")).toBeInTheDocument();
  });

  it("goes back to login when any request answers 401", async () => {
    mockApi({
      ...signedIn,
      "GET /api/v1/channels": () => json({ items: [] }),
      "GET /api/v1/monitors": () => problem(401, "Session expired"),
    });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    await act(() => listMonitors().catch(() => undefined));
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(getCsrfToken()).toBeNull();
  });

  it("shows an error state when the session check fails for another reason", async () => {
    mockApi({ "GET /api/v1/auth/me": () => problem(500, "Internal error") });
    renderApp("/");
    expect(await screen.findByRole("alert")).toHaveTextContent("Cannot load your session");
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("logs out and returns to login", async () => {
    let loggedIn = true;
    const fetchMock = mockApi({
      "GET /api/v1/auth/me": () => (loggedIn ? json(session) : problem(401, "Unauthenticated")),
      "POST /api/v1/auth/logout": () => {
        loggedIn = false;
        return new Response(null, { status: 204 });
      },
      "GET /api/v1/channels": () => json({ items: [] }),
    });
    renderApp("/settings");
    await userEvent.click(await screen.findByRole("button", { name: "Log out" }));
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    const logoutCall = fetchMock.mock.calls
      .map(([r]) => r as Request)
      .find((r) => r.url.endsWith("/auth/logout"));
    expect(logoutCall?.headers.get("X-CSRF-Token")).toBe("csrf-1");
    await waitFor(() => expect(getCsrfToken()).toBeNull());
  });
});

describe("redirectTarget", () => {
  it.each([
    [{ from: "/monitors/42?x=1" }, "/monitors/42?x=1"],
    [{ from: "//evil.example" }, "/"],
    [{ from: "https://evil.example" }, "/"],
    [{ from: "/login" }, "/"],
    [{ from: "/\\evil.example" }, "/"],
    [{ from: "/monitors\\42" }, "/"],
    [null, "/"],
    [undefined, "/"],
  ])("maps %j to %s", (state, expected) => {
    expect(redirectTarget(state)).toBe(expected);
  });
});

describe("login rate limit", () => {
  const tooMany = (headers: Record<string, string>) => () =>
    new Response(JSON.stringify({ type: "about:blank", title: "Too many requests", status: 429 }), {
      status: 429,
      headers: { "Content-Type": "application/problem+json", ...headers },
    });

  const submit = async () => {
    renderApp("/login");
    await userEvent.type(await screen.findByLabelText("Email or login"), "admin");
    await userEvent.type(screen.getByLabelText("Password"), "wrong");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  };

  it("shows the Retry-After seconds", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "No session"),
      "POST /api/v1/auth/login": tooMany({ "Retry-After": "45" }),
    });
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Too many attempts. Try again in 45 s.",
    );
    // Not a wrong password: it stays, so the next try needs no retyping.
    expect(screen.getByLabelText("Password")).toHaveValue("wrong");
  });

  it("falls back to a generic message without a usable Retry-After", async () => {
    mockApi({
      "GET /api/v1/auth/me": () => problem(401, "No session"),
      "POST /api/v1/auth/login": tooMany({ "Retry-After": "soon" }),
    });
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Too many attempts. Try again later.",
    );
  });
});
