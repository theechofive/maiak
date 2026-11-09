import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { ApiError, errorMessage, errorMeta } from "../../api/client";
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
import TemporaryPasswordPanel from "../../components/TemporaryPasswordPanel";
import { useAuth } from "../../auth/AuthProvider";
import { useToast } from "../../components/Toasts";
import {
  Alert,
  Badge,
  Button,
  buttonClass,
  ConfirmDialog,
  ErrorState,
  Field,
  Metric,
  MetricStrip,
  Select,
  Skeleton,
  Table,
  Td,
  Th,
  Tr,
  Loading,
  EmptyNote,
} from "../../components/ui";
import { formatDate, formatDateTime } from "../../lib/format";
import { TenantStatusBadge } from "./TenantStatusBadge";
import { HIDDEN_PLANS } from "../../lib/usage";

/** The message of a failed admin change: the field error of a 422, otherwise the problem. */
function changeError(err: unknown): string {
  if (err instanceof ApiError && err.status === 422) {
    const fields = err.fieldErrorMap();
    if (fields.planKey) return `Plan: ${fields.planKey}.`;
    if (fields.status) return `Status: ${fields.status}.`;
  }
  return errorMessage(err);
}

const back = (
  <Link
    to="/admin/tenants"
    className="self-start py-2.5 text-[13px] text-muted-foreground hover:text-foreground sm:py-0"
  >
    ← Tenants
  </Link>
);

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

  if (tenant.isPending) {
    return (
      <Loading label="Loading tenant…" className="pt-9">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="w-80 max-w-full" />
      </Loading>
    );
  }
  if (tenant.isError) {
    if (tenant.error instanceof ApiError && tenant.error.status === 404) {
      return (
        <section className="flex flex-col items-start gap-4 py-10">
          <h1 className="text-headline font-semibold tracking-[-0.02em] sm:text-display">
            Tenant not found
          </h1>
          <Link to="/admin/tenants" className={buttonClass("secondary")}>
            Back to tenants
          </Link>
        </section>
      );
    }
    return (
      <div className="pt-9">
        <ErrorState
          title="Cannot load the tenant"
          detail={errorMessage(tenant.error)}
          meta={errorMeta(tenant.error)}
          onRetry={() => void tenant.refetch()}
        />
      </div>
    );
  }
  return (
    <section className="flex flex-col gap-8 pt-4">
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
  const [planError, setPlanError] = useState<string | undefined>();
  const [confirm, setConfirm] = useState<"suspend" | "unsuspend" | "delete" | null>(null);
  const auth = useAuth();
  const deleted = tenant.deletedAt !== null;
  const suspended = tenant.status === "suspended";
  // ADR-0097 guard rails: the backend refuses these with 409 protected-tenant.
  const protectedReason =
    tenant.slug === "default"
      ? "The platform's own tenant: it cannot be suspended or deleted."
      : tenant.id === auth.session?.tenant.id
        ? "Your own tenant: you cannot suspend or delete it."
        : undefined;

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
    setPlanError(undefined);
    update.mutate({ planKey: plan });
  }

  // `community` stays out of the picker until self-hosted packaging (L-312), unless the tenant
  // is already on it.
  const planOptions = (
    plans.data?.plans ?? [{ key: tenant.planKey, name: tenant.planName, tenants: 0 }]
  ).filter((p) => !HIDDEN_PLANS.includes(p.key) || p.key === tenant.planKey);

  return (
    <>
      <div className="flex flex-col gap-3">
        {back}
        <div className="flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
          <div className="flex min-w-0 flex-col gap-2">
            <h1 className="text-headline leading-[1.1] font-semibold tracking-[-0.02em] break-words sm:text-display">
              {tenant.name}
            </h1>
            <div className="flex flex-wrap items-center gap-3">
              <TenantStatusBadge tenant={tenant} />
              <span className="font-mono text-[13px] text-muted-foreground">{tenant.slug}</span>
            </div>
          </div>
          {!deleted && protectedReason && (
            <p className="max-w-[280px] text-caption text-muted-foreground">{protectedReason}</p>
          )}
          {!deleted && !protectedReason && (
            <div className="flex flex-wrap gap-2">
              <Button
                variant="secondary"
                onClick={() => setConfirm(suspended ? "unsuspend" : "suspend")}
              >
                {suspended ? "Unsuspend" : "Suspend"}
              </Button>
              <Button variant="ghost-destructive" onClick={() => setConfirm("delete")}>
                Delete
              </Button>
            </div>
          )}
        </div>
      </div>

      <MetricStrip>
        <Metric label="Users" value={tenant.users} />
        <Metric label="Monitors" value={tenant.monitors} />
        <Metric label="Watchdogs" value={tenant.heartbeats} />
        <Metric
          label="Created"
          value={formatDate(tenant.createdAt)}
          note={tenant.deletedAt ? `Deleted ${formatDateTime(tenant.deletedAt)}` : undefined}
        />
      </MetricStrip>

      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-[140px_minmax(0,1fr)]">
        <dt className="text-caption text-muted-foreground">Tenant id</dt>
        <dd className="font-mono text-[13px] break-all">{tenant.id}</dd>
      </dl>

      {deleted ? (
        <Alert tone="neutral" title={`Plan: ${tenant.planName}.`}>
          A deleted tenant cannot be changed; its data is kept (see the admin runbook to restore
          it).
        </Alert>
      ) : (
        <form onSubmit={onSavePlan} className="flex max-w-[560px] flex-col gap-2">
          <Field
            label="Plan"
            hint="Existing monitors are kept when the new plan allows fewer; only new ones are refused."
            error={planError}
          >
            {(c) => (
              <div className="flex gap-2">
                <Select
                  {...c}
                  className="flex-1"
                  value={plan}
                  onChange={(e) => {
                    setPlan(e.target.value);
                    setPlanError(undefined);
                  }}
                >
                  {planOptions.map((p) => (
                    <option key={p.key} value={p.key}>
                      {p.name}
                    </option>
                  ))}
                </Select>
                <Button
                  type="submit"
                  variant="secondary"
                  busy={update.isPending && plan !== tenant.planKey}
                  disabled={update.isPending || plan === tenant.planKey}
                >
                  Save
                </Button>
              </div>
            )}
          </Field>
        </form>
      )}

      {confirm === "suspend" && (
        <ConfirmDialog
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
          title={`Unsuspend ${tenant.name}?`}
          confirmLabel="Unsuspend tenant"
          confirmVariant="primary"
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
          title={`Delete ${tenant.name}?`}
          confirmLabel="Delete tenant"
          busy={remove.isPending}
          onCancel={() => setConfirm(null)}
          onConfirm={() => remove.mutate()}
        >
          <p>
            The tenant is marked deleted: its {tenant.users} users cannot sign in, its{" "}
            {tenant.monitors} monitors and {tenant.heartbeats} watchdogs are not checked and it
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
    <section aria-labelledby="users-heading" className="flex flex-col gap-3">
      <h2 id="users-heading" className="text-lead font-semibold">
        Users
      </h2>
      {temporary && (
        <TemporaryPasswordPanel login={temporary.login} password={temporary.password}>
          <Button
            variant="secondary"
            onClick={() => {
              setTemporary(null);
              reset.reset();
            }}
          >
            I handed it over, hide it
          </Button>
        </TemporaryPasswordPanel>
      )}
      {users.isPending && (
        <Loading label="Loading users…">
          <Skeleton className="w-1/2" />
        </Loading>
      )}
      {users.isError && (
        <ErrorState
          variant="section"
          title="Cannot load the users"
          onRetry={() => void users.refetch()}
        />
      )}
      {users.data && users.data.items.length === 0 && (
        <EmptyNote>This tenant has no users.</EmptyNote>
      )}
      {users.data && users.data.items.length > 0 && (
        <Table>
          <thead>
            <tr>
              <Th>Login</Th>
              <Th>Role</Th>
              <Th>Status</Th>
              <Th numeric>Created</Th>
              <Th numeric>
                <span className="sr-only">Actions</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {users.data.items.map((u) => (
              <Tr key={u.id}>
                <Td>
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="font-mono text-[13px] break-all">{u.login}</span>
                    {u.isPlatformAdmin && <Badge tone="inverse">System admin</Badge>}
                  </span>
                </Td>
                <Td>{u.role}</Td>
                <Td className={u.status === "disabled" ? "text-subtle-foreground" : undefined}>
                  {u.status}
                </Td>
                <Td numeric>{formatDate(u.createdAt)}</Td>
                <Td numeric>
                  <Button
                    variant="secondary"
                    size="small"
                    disabled={u.isPlatformAdmin}
                    aria-describedby={u.isPlatformAdmin ? `system-admin-note-${u.id}` : undefined}
                    onClick={() => setConfirmUser(u)}
                  >
                    Reset password
                  </Button>
                  {u.isPlatformAdmin && (
                    <span
                      id={`system-admin-note-${u.id}`}
                      className="mt-1 block font-sans text-caption text-muted-foreground"
                    >
                      Managed by the maiak-admin Secret, not here.
                    </span>
                  )}
                </Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
      {users.data && users.data.total > users.data.items.length && (
        <p className="text-caption text-muted-foreground">
          Showing the first {users.data.items.length} of {users.data.total} users.
        </p>
      )}
      {confirmUser && (
        <ConfirmDialog
          title={`Reset the password of ${confirmUser.login}?`}
          confirmLabel="Reset password"
          confirmVariant="primary"
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
    </section>
  );
}
