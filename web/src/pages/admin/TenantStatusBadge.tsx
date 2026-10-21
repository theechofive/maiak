import type { AdminTenant } from "../../api/endpoints";

/** "Deleted", "Suspended" or "Active"; a deleted tenant is also suspended in the API. */
export function tenantStatusText(tenant: Pick<AdminTenant, "status" | "deletedAt">): string {
  if (tenant.deletedAt) return "Deleted";
  return tenant.status === "suspended" ? "Suspended" : "Active";
}

export function TenantStatusBadge({
  tenant,
}: {
  tenant: Pick<AdminTenant, "status" | "deletedAt">;
}) {
  const text = tenantStatusText(tenant);
  return <span className={text === "Active" ? "status-up" : "status-down"}>{text}</span>;
}
