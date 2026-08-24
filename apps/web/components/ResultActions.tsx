"use client";

import { useState } from "react";
import { clsx } from "@/lib/clsx";

/* Small client-side toolbar primitives for getting a finished result *out* of
 * the console — copy to clipboard, or download as a file. Kept deliberately
 * tiny so server components (ResultView, job detail) can compose them without
 * turning client. */

const actionBtn =
  "inline-flex items-center gap-1.5 rounded-md border border-hairline-2 bg-panel-2 px-2.5 py-1 " +
  "font-mono text-2xs uppercase tracking-wider text-ink-mid transition-colors " +
  "hover:border-brass/50 hover:text-brass disabled:cursor-not-allowed disabled:opacity-40";

/** Copies `text` to the clipboard, flashing a "Copied" confirmation. */
export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
    }
  };

  return (
    <button type="button" onClick={copy} className={clsx(actionBtn, copied && "border-ok/50 text-ok")}>
      {copied ? "✓ Copied" : label}
    </button>
  );
}

/** Saves `content` to disk as `filename` via a transient object URL. */
export function DownloadButton({
  content,
  filename,
  mime = "text/plain",
  label = "Download",
}: {
  content: string;
  filename: string;
  mime?: string;
  label?: string;
}) {
  const download = () => {
    const blob = new Blob([content], { type: mime });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  return (
    <button type="button" onClick={download} className={actionBtn}>
      ↓ {label}
    </button>
  );
}
