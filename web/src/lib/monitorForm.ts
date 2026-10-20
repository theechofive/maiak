import { ApiError, errorMessage } from "../api/client";
import type { MonitorCreate, MonitorDetail } from "../api/endpoints";

export interface MonitorFormValues {
  name: string;
  url: string;
  expectedMode: "2xx" | "exact";
  expectedCode: string;
  failureThreshold: number;
  notifySlack: boolean;
}

export const formFields = [
  "name",
  "url",
  "expectedStatus",
  "failureThreshold",
  "notifySlack",
] as const;
export type FormField = (typeof formFields)[number];
export type MonitorFormErrors = Partial<Record<FormField, string>>;

export const NAME_MAX = 80;
export const URL_MAX = 2048;

/** ADR-0018: `2xx`, an exact code 200-299, or an exact code 400-599 (1xx and 3xx are rejected). */
export const EXPECTED_STATUS_PATTERN = /^(2xx|2[0-9]{2}|[45][0-9]{2})$/;
export const EXPECTED_STATUS_MESSAGE = "Use 2xx, a code from 200 to 299, or a code from 400 to 599";

export const emptyMonitorForm: MonitorFormValues = {
  name: "",
  url: "",
  expectedMode: "2xx",
  expectedCode: "200",
  failureThreshold: 2,
  notifySlack: true,
};

export function valuesFromMonitor(m: MonitorDetail): MonitorFormValues {
  const exact = m.expectedStatus !== "2xx";
  return {
    name: m.name,
    url: m.url,
    expectedMode: exact ? "exact" : "2xx",
    expectedCode: exact ? m.expectedStatus : "200",
    failureThreshold: m.failureThreshold,
    notifySlack: m.notifySlack,
  };
}

/** Mirrors the server's URL policy that can be checked without DNS: https, no credentials, port 443. */
export function validateMonitorUrl(raw: string): string | undefined {
  const value = raw.trim();
  if (!value) return "Enter a URL.";
  if (value.length > URL_MAX) return `Use ${URL_MAX} characters or fewer.`;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return "Enter a full URL, such as https://example.com/health.";
  }
  if (url.protocol !== "https:") return "Use an https:// URL.";
  if (!url.hostname) return "Enter a host name.";
  if (url.username || url.password) return "Remove the user name and password from the URL.";
  // URL drops the default port, so any remaining port is not 443.
  if (url.port !== "") return "Only port 443 is allowed.";
  return undefined;
}

export function validateMonitorForm(v: MonitorFormValues): MonitorFormErrors {
  const errors: MonitorFormErrors = {};
  const name = v.name.trim();
  if (!name) errors.name = "Enter a name.";
  else if ([...name].length > NAME_MAX) errors.name = `Use ${NAME_MAX} characters or fewer.`;
  const urlError = validateMonitorUrl(v.url);
  if (urlError) errors.url = urlError;
  if (v.expectedMode === "exact" && !EXPECTED_STATUS_PATTERN.test(v.expectedCode.trim())) {
    errors.expectedStatus = EXPECTED_STATUS_MESSAGE;
  }
  if (![1, 2, 3].includes(v.failureThreshold)) {
    errors.failureThreshold = "Choose 1, 2 or 3 failed checks.";
  }
  return errors;
}

export function toMonitorRequest(v: MonitorFormValues) {
  return {
    name: v.name.trim(),
    url: v.url.trim(),
    expectedStatus: v.expectedMode === "2xx" ? "2xx" : v.expectedCode.trim(),
    failureThreshold: v.failureThreshold,
    notifySlack: v.notifySlack,
  } satisfies MonitorCreate;
}

/**
 * Splits a failed save into messages for form fields and one general message. Field errors for
 * fields the form does not show (for example `intervalSeconds`) go into the general message.
 */
export function mapServerErrors(err: unknown): {
  fields: MonitorFormErrors;
  general?: string;
} {
  if (!(err instanceof ApiError)) return { fields: {}, general: errorMessage(err) };
  const fields: MonitorFormErrors = {};
  const other: string[] = [];
  for (const fe of err.fieldErrors) {
    const field = formFields.find((f) => f === fe.field);
    if (field) fields[field] ??= capitalize(fe.message);
    else other.push(`${fe.field}: ${fe.message}`);
  }
  if (other.length > 0) return { fields, general: other.join(" ") };
  if (Object.keys(fields).length > 0) return { fields };
  return { fields, general: errorMessage(err) };
}

const capitalize = (s: string) => (s ? s[0]!.toUpperCase() + s.slice(1) : s);
