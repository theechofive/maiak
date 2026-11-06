import { useQuery } from "@tanstack/react-query";
import { ApiError } from "../api/client";
import {
  getAuthProviders,
  queryKeys,
  startOAuth,
  type AuthProviders,
  type OAuthProvider,
} from "../api/endpoints";

/** Problem types of sign-in methods and domains (ADR-0104, O-04). */
export const LAST_LOGIN_METHOD_PROBLEM_TYPE = "urn:maiak:problem:last-login-method";
export const DOMAIN_NOT_ALLOWED_PROBLEM_TYPE = "urn:maiak:problem:domain-not-allowed";
export const DOMAIN_NOT_VERIFIED_PROBLEM_TYPE = "urn:maiak:problem:domain-not-verified";
export const DOMAIN_TAKEN_PROBLEM_TYPE = "urn:maiak:problem:domain-taken";
export const PASSWORD_LOGIN_DISABLED_PROBLEM_TYPE = "urn:maiak:problem:password-login-disabled";

/** The sign-in methods the login page offers; undefined while loading or when it cannot load. */
export function useAuthProviders(): AuthProviders | undefined {
  const query = useQuery({
    queryKey: queryKeys.authProviders,
    queryFn: getAuthProviders,
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  });
  return query.data;
}

/** Leaves the app for the provider. A seam so tests can watch the navigation. */
export const browser = {
  go(url: string) {
    globalThis.location.assign(url);
  },
};

/** Starts the flow and navigates the whole window to the provider. */
export async function goToProvider(
  provider: OAuthProvider,
  body: { intent?: "login" | "link"; inviteToken?: string },
): Promise<void> {
  const { authorizationUrl } = await startOAuth(provider, body);
  browser.go(authorizationUrl);
}

const MESSAGES: Record<string, string> = {
  cancelled: "Sign-in was cancelled at the provider.",
  failed: "Sign-in with the provider failed. Try again.",
  state_invalid:
    "The sign-in took too long or was started in another browser. Start it again from this page.",
  email_unverified: "The provider has no verified email address for this account.",
  email_unusable: "The provider's email address cannot be used as a Maiak login.",
  not_invited:
    "There is no Maiak account for this sign-in. Ask an administrator of your organization for an invitation.",
  account_exists:
    "An account with this email already exists. Sign in with its password, then link the provider on your profile page.",
  invitation_invalid: "The invitation was used, revoked or expired. Ask for a new link.",
  already_member: "This account is already a member of that organization.",
  plan_limit: "The organization has reached its member limit. Ask its administrator.",
  suspended: "This account's organization is suspended. Contact the administrator of this Maiak.",
  rate_limited: "Too many sign-in attempts. Try again in a few minutes.",
  identity_in_use: "This provider account is already linked to another Maiak user.",
  already_linked: "You already have an account of this provider linked. Unlink it first.",
  session_expired: "Your session changed while linking. Sign in and try again.",
};

/** The plain message for an error code of the OAuth callback redirect (never the provider's text). */
export function oauthErrorMessage(code: string | null): string | undefined {
  if (!code) return undefined;
  return MESSAGES[code] ?? MESSAGES.failed;
}

/** The message for a failed start request. */
export function startErrorMessage(err: unknown): string {
  if (err instanceof ApiError && err.status === 429) return MESSAGES.rate_limited!;
  if (err instanceof ApiError && err.status === 404) {
    return "This invitation or sign-in method is no longer available.";
  }
  if (err instanceof ApiError && err.status === 503) {
    return "The provider cannot be reached right now. Try again later.";
  }
  return "Cannot start the sign-in. Try again.";
}
