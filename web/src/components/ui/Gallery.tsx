import { useState, type ReactNode } from "react";
import type { LatencyPoint } from "../../api/endpoints";
import {
  Alert,
  Badge,
  Button,
  buttonClass,
  Card,
  Checkbox,
  ConfirmDialog,
  CopyField,
  DiamondShape,
  DotShape,
  EmptyNote,
  EmptyState,
  ErrorState,
  Field,
  Input,
  Loading,
  Meter,
  Metric,
  MetricStrip,
  Pagination,
  PauseShape,
  ReadOnlyValue,
  RibbonPlaceholder,
  RibbonSkeleton,
  Segmented,
  Select,
  SignalRibbon,
  Skeleton,
  StatusIndicator,
  Sweep,
  Table,
  Td,
  Th,
  Toast,
  Tooltip,
  Tr,
  WriteOnlyValue,
} from ".";

const TOKENS = [
  "background",
  "foreground",
  "card",
  "popover",
  "field",
  "muted",
  "muted-foreground",
  "subtle-foreground",
  "border",
  "border-faint",
  "input",
  "input-hover",
  "ring",
  "primary",
  "primary-foreground",
  "primary-hover",
  "primary-active",
  "destructive",
  "destructive-foreground",
  "destructive-hover",
  "destructive-active",
  "down-text",
  "down-tint",
  "down-border",
  "warning",
  "warning-tint",
  "warning-border",
  "success",
  "ribbon",
  "inverse",
  "inverse-foreground",
  "overlay",
];

const TYPE: [string, string, string][] = [
  ["display", "text-display font-semibold tracking-[-0.02em]", "32 · page title"],
  ["headline", "text-headline font-semibold tracking-[-0.02em]", "24 · phone title, figures"],
  ["title", "text-title font-semibold", "18 · dialog title"],
  ["lead", "text-lead font-semibold", "15 · section title"],
  ["body", "text-body", "13.5 · default"],
  ["caption", "text-caption", "12.5 · hints, errors, labels"],
  ["mono", "font-mono text-mono", "11.5 · secondary mono"],
  ["eyebrow", "eyebrow", "10 · mono capitals"],
];

/** A fixed day of 15-minute buckets: normal, one blip, a gap, then an outage at the end. */
function sampleDay(): LatencyPoint[] {
  const start = Date.UTC(2026, 10, 1, 3, 15);
  return Array.from({ length: 97 }, (_, i) => {
    const t = new Date(start + i * 15 * 60_000).toISOString();
    if (i >= 50 && i <= 53) return { t, avgMs: null, okRatio: null };
    if (i >= 95) return { t, avgMs: null, okRatio: 0 };
    const wave = 180 + Math.sin((i / 97) * Math.PI * 2 - 1.2) * 40 + ((i * 37) % 90) - 45;
    if (i === 30) return { t, avgMs: Math.round(wave), okRatio: 0.9 };
    if (i >= 91) return { t, avgMs: 400 + (i - 90) * 450, okRatio: i === 94 ? 0.4 : 0.8 };
    return { t, avgMs: Math.round(wave), okRatio: 1 };
  });
}

function Section({
  number,
  title,
  note,
  children,
}: {
  number: string;
  title: string;
  note?: string;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col gap-4 border-t border-border py-7 first:border-t-0">
      <h2 className="flex flex-wrap items-baseline gap-3">
        <span className="eyebrow">
          {number} · {title}
        </span>
        {note && <span className="font-normal text-subtle-foreground">{note}</span>}
      </h2>
      {children}
    </section>
  );
}

const Note = ({ children }: { children: ReactNode }) => (
  <p className="text-caption text-subtle-foreground">{children}</p>
);

/** Every component in every state, served at /ui-kit.html by the dev server only. */
export default function Gallery() {
  const [theme, setTheme] = useState<"system" | "light" | "dark">("system");
  const [name, setName] = useState("");
  const [range, setRange] = useState<"24h" | "7d" | "30d">("24h");
  const [notify, setNotify] = useState(true);
  const [page, setPage] = useState(1);
  const [dialog, setDialog] = useState(false);
  const [sweep, setSweep] = useState(0);
  const day = sampleDay();

  return (
    <div
      className={
        theme === "system"
          ? "bg-background text-foreground"
          : `theme-${theme} bg-background text-foreground`
      }
    >
      <main className="mx-auto flex max-w-[1040px] flex-col px-4 pt-8 pb-16 sm:px-8">
        <header className="flex flex-wrap items-end justify-between gap-4 pb-4">
          <div className="flex flex-col gap-2">
            <span className="font-mono text-caption font-semibold tracking-[0.18em]">MAIAK</span>
            <h1 className="text-display font-semibold tracking-[-0.02em]">UI kit</h1>
          </div>
          <Segmented
            label="Theme"
            name="theme"
            value={theme}
            onChange={setTheme}
            options={[
              { value: "system", label: "System" },
              { value: "light", label: "Light" },
              { value: "dark", label: "Dark" },
            ]}
          />
        </header>

        <Section number="00" title="Tokens" note="Use by name: bg-muted, text-down, border-input.">
          <div className="grid grid-cols-[repeat(auto-fill,minmax(210px,1fr))] border-t border-l border-border">
            {TOKENS.map((token) => (
              <div
                key={token}
                className="flex items-center gap-2.5 border-r border-b border-border p-2.5"
              >
                <span
                  className="size-6 flex-none border border-border"
                  style={{ background: `var(--${token})` }}
                />
                <span className="font-mono text-mono">--{token}</span>
              </div>
            ))}
          </div>
          <div className="flex flex-col gap-2">
            {TYPE.map(([key, style, note]) => (
              <div key={key} className="flex flex-wrap items-baseline gap-4">
                <span className={style}>Checkout API</span>
                <span className="text-caption text-subtle-foreground">
                  {key} · {note}
                </span>
              </div>
            ))}
          </div>
        </Section>

        <Section number="01" title="Button" note="Hover, press and Tab them.">
          <div className="flex flex-wrap items-center gap-3">
            <Button>Add monitor</Button>
            <Button variant="secondary">Send test message</Button>
            <Button variant="ghost">Cancel</Button>
            <Button variant="destructive">Delete monitor</Button>
            <Button variant="ghost-destructive">Delete</Button>
            <Button variant="secondary" mono>
              15 min
            </Button>
            <Button variant="secondary" size="square" mono>
              2
            </Button>
            <span>
              Inline: <Button variant="link">replace the URL</Button>.
            </span>
            <Button variant="secondary" size="small">
              Small
            </Button>
            <a className={buttonClass("secondary")} href="#link">
              Link as a button
            </a>
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <Button disabled>Save</Button>
            <Button aria-disabled="true">At the plan limit</Button>
            <Button busy>Saving…</Button>
            <Button variant="secondary" busy>
              Retrying…
            </Button>
            <Button variant="destructive" busy>
              Deleting…
            </Button>
          </div>
        </Section>

        <Section number="02" title="Input" note="Label above, hint or error below.">
          <div className="grid gap-x-5 gap-y-6 sm:grid-cols-3">
            <Field label="Name" hint="Live. Hover and type.">
              {(c) => (
                <Input
                  {...c}
                  placeholder="Checkout API"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              )}
            </Field>
            <Field label="URL" hint="HTTPS only.">
              {(c) => (
                <Input {...c} mono start="https://" defaultValue="api.shop-example.com/health" />
              )}
            </Field>
            <Field label="URL" error="Private and internal addresses are not allowed.">
              {(c) => <Input {...c} mono defaultValue="https://10.0.4.12/health" />}
            </Field>
            <Field label="Check every">
              {() => <ReadOnlyValue note="Fixed for now">60 s</ReadOnlyValue>}
            </Field>
            <Field label="Timeout" hint="Disabled.">
              {(c) => <Input {...c} mono disabled defaultValue="10 s" />}
            </Field>
            <Field label="Grace" hint="With a unit.">
              {(c) => <Input {...c} mono inputMode="numeric" end="s" defaultValue="300" />}
            </Field>
            <Field label="Slack webhook URL" hint="Masked as you type.">
              {(c) => (
                <Input {...c} mono secret defaultValue="https://hooks.slack.com/services/x" />
              )}
            </Field>
          </div>
        </Section>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="03" title="Select">
            <Field label="Plan" hint="Native: the platform's list.">
              {(c) => (
                <Select {...c} defaultValue="free">
                  <option value="free">Free</option>
                  <option value="internal">Internal</option>
                </Select>
              )}
            </Field>
          </Section>
          <Section number="04" title="Checkbox">
            <div className="flex flex-col gap-3">
              <Checkbox
                label="Notify Slack"
                description="Posts when it goes down and when it recovers."
                checked={notify}
                onChange={(e) => setNotify(e.target.checked)}
              />
              <Checkbox label="Show deleted tenants" />
              <Checkbox
                label="Notify Slack"
                description="Add a webhook in Settings first."
                disabled
              />
              <Checkbox
                label="Notify Slack"
                description="Posts when it goes down and when it recovers."
                error="No Slack webhook is configured."
              />
            </div>
          </Section>
        </div>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="05" title="Status indicator" note="Shape and word, never colour alone.">
            <div className="flex flex-col items-start gap-2.5">
              <StatusIndicator status="up" />
              <StatusIndicator status="down" label="Down 12 min" />
              <StatusIndicator status="paused" />
              <StatusIndicator status="pending" />
              <StatusIndicator status="down" inline label="1 down" />
            </div>
          </Section>
          <Section number="06" title="Badge">
            <div className="flex flex-wrap items-center gap-2">
              <Badge>HTTP</Badge>
              <Badge tone="strong">Watchdog</Badge>
              <Badge tone="plan">Free plan</Badge>
              <Badge tone="inverse">Admin</Badge>
              <Badge tone="warning" icon={<DiamondShape />}>
                TLS in 9 days
              </Badge>
              <Badge tone="success" icon={<DotShape />}>
                Connected
              </Badge>
              <Badge tone="danger">Suspended</Badge>
              <Badge tone="deleted">Deleted</Badge>
            </div>
          </Section>
        </div>

        <Section
          number="07"
          title="Signal ribbon"
          note="Height = response time · tone = failed checks · break = no checks ran."
        >
          <SignalRibbon
            points={day}
            label="Checkout API, last 24 hours"
            axis={["03:15 yesterday", "09:15", "15:15", "21:15", "Now"]}
          />
          <div className="grid items-center gap-x-4 gap-y-3 sm:grid-cols-[90px_minmax(0,1fr)_90px_minmax(0,1fr)]">
            <span className="eyebrow">Row · 18 px</span>
            <SignalRibbon points={day} heightPx={18} />
            <span className="eyebrow">Gap</span>
            <SignalRibbon points={day.slice(40, 70)} heightPx={18} />
            <span className="eyebrow">Pending</span>
            <RibbonPlaceholder>No checks yet</RibbonPlaceholder>
            <span className="eyebrow">Loading</span>
            <RibbonSkeleton />
          </div>
          <div className="relative overflow-hidden border border-border px-4 py-3">
            <Sweep runKey={sweep} />
            <div className="flex items-center justify-between gap-3">
              <span>The refresh sweep: one slow pass when the data refreshes.</span>
              <Button variant="secondary" size="small" onClick={() => setSweep((n) => n + 1)}>
                Play
              </Button>
            </div>
          </div>
        </Section>

        <Section number="08" title="Metric and meter" note="Ruled strip, never a row of cards.">
          <MetricStrip>
            <Metric label="Uptime 24h" value="98.74%" tone="down" note="below target" />
            <Metric label="Uptime 30d" value="99.93%" note="2 incidents" />
            <Metric label="TLS expires" value="Jan 14" note="in 73 days" />
            <Metric
              label="TLS expires"
              value="Nov 11"
              tone="warning"
              note="in 9 days · renew now"
            />
          </MetricStrip>
          <div className="max-w-[560px] divide-y divide-border-faint">
            <Meter label="Monitors" used={1} limit={3} />
            <Meter label="Heartbeat monitors" used={4} limit={5} />
            <Meter label="Notification channels" used={1} limit={1} />
          </div>
        </Section>

        <Section
          number="09"
          title="Table and pagination"
          note="Rows hover to muted; down rows tint."
        >
          <Table>
            <thead>
              <tr>
                <Th>Status</Th>
                <Th>Monitor</Th>
                <Th numeric>Uptime</Th>
                <Th numeric>Resp</Th>
                <Th numeric>Checked</Th>
              </tr>
            </thead>
            <tbody>
              <Tr tone="down" interactive tabIndex={0}>
                <Td>
                  <StatusIndicator status="down" label="Down 12 min" />
                </Td>
                <Td>
                  <span className="text-[14.5px] font-medium">Checkout API</span>{" "}
                  <span className="font-mono text-mono text-subtle-foreground">
                    api.shop-example.com/health
                  </span>
                </Td>
                <Td numeric className="text-down">
                  98.74%
                </Td>
                <Td numeric className="text-down">
                  10.0 s
                </Td>
                <Td numeric>7 s ago</Td>
              </Tr>
              <Tr interactive tabIndex={0}>
                <Td>
                  <StatusIndicator status="up" />
                </Td>
                <Td>
                  <span className="text-[14.5px] font-medium">Marketing site</span>{" "}
                  <span className="font-mono text-mono text-subtle-foreground">
                    www.example.com
                  </span>
                </Td>
                <Td numeric>100%</Td>
                <Td numeric>318 ms</Td>
                <Td numeric>12 s ago</Td>
              </Tr>
              <Tr interactive tabIndex={0}>
                <Td>
                  <StatusIndicator status="up" />
                </Td>
                <Td>
                  <span className="text-[14.5px] font-medium">Nightly backup</span>{" "}
                  <Badge tone="strong">Watchdog</Badge>
                </Td>
                <Td numeric>100%</Td>
                <Td numeric>—</Td>
                <Td numeric>pinged 5 h ago</Td>
              </Tr>
              <Tr interactive tabIndex={0}>
                <Td>
                  <StatusIndicator status="paused" />
                </Td>
                <Td>
                  <span className="text-[14.5px] font-medium text-muted-foreground">
                    Staging API
                  </span>
                </Td>
                <Td numeric>99.61%</Td>
                <Td numeric>—</Td>
                <Td numeric>—</Td>
              </Tr>
              <Tr interactive tabIndex={0}>
                <Td>
                  <StatusIndicator status="pending" />
                </Td>
                <Td>
                  <span className="text-[14.5px] font-medium">Billing API</span>
                </Td>
                <Td numeric>—</Td>
                <Td numeric>—</Td>
                <Td numeric>—</Td>
              </Tr>
            </tbody>
          </Table>
          <Pagination
            page={page}
            pageCount={2}
            label={page === 1 ? "1–25 of 40" : "26–40 of 40"}
            onPage={setPage}
          />
        </Section>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="11" title="Card" note="A hairline, no shadow at rest.">
            <Card
              title="Slack"
              aside={
                <Badge tone="success" icon={<DotShape />}>
                  Connected
                </Badge>
              }
            >
              <p className="text-muted-foreground">
                Posts when a monitor goes down, recovers, or a TLS certificate is about to expire.
              </p>
              <div className="flex gap-2">
                <Button variant="secondary" size="small">
                  Send test message
                </Button>
                <Button variant="ghost" size="small">
                  Replace URL
                </Button>
              </div>
            </Card>
          </Section>
          <Section number="12" title="Copy field">
            <CopyField
              label="Heartbeat URL"
              value="https://maiak.example/api/v1/heartbeat/3f9a7c21"
            />
            <WriteOnlyValue
              label="Webhook URL"
              hint="Stored encrypted, never shown again."
              onReplace={() => undefined}
            />
          </Section>
        </div>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="13" title="Dialog" note="Focus lands on Cancel. Esc closes.">
            <div>
              <Button variant="destructive" onClick={() => setDialog(true)}>
                Delete monitor
              </Button>
            </div>
            {dialog && (
              <ConfirmDialog
                title="Delete Checkout API?"
                confirmLabel="Delete monitor"
                onConfirm={() => setDialog(false)}
                onCancel={() => setDialog(false)}
              >
                <p>Maiak stops checking it and it disappears from your list.</p>
              </ConfirmDialog>
            )}
          </Section>
          <Section number="14" title="Toast" note="Bottom right. Errors stay until dismissed.">
            <div className="flex flex-col gap-2">
              <Toast onDismiss={() => undefined}>Test message delivered</Toast>
              <Toast kind="error" onDismiss={() => undefined}>
                Could not send the test message
              </Toast>
            </div>
          </Section>
        </div>

        <Section number="—" title="Alert" note="A banner in the page flow.">
          <Alert title="Could not save changes.">One field needs a change.</Alert>
          <Alert tone="warning" title="Copy it now:">
            it is shown only once.
          </Alert>
          <Alert tone="neutral" title="Registration is turned off.">
            Ask the administrator for an account.
          </Alert>
        </Section>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="15" title="Empty state">
            <EmptyState title="Start your first monitor" action={<Button>Add monitor</Button>}>
              Maiak checks a URL every minute, or waits for a heartbeat from your jobs.
            </EmptyState>
          </Section>
          <Section number="16" title="Error state">
            <ErrorState
              title="Could not load monitors"
              detail="The server did not answer. Your monitors are still running."
              meta="HTTP 502 · req_7c1e9a40"
              onRetry={() => undefined}
            />
            <ErrorState
              variant="section"
              title="Recent checks did not load."
              onRetry={() => undefined}
            />
          </Section>
        </div>

        <div className="grid gap-x-8 border-t border-border sm:grid-cols-2 [&>section]:border-t-0">
          <Section number="17" title="Skeleton" note="Static shapes, no shimmer.">
            <Loading label="Loading incidents…" />
            <EmptyNote>No incidents.</EmptyNote>
            <div className="flex flex-col">
              {["60%", "42%", "70%", "50%"].map((width) => (
                <div
                  key={width}
                  className="grid grid-cols-[56px_minmax(0,1fr)_100px_48px] items-center gap-4 border-b border-border-faint py-3"
                >
                  <Skeleton />
                  <span style={{ width }}>
                    <Skeleton />
                  </span>
                  <RibbonSkeleton />
                  <Skeleton />
                </div>
              ))}
            </div>
          </Section>
          <Section number="18" title="Tooltip" note="Inverse surface, after 300 ms.">
            <div className="pt-8">
              <Tooltip content="Your plan allows 3 monitors.">
                <Button aria-disabled="true" size="small">
                  Add monitor
                </Button>
              </Tooltip>
            </div>
            <div className="flex items-center gap-2 text-muted-foreground">
              <PauseShape /> Hover or focus the button.
            </div>
          </Section>
        </div>

        <Section number="—" title="Segmented" note="Native radios: arrow keys work.">
          <div className="flex flex-wrap gap-4">
            <Segmented
              label="Time range"
              name="range"
              value={range}
              onChange={setRange}
              options={[
                { value: "24h", label: "24h" },
                { value: "7d", label: "7d" },
                { value: "30d", label: "30d" },
              ]}
            />
            <Segmented
              label="Density"
              name="small"
              size="small"
              value="a"
              onChange={() => undefined}
              options={[
                { value: "a", label: "Small" },
                { value: "b", label: "Option", disabled: true },
              ]}
            />
          </div>
          <Note>Used for short exclusive choices: time range, expected status, alert after.</Note>
        </Section>
      </main>
    </div>
  );
}
