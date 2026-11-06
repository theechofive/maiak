import type { Channel, ChannelCreate, ChannelEvent, ChannelUpdate } from "../api/endpoints";
import { validateSlackUrl } from "./slack";

export const CHANNEL_NAME_MAX = 60;
export const MENTIONS_MAX = 3;
export const REMINDER_MIN = 5;
export const REMINDER_MAX = 1440;

export const channelEvents: { value: ChannelEvent; label: string }[] = [
  { value: "down", label: "Down" },
  { value: "recovered", label: "Recovered" },
  { value: "tls_expiring", label: "TLS certificate expiring" },
];

/** Mirrors the server rule (ADR-0100): @here, @channel, a user group or a user. */
export const MENTION_PATTERN =
  /^(@here|@channel|<!subteam\^[A-Z0-9]{2,20}>|<@[UW][A-Z0-9]{2,20}>)$/;

export interface ChannelFormValues {
  name: string;
  /** Empty when editing: the stored webhook is kept. */
  webhookUrl: string;
  events: ChannelEvent[];
  /** Space-separated mentions, as typed. */
  mention: string;
  /** Minutes as typed; empty or 0 turns reminders off. */
  reminderMinutes: string;
  /** A project id, ORGANIZATION_WIDE, or empty for the Default project (T-02). */
  project: string;
}

/** The project choice of an organization-wide channel (no project, every project may use it). */
export const ORGANIZATION_WIDE = "organization";

export type ChannelFormErrors = Partial<Record<keyof ChannelFormValues, string>>;

export const emptyChannelForm: ChannelFormValues = {
  name: "",
  webhookUrl: "",
  events: ["down", "recovered", "tls_expiring"],
  mention: "",
  reminderMinutes: "0",
  project: "",
};

export function valuesFromChannel(c: Channel): ChannelFormValues {
  return {
    name: c.name,
    webhookUrl: "",
    events: c.events,
    mention: c.mention.join(" "),
    reminderMinutes: String(c.reminderMinutes),
    project: c.projectId ?? ORGANIZATION_WIDE,
  };
}

export function parseMention(raw: string): string[] {
  return raw.split(/\s+/).filter(Boolean);
}

function reminderOf(raw: string): number | undefined {
  const value = raw.trim();
  if (value === "") return 0;
  if (!/^[0-9]+$/.test(value)) return undefined;
  const n = Number(value);
  return n === 0 || (n >= REMINDER_MIN && n <= REMINDER_MAX) ? n : undefined;
}

/** Client-side checks; the server repeats them. requireWebhook is true when creating. */
export function validateChannelForm(
  v: ChannelFormValues,
  requireWebhook: boolean,
): ChannelFormErrors {
  const errors: ChannelFormErrors = {};
  const name = v.name.trim();
  if (!name) errors.name = "Enter a name.";
  else if ([...name].length > CHANNEL_NAME_MAX)
    errors.name = `Use ${CHANNEL_NAME_MAX} characters or fewer.`;
  if (requireWebhook || v.webhookUrl.trim() !== "") {
    const urlError = validateSlackUrl(v.webhookUrl);
    if (urlError) errors.webhookUrl = urlError;
  }
  if (v.events.length === 0) errors.events = "Choose at least one event.";
  const mentions = parseMention(v.mention);
  if (mentions.some((m) => !MENTION_PATTERN.test(m))) {
    errors.mention = "Use @here, @channel, <!subteam^ID> or <@UID>, separated by spaces.";
  } else if (new Set(mentions).size > MENTIONS_MAX) {
    errors.mention = `Use at most ${MENTIONS_MAX} mentions.`;
  }
  if (reminderOf(v.reminderMinutes) === undefined) {
    errors.reminderMinutes = `Enter 0 (off) or whole minutes from ${REMINDER_MIN} to ${REMINDER_MAX}.`;
  }
  return errors;
}

export function toChannelCreate(v: ChannelFormValues): ChannelCreate {
  const scope =
    v.project === ORGANIZATION_WIDE
      ? { organizationWide: true }
      : v.project
        ? { projectId: v.project }
        : {};
  return {
    name: v.name.trim(),
    webhookUrl: v.webhookUrl.trim(),
    events: v.events,
    mention: [...new Set(parseMention(v.mention))],
    reminderMinutes: reminderOf(v.reminderMinutes) ?? 0,
    ...scope,
  };
}

/** The PATCH body; the webhook is sent only when a new one was typed. */
export function toChannelUpdate(v: ChannelFormValues): ChannelUpdate {
  const { webhookUrl, ...rest } = toChannelCreate(v);
  return webhookUrl ? { ...rest, webhookUrl } : rest;
}

/** "Down, Recovered" */
export function eventsLabel(events: ChannelEvent[]): string {
  return channelEvents
    .filter((e) => events.includes(e.value))
    .map((e) => e.label)
    .join(", ");
}

export function reminderLabel(minutes: number): string {
  if (minutes === 0) return "Off";
  if (minutes % 60 === 0) return `Every ${minutes / 60} h`;
  return `Every ${minutes} min`;
}
