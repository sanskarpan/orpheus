"use client";

import { useState } from "react";

/**
 * One-time secret reveal. The API returns cleartext secrets exactly once; this
 * shows it with a copy button and a clear "won't be shown again" warning.
 */
export function CopyOnce({ label, secret }: { label: string; secret: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className="rounded-md border border-brass/40 bg-brass/[0.06] p-4">
      <div className="mb-2 flex items-center gap-2 text-brass">
        <span className="font-mono">⚿</span>
        <span className="text-sm font-semibold">{label}</span>
      </div>
      <div className="flex items-center gap-2">
        <code className="flex-1 overflow-x-auto whitespace-nowrap rounded border border-hairline bg-ground/70 px-3 py-2 font-mono text-xs text-ink-hi">
          {secret}
        </code>
        <button onClick={copy} className="btn shrink-0">
          {copied ? "✓ Copied" : "Copy"}
        </button>
      </div>
      <p className="mt-2 text-2xs text-ink-lo">
        Store this now — it is shown only once and cannot be retrieved again.
      </p>
    </div>
  );
}
