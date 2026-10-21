import createClient from "openapi-fetch";
import type { components, paths } from "./generated/schema";

export type Schemas = components["schemas"];
export type FieldError = Schemas["FieldError"];
export type Problem = Schemas["Problem"];

const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);
/** Problem type of a rejected CSRF token (backend `urn:maiak:problem:csrf`). */
export const CSRF_PROBLEM_TYPE = "urn:maiak:problem:csrf";
/** Problem type of a plan quota failure (409, with the members `limit` and `max`, ADR-0093). */
export const PLAN_LIMIT_PROBLEM_TYPE = "urn:maiak:problem:plan-limit-reached";
const LOGIN_PATH = "/api/v1/auth/login";

// The CSRF token lives only in memory (ADR-0015): it is fetched again from /auth/me after a reload.
let csrfToken: string | null = null;

export function setCsrfToken(token: string | null): void {
  csrfToken = token;
}

export function getCsrfToken(): string | null {
  return csrfToken;
}

let unauthorizedHandler: (() => void) | null = null;

/** Registers the function called on any 401 (except a failed login). Returns an unregister function. */
export function setUnauthorizedHandler(handler: () => void): () => void {
  unauthorizedHandler = handler;
  return () => {
    if (unauthorizedHandler === handler) unauthorizedHandler = null;
  };
}

/** A failed API request: the problem+json body (RFC 9457) or a synthesized one. */
export class ApiError extends Error {
  readonly status: number;
  readonly type: string;
  readonly title: string;
  readonly detail?: string;
  readonly requestId?: string;
  readonly fieldErrors: FieldError[];
  /** Seconds from the Retry-After header (429), when present and valid. */
  readonly retryAfter?: number;
  /** Which plan limit was reached and its value (plan-limit problems only). */
  readonly limit?: Problem["limit"];
  readonly max?: number;

  constructor(problem: Problem, retryAfter?: number) {
    super(problem.detail ?? problem.title);
    this.name = "ApiError";
    this.status = problem.status;
    this.type = problem.type;
    this.title = problem.title;
    this.detail = problem.detail;
    this.requestId = problem.request_id;
    this.fieldErrors = problem.fieldErrors ?? [];
    this.retryAfter = retryAfter;
    this.limit = problem.limit;
    this.max = problem.max;
  }

  /** Field errors keyed by field name; the first message wins. */
  fieldErrorMap(): Record<string, string> {
    const map: Record<string, string> = {};
    for (const fe of this.fieldErrors) map[fe.field] ??= fe.message;
    return map;
  }
}

function isProblem(value: unknown): value is Problem {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return typeof v.title === "string" && typeof v.status === "number";
}

function isFieldError(value: unknown): value is FieldError {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return typeof v.field === "string" && typeof v.message === "string";
}

const PLAN_RESOURCES = new Set<unknown>(["monitors", "heartbeats", "channels"]);

function isPlanResource(value: unknown): value is NonNullable<Problem["limit"]> {
  return PLAN_RESOURCES.has(value);
}

const PLAN_RESOURCE_NAMES: Record<NonNullable<Problem["limit"]>, [string, string]> = {
  monitors: ["monitor", "monitors"],
  heartbeats: ["heartbeat monitor", "heartbeat monitors"],
  channels: ["notification channel", "notification channels"],
};

/** A user-facing message for a plan-limit problem, or undefined for any other error. */
export function planLimitMessage(err: unknown): string | undefined {
  if (!(err instanceof ApiError) || err.type !== PLAN_LIMIT_PROBLEM_TYPE) return undefined;
  if (!err.limit || err.max === undefined) return err.detail ?? err.title;
  const [one, many] = PLAN_RESOURCE_NAMES[err.limit];
  const what = `${err.max} ${err.max === 1 ? one : many}`;
  const hint = err.limit === "channels" ? "" : ` Delete a ${one} to add a new one.`;
  return `Your plan allows at most ${what}, and the limit is reached.${hint}`;
}

/** Builds an ApiError from an error response and its parsed body (whatever shape it has). */
export function toApiError(response: Response, body: unknown): ApiError {
  const header = response.headers.get("Retry-After");
  const seconds = header !== null && /^\d+$/.test(header.trim()) ? Number(header) : undefined;
  if (isProblem(body)) {
    const fieldErrors = Array.isArray(body.fieldErrors)
      ? body.fieldErrors.filter(isFieldError)
      : [];
    return new ApiError(
      {
        ...body,
        type: typeof body.type === "string" ? body.type : "about:blank",
        status: response.status,
        fieldErrors,
        limit: isPlanResource(body.limit) ? body.limit : undefined,
        max: typeof body.max === "number" ? body.max : undefined,
      },
      seconds,
    );
  }
  return new ApiError(
    {
      type: "about:blank",
      title: response.statusText || `HTTP ${response.status}`,
      status: response.status,
    },
    seconds,
  );
}

/**
 * The fetch used by the generated client: adds the CSRF header to unsafe methods, keeps cookies
 * same-origin and reports 401 to the registered handler.
 */
export async function apiFetch(request: Request): Promise<Response> {
  const headers = new Headers(request.headers);
  if (UNSAFE_METHODS.has(request.method.toUpperCase()) && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken);
  }
  const response = await fetch(new Request(request, { headers, credentials: "same-origin" }));
  if (response.status === 401 && new URL(request.url).pathname !== LOGIN_PATH) {
    unauthorizedHandler?.();
  }
  return response;
}

// The spec declares X-CSRF-Token as a required header parameter (ADR-0017); apiFetch supplies it,
// so callers do not pass it.
type WithoutCsrf<Op> = Op extends { parameters: infer Params }
  ? Params extends { header: infer Header }
    ? Omit<Op, "parameters"> & {
        parameters: Omit<Params, "header"> & { header?: Omit<Header, "X-CSRF-Token"> };
      }
    : Op
  : Op;

export type ClientPaths = {
  [Path in keyof paths]: { [Method in keyof paths[Path]]: WithoutCsrf<paths[Path][Method]> };
};

export const client = createClient<ClientPaths>({
  baseUrl: globalThis.location?.origin ?? "",
  credentials: "same-origin",
  // Late-bound so tests can replace the global fetch.
  fetch: (request) => apiFetch(request),
});

type Result<T> = { data?: T; error?: unknown; response: Response };

/** Resolves to the response data or throws an ApiError (status 0 when the server is unreachable). */
export async function unwrap<T>(call: Promise<Result<T>>): Promise<T> {
  let result: Result<T>;
  try {
    result = await call;
  } catch (err) {
    throw new ApiError({
      type: "about:blank",
      title: "Cannot reach the server",
      status: 0,
      detail: err instanceof Error ? err.message : undefined,
    });
  }
  if (!result.response.ok) throw toApiError(result.response, result.error);
  return result.data as T;
}

/** A short, user-facing message for any error. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return "Cannot reach the server. Check your connection and try again.";
    if (err.status === 501) return "This feature is not available yet.";
    if (err.status === 403 && err.type === CSRF_PROBLEM_TYPE) {
      return "The request was refused. Reload the page and try again.";
    }
    if (err.status >= 500) return "The server failed to answer. Try again later.";
    return planLimitMessage(err) ?? err.detail ?? err.title;
  }
  return "Something went wrong. Try again.";
}
