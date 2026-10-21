import type { ReactNode } from "react";
import { alertmanagerSnippet, secretCommand } from "../lib/heartbeat";
import { useToast } from "./Toasts";

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
  const toast = useToast();

  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      toast("URL copied");
    } catch {
      toast("Copying failed. Select the URL and copy it by hand.", "error");
    }
  }

  return (
    <article aria-labelledby="heartbeat-url-heading">
      <h2 id="heartbeat-url-heading">Your heartbeat URL</h2>
      <p id="heartbeat-url-warning">
        <strong>Copy it now: it is shown only once.</strong> Maiak keeps only a hash of it and
        cannot show it again. If you lose it, open the monitor and choose Regenerate URL; the old
        URL then stops working. Treat it like a password: anybody who has it can keep this monitor
        green.
      </p>
      <label htmlFor="heartbeat-url">Heartbeat URL</label>
      <input
        id="heartbeat-url"
        readOnly
        value={url}
        aria-describedby="heartbeat-url-warning"
        onFocus={(e) => e.currentTarget.select()}
      />
      <button type="button" onClick={() => void copy()}>
        Copy URL
      </button>

      <h3>Alertmanager</h3>
      <p>
        Route the always-firing <code>Watchdog</code> alert to a webhook receiver. Each notification
        is a ping (GET or POST, the body is ignored). Keep the URL in a file that
        <code> url_file</code> points to, for example from a Kubernetes Secret:
      </p>
      <pre aria-label="Alertmanager configuration">
        <code>{alertmanagerSnippet(expectedIntervalSeconds)}</code>
      </pre>
      <pre aria-label="Secret with the URL">
        <code>{secretCommand(url)}</code>
      </pre>
      {children}
    </article>
  );
}
