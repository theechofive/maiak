import type { Schemas } from "../api/client";

/**
 * The public status page (ADR-0103). The server validates everything again; these rules only
 * catch mistakes before a round trip, and the wording mirrors the server's.
 */
export type PublicStatus = Schemas["PublicStatus"];
export type PublicOverallStatus = Schemas["PublicOverallStatus"];
export type PublicComponent = Schemas["PublicComponent"];

/** Reserved slugs, the same list as internal/statuspages and the migration. */
export const RESERVED_SLUGS = new Set([
  "admin",
  "api",
  "app",
  "assets",
  "auth",
  "badge",
  "dashboard",
  "docs",
  "embed",
  "feed",
  "health",
  "healthz",
  "help",
  "login",
  "logout",
  "maiak",
  "metrics",
  "public",
  "readyz",
  "register",
  "robots",
  "root",
  "rss",
  "settings",
  "signup",
  "static",
  "status",
  "support",
  "system",
  "www",
]);

export const TITLE_MAX = 80;
export const DESCRIPTION_MAX = 500;
export const NAME_MAX = 60;
export const COMPONENTS_MAX = 50;
export const PASSWORD_MIN = 12;

export function slugError(slug: string): string | undefined {
  if (!/^[a-z0-9][a-z0-9-]{2,62}$/.test(slug)) {
    return "Use 3 to 63 lowercase letters, digits and hyphens, starting with a letter or digit.";
  }
  if (RESERVED_SLUGS.has(slug)) return "This address is reserved. Choose another.";
  return undefined;
}

/** A slug suggestion from a title: lowercase ASCII letters and digits joined by hyphens. */
export function slugFrom(title: string): string {
  const s = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/, "");
  return s.length >= 3 ? s : "";
}

/** "" is fine (no accent); otherwise #rrggbb, lowercased by the server. */
export function accentError(value: string): string | undefined {
  const v = value.trim();
  if (v === "" || /^#[0-9a-fA-F]{6}$/.test(v)) return undefined;
  return "Use a colour like #1f6feb.";
}

/** "" is fine (no logo); otherwise an https address without user name or password. */
export function logoError(value: string): string | undefined {
  const v = value.trim();
  if (v === "") return undefined;
  if (v.length > 500) return "Use at most 500 characters.";
  try {
    const u = new URL(v);
    if (
      u.protocol !== "https:" ||
      !u.hostname ||
      u.username ||
      u.password ||
      /[\s"'<>\\`]/.test(v) ||
      [...v].some((ch) => ch.charCodeAt(0) < 0x20)
    ) {
      throw new Error("bad");
    }
  } catch {
    return "Use an https:// address of an image, without user name or password.";
  }
  return undefined;
}

/** Invisible format characters (bidi overrides, zero-width spaces) that the server refuses. */
export function hasInvisible(value: string): boolean {
  return /\p{Cf}/u.test(value);
}

export const INVISIBLE_MESSAGE = "Remove invisible formatting characters.";

export function passwordError(value: string): string | undefined {
  if ([...value].length < PASSWORD_MIN) return `Use at least ${PASSWORD_MIN} characters.`;
  if (new TextEncoder().encode(value).length > 128) return "Use at most 128 bytes.";
  return undefined;
}

export const OVERALL_TEXT: Record<PublicOverallStatus, string> = {
  operational: "All systems operational",
  degraded: "Degraded performance",
  partial_outage: "Partial outage",
  major_outage: "Major outage",
  no_data: "No status data yet",
};

export const COMPONENT_TEXT: Record<PublicStatus, string> = {
  operational: "Operational",
  degraded: "Degraded",
  outage: "Outage",
  no_data: "No data",
};

/** Consecutive components with the same group are shown together; no group shares a block too. */
export function groupComponents<T extends { group: string | null }>(
  items: T[],
): { name: string | null; items: T[] }[] {
  const out: { name: string | null; items: T[] }[] = [];
  for (const it of items) {
    const last = out[out.length - 1];
    if (last && last.name === it.group) last.items.push(it);
    else out.push({ name: it.group, items: [it] });
  }
  return out;
}

/** Moves one item of a list by delta places (clamped); a new array. */
export function move<T>(list: T[], index: number, delta: number): T[] {
  const to = Math.max(0, Math.min(list.length - 1, index + delta));
  if (to === index) return list;
  const out = [...list];
  out.splice(to, 0, ...out.splice(index, 1));
  return out;
}

export function formatUptime(value: number | null | undefined): string {
  if (value === null || value === undefined) return "No data";
  const s = value >= 99.995 && value < 100 ? "99.99" : value.toFixed(2);
  return `${s} % uptime`;
}

/** The two embed snippets of a public page: a script that adds the badge, or the iframe itself. */
export function embedSnippets(publicUrl: string): { script: string; iframe: string } {
  return {
    script: `<script src="${publicUrl}/embed.js" async></script>`,
    iframe: `<iframe src="${publicUrl}/badge" title="Status" width="260" height="40" style="border:0" loading="lazy"></iframe>`,
  };
}
