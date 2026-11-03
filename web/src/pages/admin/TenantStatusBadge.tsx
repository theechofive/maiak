import type { AdminTenant } from "../../api/endpoints";
import { Badge, DiamondShape } from "../../components/ui";

/** "Deleted", "Suspended" or "Active"; a deleted tenant is also suspended in the API. */
export function tenantStatusText(tenant: Pick<AdminTenant, "status" | "deletedAt">): string {
  if (tenant.deletedAt) return "Deleted";
  return tenant.status === "suspended" ? "Suspended" : "Active";
}

/** The row tone for a tenant: red is for outages, so a suspension is amber. */
export function tenantTone(
  tenant: Pick<AdminTenant, "status" | "deletedAt">,
): "default" | "warning" | "deleted" {
  if (tenant.deletedAt) return "deleted";
  return tenant.status === "suspended" ? "warning" : "default";
}

export function TenantStatusBadge({
  tenant,
}: {
  tenant: Pick<AdminTenant, "status" | "deletedAt">;
}) {
  const text = tenantStatusText(tenant);
  if (text === "Deleted") return <Badge tone="deleted">{text}</Badge>;
  if (text === "Suspended")
    return (
      <Badge tone="warning" icon={<DiamondShape />}>
        {text}
      </Badge>
    );
  return <Badge>{text}</Badge>;
}
