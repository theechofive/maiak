import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { ADMIN_PAGE_SIZE, listAdminTenants, queryKeys } from "../../api/endpoints";
import { ErrorState, Loading } from "../../components/QueryState";
import { formatDate } from "../../lib/format";
import { TenantStatusBadge } from "./TenantStatusBadge";

/** The list of tenants with search and pagination; the state lives in the URL. */
export default function AdminTenants() {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const query = params.get("query") ?? "";
  const offset = Math.max(0, Number.parseInt(params.get("offset") ?? "0", 10) || 0);
  const includeDeleted = params.get("deleted") === "1";
  const [draft, setDraft] = useState(query);

  const tenants = useQuery({
    queryKey: queryKeys.adminTenants(query, offset, includeDeleted),
    queryFn: () => listAdminTenants(query, offset, includeDeleted),
    placeholderData: keepPreviousData,
  });

  function update(next: { query?: string; offset?: number; includeDeleted?: boolean }) {
    const p = new URLSearchParams();
    const q = next.query ?? query;
    const o = next.offset ?? offset;
    const d = next.includeDeleted ?? includeDeleted;
    if (q) p.set("query", q);
    if (o > 0) p.set("offset", String(o));
    if (d) p.set("deleted", "1");
    setParams(p);
  }

  function onSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update({ query: draft.trim(), offset: 0 });
  }

  const data = tenants.data;
  const from = data && data.items.length > 0 ? offset + 1 : 0;
  const to = data ? offset + data.items.length : 0;

  return (
    <section>
      <div className="page-heading">
        <h1>Tenants</h1>
        <Link to="/admin" className="secondary">
          Admin overview
        </Link>
      </div>
      <form role="search" onSubmit={onSearch}>
        <fieldset role="group">
          <input
            type="search"
            name="query"
            aria-label="Search tenants"
            placeholder="Name, slug, tenant id or a member's email"
            maxLength={200}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
          />
          <button type="submit">Search</button>
        </fieldset>
        <label>
          <input
            type="checkbox"
            checked={includeDeleted}
            onChange={(e) => update({ includeDeleted: e.target.checked, offset: 0 })}
          />
          Show deleted tenants
        </label>
      </form>

      {tenants.isPending && <Loading label="Loading tenants…" />}
      {tenants.isError && !data && (
        <ErrorState
          title="Cannot load tenants"
          error={tenants.error}
          onRetry={() => void tenants.refetch()}
        />
      )}
      {data && data.items.length === 0 && (
        <p>{query ? `No tenants match “${query}”.` : "No tenants."}</p>
      )}
      {data && data.items.length > 0 && (
        <>
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Plan</th>
                <th scope="col">Status</th>
                <th scope="col">Users</th>
                <th scope="col">Monitors</th>
                <th scope="col">Watchdogs</th>
                <th scope="col">Created</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((t) => (
                <tr
                  key={t.id}
                  className="clickable"
                  onClick={() => navigate(`/admin/tenants/${t.id}`)}
                >
                  <td>
                    <Link to={`/admin/tenants/${t.id}`} onClick={(e) => e.stopPropagation()}>
                      {t.name}
                    </Link>
                    <br />
                    <small>{t.slug}</small>
                  </td>
                  <td>{t.planName}</td>
                  <td>
                    <TenantStatusBadge tenant={t} />
                  </td>
                  <td>{t.users}</td>
                  <td>{t.monitors}</td>
                  <td>{t.heartbeats}</td>
                  <td>{formatDate(t.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <nav aria-label="Pages" className="actions">
            <small>
              {from}–{to} of {data.total}
            </small>
            <button
              type="button"
              className="secondary"
              disabled={offset === 0}
              onClick={() => update({ offset: Math.max(0, offset - ADMIN_PAGE_SIZE) })}
            >
              Previous
            </button>
            <button
              type="button"
              className="secondary"
              disabled={to >= data.total}
              onClick={() => update({ offset: offset + ADMIN_PAGE_SIZE })}
            >
              Next
            </button>
          </nav>
        </>
      )}
    </section>
  );
}
