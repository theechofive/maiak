import { useQuery } from "@tanstack/react-query";
import { errorMessage, errorMeta } from "../../api/client";
import { getAdminStats, queryKeys, type AdminStats } from "../../api/endpoints";
import {
  ErrorState,
  Metric,
  MetricStrip,
  Skeleton,
  Table,
  Td,
  Th,
  Tr,
  Loading,
} from "../../components/ui";

export const adminTitle =
  "pt-6 pb-5 text-headline leading-[1.1] font-semibold tracking-[-0.02em] sm:pt-9 sm:pb-6 sm:text-display";

/** Platform-wide counts for the platform admin (ADR-0097). */
export default function AdminOverview() {
  const stats = useQuery({ queryKey: queryKeys.adminStats, queryFn: getAdminStats });

  return (
    <section className="flex flex-col">
      <h1 className={adminTitle}>Admin</h1>
      {stats.isPending && (
        <Loading label="Loading counts…">
          <Skeleton className="h-16 w-full" />
        </Loading>
      )}
      {stats.isError && (
        <ErrorState
          title="Cannot load the counts"
          detail={errorMessage(stats.error)}
          meta={errorMeta(stats.error)}
          onRetry={() => void stats.refetch()}
        />
      )}
      {stats.data && <Overview stats={stats.data} />}
    </section>
  );
}

function Overview({ stats }: { stats: AdminStats }) {
  const { active, suspended, deleted } = stats.tenants;
  const all = active + suspended + deleted;
  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-3">
        <MetricStrip>
          <Metric
            label="Tenants"
            value={stats.tenants.total}
            note={`${active} active · ${suspended} suspended · ${deleted} deleted`}
          />
          <Metric label="Users" value={stats.users} />
          <Metric label="Monitors" value={stats.monitors} />
          <Metric label="Watchdogs" value={stats.heartbeats} />
        </MetricStrip>
        {all > 0 && (
          // One bar for every tenant ever created: active, suspended, deleted.
          <div aria-hidden="true" className="flex h-1.5 gap-px">
            {[
              { n: active, className: "bg-ribbon" },
              { n: suspended, className: "bg-warning" },
              { n: deleted, className: "border border-dashed border-input" },
            ]
              .filter((s) => s.n > 0)
              .map((s, i) => (
                <span key={i} className={s.className} style={{ flexGrow: s.n }} />
              ))}
          </div>
        )}
      </div>

      <section aria-labelledby="plans-heading" className="flex flex-col gap-3">
        <h2 id="plans-heading" className="text-lead font-semibold">
          Tenants by plan
        </h2>
        <Table className="max-w-[560px]">
          <thead>
            <tr>
              <Th>Plan</Th>
              <Th>Key</Th>
              <Th numeric>Tenants</Th>
            </tr>
          </thead>
          <tbody>
            {stats.plans.map((p) => (
              <Tr key={p.key}>
                <Td>{p.name}</Td>
                <Td className="font-mono text-mono text-muted-foreground">{p.key}</Td>
                <Td numeric>{p.tenants}</Td>
              </Tr>
            ))}
          </tbody>
        </Table>
        <p className="text-caption text-muted-foreground">
          Deleted tenants are counted only under &ldquo;deleted&rdquo;.
        </p>
      </section>
    </div>
  );
}
