import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { errorMessage } from "../api/client";
import { listOrganizations, queryKeys } from "../api/endpoints";
import { useAuth } from "../auth/AuthProvider";
import { ROLE_LABELS } from "../lib/roles";
import { useToast } from "./Toasts";
import { cx, Select } from "./ui";

/**
 * The organization of the session, and a switcher when the user belongs to more than one
 * (ADR-0102). Switching replaces the session on the server; the page starts over at the monitors.
 */
export default function OrganizationSwitcher({ className }: { className?: string }) {
  const auth = useAuth();
  const navigate = useNavigate();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const current = auth.session?.tenant;
  const orgs = useQuery({
    queryKey: queryKeys.organizations,
    queryFn: listOrganizations,
    staleTime: 60_000,
    enabled: current !== undefined,
  });
  if (!current) return null;

  const items = orgs.data?.items ?? [];
  if (items.length < 2) {
    // One organization (or the list is unavailable): its name is enough.
    return (
      <span className={cx("max-w-[180px] truncate text-caption", className)} title={current.name}>
        {current.name}
      </span>
    );
  }

  async function onChange(id: string) {
    if (id === current?.id) return;
    setBusy(true);
    try {
      await auth.switchOrganization(id);
      const name = items.find((o) => o.id === id)?.name ?? "the organization";
      navigate("/", { replace: true });
      toast(`Switched to ${name}`);
    } catch (err) {
      toast(errorMessage(err), "error");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Select
      aria-label="Organization"
      className={cx("max-w-[220px]", className)}
      value={current.id}
      disabled={busy}
      onChange={(e) => void onChange(e.target.value)}
    >
      {items.map((o) => (
        <option key={o.id} value={o.id} disabled={o.status !== "active"}>
          {o.name} · {ROLE_LABELS[o.role]}
          {o.status !== "active" ? " (suspended)" : ""}
        </option>
      ))}
    </Select>
  );
}
