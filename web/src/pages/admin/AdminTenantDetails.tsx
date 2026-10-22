import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { ApiError, errorMessage } from "../../api/client";
import {
  deleteAdminTenant,
  getAdminStats,
  getAdminTenant,
  listAdminTenantUsers,
  queryKeys,
  resetAdminUserPassword,
  updateAdminTenant,
  type AdminTenant,
  type AdminTenantUpdate,
  type AdminUser,
} from "../../api/endpoints";
import ConfirmDialog from "../../components/ConfirmDialog";
import { ErrorState, Loading } from "../../components/QueryState";
import TemporaryPasswordPanel from "../../components/TemporaryPasswordPanel";
import { useToast } from "../../components/Toasts";
import { formatDate, formatDateTime } from "../../lib/format";
import { TenantStatusBadge } from "./TenantStatusBadge";

/** The message of a failed admin change: the field error of a 422, otherwise the problem. */
function changeError(err: unknown): string {
  if (err instanceof ApiError && err.status === 422) {
    const fields = err.fieldErrorMap();
    if (fields.planKey) return `Plan: ${fields.planKey}.`;
    if (fields.status) return `Status: ${fields.status}.`;
  }
  return errorMessage(err);
}

export default function AdminTenantDetails() {
  const { id = "" } = useParams();
  // Keyed by tenant: moving to another tenant starts with fresh state, so a temporary password
  // shown for one tenant never stays on the screen of the next.
  return <TenantDetails key={id} id={id} />;
}

function TenantDetails({ id }: { id: string }) {
  const tenant = useQuery({
    queryKey: queryKeys.adminTenant(id),
    queryFn: () => getAdminTenant(id),
  });

  if (tenant.isPending) return <Loading label="Loading tenant…" />;
  if (tenant.isError) {
    if (tenant.error instanceof ApiError && tenant.error.status === 404) {
      return (
        <section>
          <h1>Tenant not found</h1>
          <p>
            <Link to="/admin/tenants">Back to tenants</Link>
          </p>
        </section>
      );
    }
    return (
      <ErrorState
        title="Cannot load the tenant"
        error={tenant.error}
        onRetry={() => void tenant.refetch()}
      />
    );
  }
  return (
    <section>
      <p>
        <Link to="/admin/tenants">← Tenants</Link>
      </p>
      <TenantInfo tenant={tenant.data} />
      <TenantUsers tenant={tenant.data} />
    </section>
  );
}

function TenantInfo({ tenant }: { tenant: AdminTenant }) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const plans = useQuery({ queryKey: queryKeys.adminStats, queryFn: getAdminStats });
  const [plan, setPlan] = useState(tenant.planKey);
  const [planError, setPlanError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<"suspend" | "unsuspend" | "delete" | null>(null);
  const deleted = tenant.deletedAt !== null;
  const suspended = tenant.status === "suspended";

  function refresh(updated?: AdminTenant) {
    if (updated) queryClient.setQueryData(queryKeys.adminTenant(tenant.id), updated);
    else void queryClient.invalidateQueries({ queryKey: queryKeys.adminTenant(tenant.id) });
    void queryClient.invalidateQueries({ queryKey: ["admin", "tenants"] });
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminStats });
  }

  const update = useMutation({
    mutationFn: (body: AdminTenantUpdate) => updateAdminTenant(tenant.id, body),
    onSuccess: (updated, body) => {
      setConfirm(null);
      refresh(updated);
      if (body.planKey) toast(`Plan changed to ${updated.planName}`);
      if (body.status)
        toast(updated.status === "suspended" ? "Tenant suspended" : "Tenant unsuspended");
    },
    onError: (err, body) => {
      setConfirm(null);
      if (body.planKey) setPlanError(changeError(err));
      else toast(changeError(err), "error");
    },
  });

  const remove = useMutation({
    mutationFn: () => deleteAdminTenant(tenant.id),
    onSuccess: () => {
      setConfirm(null);
      refresh();
      toast("Tenant deleted");
    },
    onError: (err) => {
      setConfirm(null);
      toast(changeError(err), "error");
    },
  });

  function onSavePlan(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPlanError(null);
    update.mutate({ planKey: plan });
  }

  const planOptions = plans.data?.plans ?? [
    { key: tenant.planKey, name: tenant.planName, tenants: 0 },
  ];

  return (
    <>
      <div className="page-heading">
        <hgroup>
          <h1>{tenant.name}</h1>
          <p>
            {tenant.slug} · <TenantStatusBadge tenant={tenant} />
          </p>
        </hgroup>
        {!deleted && (
          <div className="actions">
            <button
              type="button"
              className="secondary"
              onClick={() => setConfirm(suspended ? "unsuspend" : "suspend")}
            >
              {suspended ? "Unsuspend" : "Suspend"}
            </button>
            <button
              type="button"
              className="secondary outline"
              onClick={() => setConfirm("delete")}
            >
              Delete
            </button>
          </div>
        )}
      </div>
      <dl>
        <dt>Tenant id</dt>
        <dd>
          <code>{tenant.id}</code>
        </dd>
        <dt>Created</dt>
        <dd>{formatDate(tenant.createdAt)}</dd>
        {tenant.deletedAt && (
          <>
            <dt>Deleted</dt>
            <dd>{formatDateTime(tenant.deletedAt)}</dd>
          </>
        )}
        <dt>Usage</dt>
        <dd>
          {tenant.users} users · {tenant.monitors} monitors · {tenant.heartbeats} watchdogs
        </dd>
      </dl>

      {deleted ? (
        <p>
          Plan: {tenant.planName}. A deleted tenant cannot be changed; its data is kept (see the
          admin runbook to restore it).
        </p>
      ) : (
        <form onSubmit={onSavePlan}>
          <label htmlFor="tenant-plan">Plan</label>
          <fieldset role="group">
            <select
              id="tenant-plan"
              value={plan}
              aria-invalid={planError ? true : undefined}
              aria-describedby="tenant-plan-help"
              onChange={(e) => {
                setPlan(e.target.value);
                setPlanError(null);
              }}
            >
              {planOptions.map((p) => (
                <option key={p.key} value={p.key}>
                  {p.name}
                </option>
              ))}
            </select>
            <button
              type="submit"
              aria-busy={update.isPending && plan !== tenant.planKey}
              disabled={update.isPending || plan === tenant.planKey}
            >
              Save
            </button>
          </fieldset>
          <small id="tenant-plan-help" className={planError ? "error" : undefined}>
            {planError ??
              "Existing monitors are kept when the new plan allows fewer; only new ones are refused."}
          </small>
        </form>
      )}

      {confirm === "suspend" && (
        <ConfirmDialog
          id="suspend"
          title={`Suspend ${tenant.name}?`}
          confirmLabel="Suspend tenant"
          busy={update.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => update.mutate({ status: "suspended" })}
        >
          <p>
            Its users cannot sign in and their sessions end. Its monitors are not checked, its
            watchdogs are not evaluated and its Slack alerts wait. No data is deleted; you can
            unsuspend it later.
          </p>
        </ConfirmDialog>
      )}
      {confirm === "unsuspend" && (
        <ConfirmDialog
          id="unsuspend"
          title={`Unsuspend ${tenant.name}?`}
          confirmLabel="Unsuspend tenant"
          busy={update.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => update.mutate({ status: "active" })}
        >
          <p>
            Its users can sign in again and its monitors are checked again within a minute. Slack
            alerts that waited up to 15 minutes are sent now, older ones are dropped, and watchdogs
            that missed their deadline meanwhile go down until their next ping.
          </p>
        </ConfirmDialog>
      )}
      {confirm === "delete" && (
        <ConfirmDialog
          id="delete"
          title={`Delete ${tenant.name}?`}
          confirmLabel="Delete tenant"
          busy={remove.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => remove.mutate()}
        >
          <p>
            The tenant is marked deleted: its users cannot sign in, nothing of it is checked and it
            leaves the tenant list. Its data is kept, but the panel cannot undo this.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

function TenantUsers({ tenant }: { tenant: AdminTenant }) {
  const toast = useToast();
  const users = useQuery({
    queryKey: queryKeys.adminUsers(tenant.id),
    queryFn: () => listAdminTenantUsers(tenant.id),
  });
  const [confirmUser, setConfirmUser] = useState<AdminUser | null>(null);
  // The temporary password lives only here: never in storage, gone when hidden or the page is
  // left. The mutation keeps no copy: gcTime 0 and reset() right after copying it (ADR-0098).
  const [temporary, setTemporary] = useState<{ login: string; password: string } | null>(null);

  const reset = useMutation({
    mutationFn: (user: AdminUser) => resetAdminUserPassword(user.id),
    gcTime: 0,
    onSuccess: (res, user) => {
      setConfirmUser(null);
      setTemporary({ login: user.login, password: res.temporaryPassword });
      reset.reset();
    },
    onError: (err) => {
      setConfirmUser(null);
      toast(errorMessage(err), "error");
    },
  });

  return (
    <>
      <h2>Users</h2>
      {temporary && (
        <TemporaryPasswordPanel login={temporary.login} password={temporary.password}>
          <button
            type="button"
            className="secondary"
            onClick={() => {
              setTemporary(null);
              reset.reset();
            }}
          >
            I handed it over, hide it
          </button>
        </TemporaryPasswordPanel>
      )}
      {users.isPending && <Loading label="Loading users…" />}
      {users.isError && (
        <ErrorState
          title="Cannot load the users"
          error={users.error}
          onRetry={() => void users.refetch()}
        />
      )}
      {users.data && users.data.items.length === 0 && <p>This tenant has no users.</p>}
      {users.data && users.data.items.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Login</th>
              <th scope="col">Role</th>
              <th scope="col">Status</th>
              <th scope="col">Created</th>
              <th scope="col">
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {users.data.items.map((u) => (
              <tr key={u.id}>
                <td>
                  {u.login}
                  {u.isPlatformAdmin && <span className="badge">system admin</span>}
                </td>
                <td>{u.role}</td>
                <td>{u.status}</td>
                <td>{formatDate(u.createdAt)}</td>
                <td>
                  <button
                    type="button"
                    className="secondary outline"
                    disabled={u.isPlatformAdmin}
                    aria-describedby={u.isPlatformAdmin ? `system-admin-note-${u.id}` : undefined}
                    onClick={() => setConfirmUser(u)}
                  >
                    Reset password
                  </button>
                  {u.isPlatformAdmin && (
                    <small id={`system-admin-note-${u.id}`}>
                      <br />
                      Managed by the maiak-admin Secret, not here.
                    </small>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {users.data && users.data.total > users.data.items.length && (
        <p>
          <small>
            Showing the first {users.data.items.length} of {users.data.total} users.
          </small>
        </p>
      )}
      {confirmUser && (
        <ConfirmDialog
          id="reset"
          title={`Reset the password of ${confirmUser.login}?`}
          confirmLabel="Reset password"
          busy={reset.isPending}
          onCancel={() => setConfirmUser(null)}
          onConfirm={() => reset.mutate(confirmUser)}
        >
          <p>
            The current password stops working and all of the user&apos;s sessions end. You get a
            temporary password once, to hand over yourself. Verify who asked for it first.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}
