import {
  ApiError,
  apiFetch,
  CSRF_PROBLEM_TYPE,
  client,
  errorMessage,
  setCsrfToken,
  setUnauthorizedHandler,
  unwrap,
} from "./client";

const fetchMock = vi.fn<typeof fetch>();

function problem(status: number, body: object): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

function lastRequest(): Request {
  const call = fetchMock.mock.calls.at(-1);
  if (!call) throw new Error("fetch was not called");
  return call[0] as Request;
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  setCsrfToken("token-123");
});

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

describe("apiFetch", () => {
  it.each(["POST", "PUT", "PATCH", "DELETE"])("adds X-CSRF-Token to %s", async (method) => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await apiFetch(new Request("http://localhost/api/v1/x", { method }));
    expect(lastRequest().headers.get("X-CSRF-Token")).toBe("token-123");
  });

  it.each(["GET", "HEAD"])("does not add X-CSRF-Token to %s", async (method) => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await apiFetch(new Request("http://localhost/api/v1/x", { method }));
    expect(lastRequest().headers.has("X-CSRF-Token")).toBe(false);
  });

  it("sends cookies only to the same origin", async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await apiFetch(new Request("http://localhost/api/v1/x"));
    expect(lastRequest().credentials).toBe("same-origin");
  });

  it("calls the 401 handler on any 401 except login", async () => {
    const handler = vi.fn();
    const unregister = setUnauthorizedHandler(handler);
    fetchMock.mockImplementation(async () => problem(401, { type: "x", title: "No", status: 401 }));

    await apiFetch(new Request("http://localhost/api/v1/monitors"));
    expect(handler).toHaveBeenCalledTimes(1);

    await apiFetch(new Request("http://localhost/api/v1/auth/login", { method: "POST" }));
    expect(handler).toHaveBeenCalledTimes(1);

    unregister();
    await apiFetch(new Request("http://localhost/api/v1/monitors"));
    expect(handler).toHaveBeenCalledTimes(1);
  });
});

describe("generated client through the wrapper", () => {
  it("sends the CSRF header on unsafe calls without the caller passing it", async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await unwrap(client.POST("/api/v1/settings/slack/test"));
    expect(lastRequest().method).toBe("POST");
    expect(lastRequest().headers.get("X-CSRF-Token")).toBe("token-123");
  });

  it("sends no CSRF header on a GET even with a token", async () => {
    fetchMock.mockResolvedValue(Response.json({ configured: true }));
    await unwrap(client.GET("/api/v1/settings/slack"));
    expect(lastRequest().method).toBe("GET");
    expect(lastRequest().headers.has("X-CSRF-Token")).toBe(false);
  });

  it("asks for a reload only when a 403 is the CSRF problem", () => {
    const csrf = new ApiError({ type: CSRF_PROBLEM_TYPE, title: "Forbidden", status: 403 });
    expect(errorMessage(csrf)).toMatch(/reload the page/i);
    const other = new ApiError({
      type: "urn:maiak:problem:forbidden",
      title: "Forbidden",
      status: 403,
      detail: "Your role cannot change settings.",
    });
    expect(errorMessage(other)).toBe("Your role cannot change settings.");
  });

  it("reads Retry-After seconds into the ApiError", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ title: "Slow down", status: 429 }), {
        status: 429,
        headers: { "Content-Type": "application/problem+json", "Retry-After": "30" },
      }),
    );
    const err = (await unwrap(client.GET("/api/v1/monitors")).catch((e: unknown) => e)) as ApiError;
    expect(err.retryAfter).toBe(30);
  });

  it("returns the parsed body on success", async () => {
    fetchMock.mockResolvedValue(Response.json({ configured: true }));
    await expect(unwrap(client.GET("/api/v1/settings/slack"))).resolves.toEqual({
      configured: true,
    });
  });

  it("turns problem+json with fieldErrors into a typed ApiError", async () => {
    fetchMock.mockResolvedValue(
      problem(422, {
        type: "urn:maiak:problem:validation",
        title: "Validation failed",
        status: 422,
        detail: "The monitor is invalid",
        request_id: "req-1",
        fieldErrors: [
          { field: "url", message: "must use https" },
          { field: "url", message: "second message" },
          { field: "name", message: "is required" },
        ],
      }),
    );
    const err = await unwrap(
      client.POST("/api/v1/monitors", { body: { name: "", url: "http://x" } }),
    ).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const apiErr = err as ApiError;
    expect(apiErr.status).toBe(422);
    expect(apiErr.title).toBe("Validation failed");
    expect(apiErr.detail).toBe("The monitor is invalid");
    expect(apiErr.requestId).toBe("req-1");
    expect(apiErr.fieldErrorMap()).toEqual({ url: "must use https", name: "is required" });
  });

  it("builds an ApiError from a response that is not problem+json", async () => {
    fetchMock.mockResolvedValue(
      new Response("<html>bad gateway</html>", { status: 502, statusText: "Bad Gateway" }),
    );
    const err = (await unwrap(client.GET("/api/v1/monitors")).catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(502);
    expect(err.title).toBe("Bad Gateway");
    expect(err.fieldErrors).toEqual([]);
  });

  it("reports an unreachable server as status 0", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    const err = (await unwrap(client.GET("/api/v1/monitors")).catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(0);
    expect(errorMessage(err)).toMatch(/cannot reach the server/i);
  });

  it("explains 501 from endpoints that are not implemented yet", async () => {
    fetchMock.mockResolvedValue(
      problem(501, {
        type: "urn:maiak:problem:not-implemented",
        title: "Not implemented yet",
        status: 501,
      }),
    );
    const err = await unwrap(client.GET("/api/v1/monitors")).catch((e: unknown) => e);
    expect(errorMessage(err)).toBe("This feature is not available yet.");
  });
});
