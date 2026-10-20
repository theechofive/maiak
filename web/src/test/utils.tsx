import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { setCsrfToken } from "../api/client";
import App from "../App";
import { AuthProvider } from "../auth/AuthProvider";
import { ToastProvider } from "../components/Toasts";

type Handler = (request: Request) => Response | Promise<Response>;

export const json = (body: unknown, status = 200) => Response.json(body, { status });

export const problem = (status: number, title: string, extra: object = {}) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, ...extra }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

/**
 * Replaces the global fetch with a router keyed by "METHOD /path" (no query string). Unknown
 * routes answer 501 like the backend's unimplemented operations. Returns the mock to inspect calls.
 */
export function mockApi(routes: Record<string, Handler>) {
  const mock = vi.fn(async (input: RequestInfo | URL) => {
    const request = input instanceof Request ? input : new Request(input);
    const url = new URL(request.url);
    const handler = routes[`${request.method} ${url.pathname}`];
    if (!handler) return problem(501, "Not implemented yet");
    return handler(request);
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

export const session = {
  user: { id: "u1", email: "admin@example.com", display_name: "Admin" },
  tenant: { id: "t1", slug: "acme", name: "Acme" },
  role: "owner",
  csrf_token: "csrf-1",
  expires_at: "2030-01-01T00:00:00Z",
};

export const signedIn = { "GET /api/v1/auth/me": () => json(session) };

export function renderApp(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <AuthProvider>
          <ToastProvider>
            <App />
          </ToastProvider>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

export function renderWithClient(ui: ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(null);
});
