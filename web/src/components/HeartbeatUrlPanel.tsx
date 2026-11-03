import type { ReactNode } from "react";
import { alertmanagerSnippet, secretCommand } from "../lib/heartbeat";
import { CopyField } from "./ui";

const codeBlock =
  "m-0 overflow-x-auto border border-border-faint bg-muted px-4 py-3.5 font-mono text-mono leading-[1.65] whitespace-pre";

/**
 * The one-time heartbeat URL after creating a heartbeat or regenerating its URL, with a copy
 * button and the Alertmanager configuration that uses it. The server keeps only a hash, so this
 * is the only time the URL can be shown.
 */
export default function HeartbeatUrlPanel({
  url,
  expectedIntervalSeconds,
  children,
}: {
  url: string;
  expectedIntervalSeconds: number;
  children?: ReactNode;
}) {
  return (
    <section
      aria-labelledby="heartbeat-url-heading"
      className="flex flex-col gap-5 border border-border p-5"
    >
      <h2 id="heartbeat-url-heading" className="text-lead font-semibold">
        Your heartbeat URL
      </h2>
      <CopyField label="Heartbeat URL" value={url} copyLabel="Copy URL" />
      <p className="text-muted-foreground">
        Maiak keeps only a hash of it and cannot show it again. If you lose it, open the monitor and
        choose Regenerate URL; the old URL then stops working. Treat it like a password: anybody who
        has it can keep this monitor green.
      </p>

      <div className="flex flex-col gap-2.5">
        <h3 className="text-[14px] font-semibold">Alertmanager</h3>
        <p className="text-muted-foreground">
          Route the always-firing <code className="font-mono text-mono">Watchdog</code> alert to a
          webhook receiver. Each notification is a ping (GET or POST, the body is ignored). Keep the
          URL in a file that <code className="font-mono text-mono">url_file</code> points to, for
          example from a Kubernetes Secret:
        </p>
        <pre aria-label="Alertmanager configuration" className={codeBlock}>
          <code>{alertmanagerSnippet(expectedIntervalSeconds)}</code>
        </pre>
        <p id="heartbeat-secret-hint" className="text-muted-foreground">
          Run this with your cluster&apos;s context, paste the URL, then press Ctrl-D, so the URL
          stays out of your shell history:
        </p>
        <pre
          aria-label="Secret with the URL"
          aria-describedby="heartbeat-secret-hint"
          className={codeBlock}
        >
          <code>{secretCommand()}</code>
        </pre>
      </div>
      {children && <div className="flex flex-wrap gap-2">{children}</div>}
    </section>
  );
}
