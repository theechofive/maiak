import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { getAdminStats, queryKeys } from "../../api/endpoints";
import { ErrorState, Loading } from "../../components/QueryState";

function Card({ label, value, note }: { label: string; value: number; note?: string }) {
  return (
    <article className="metric">
      <small>{label}</small>
      <p>
        <strong>{value}</strong>
      </p>
      {note && <small>{note}</small>}
    </article>
  );
}

/** Platform-wide counts for the platform admin (ADR-0097). */
export default function AdminOverview() {
  const stats = useQuery({ queryKey: queryKeys.adminStats, queryFn: getAdminStats });

  return (
    <section>
      <div className="page-heading">
        <h1>Admin</h1>
        <Link to="/admin/tenants" role="button" className="secondary">
          Tenants
        </Link>
      </div>
      {stats.isPending && <Loading label="Loading counts…" />}
      {stats.isError && (
        <ErrorState
          title="Cannot load the counts"
          error={stats.error}
          onRetry={() => void stats.refetch()}
        />
      )}
      {stats.data && (
        <>
          <div className="grid">
            <Card
              label="Tenants"
              value={stats.data.tenants.total}
              note={`${stats.data.tenants.active} active · ${stats.data.tenants.suspended} suspended · ${stats.data.tenants.deleted} deleted`}
            />
            <Card label="Users" value={stats.data.users} />
            <Card label="Monitors" value={stats.data.monitors} />
            <Card label="Watchdogs" value={stats.data.heartbeats} />
          </div>
          <h2>Tenants by plan</h2>
          <table>
            <thead>
              <tr>
                <th scope="col">Plan</th>
                <th scope="col">Tenants</th>
              </tr>
            </thead>
            <tbody>
              {stats.data.plans.map((p) => (
                <tr key={p.key}>
                  <td>
                    {p.name} <small>({p.key})</small>
                  </td>
                  <td>{p.tenants}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p>
            <small>Deleted tenants are counted only under &ldquo;deleted&rdquo;.</small>
          </p>
        </>
      )}
    </section>
  );
}
