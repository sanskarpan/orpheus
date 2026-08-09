"use client";

import { useState, useTransition } from "react";
import { createKeyAction, revokeKeyAction } from "@/app/actions/keys";
import { CopyOnce } from "@/components/CopyOnce";
import type { APIKey } from "@/lib/orpheus";
import { absTime } from "@/lib/format";
import { clsx } from "@/lib/clsx";

const SCOPE_PRESETS = [
  "jobs:write",
  "jobs:read",
  "uploads:write",
  "uploads:read",
  "artifacts:read",
  "webhooks:write",
  "webhooks:read",
  "usage:read",
  "*",
];

export function KeysManager({ initial, ownerPrefix }: { initial: APIKey[]; ownerPrefix: string }) {
  const [keys, setKeys] = useState<APIKey[]>(initial);
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<string[]>(["jobs:write", "jobs:read", "uploads:write", "artifacts:read"]);
  const [minted, setMinted] = useState<APIKey | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const toggle = (s: string) =>
    setScopes((cur) => (cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]));

  const submit = () =>
    start(async () => {
      setError(null);
      setMinted(null);
      if (!name.trim()) return setError("Give the key a name.");
      if (scopes.length === 0) return setError("Select at least one scope.");
      const r = await createKeyAction({ name: name.trim(), scopes });
      if (!r.ok) return setError(r.error);
      setMinted(r.data);
      setKeys((cur) => [{ ...r.data, secret: undefined }, ...cur]);
      setName("");
    });

  const revoke = (id: string) =>
    start(async () => {
      const r = await revokeKeyAction(id);
      if (r.ok) setKeys((cur) => cur.filter((k) => k.id !== id));
      else setError(r.error);
    });

  return (
    <div className="grid gap-6 lg:grid-cols-5">
      {/* Create */}
      <div className="lg:col-span-2">
        <div className="panel p-5">
          <div className="label mb-4">Create key</div>
          <div className="space-y-4">
            <p className="text-xs text-ink-lo">
              Generate a scoped key for your own API integrations or CI. It's separate from the key
              the dashboard uses.
            </p>
            <div>
              <label className="label mb-1.5 block">Name</label>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="ci-pipeline"
                className="input"
              />
            </div>
            <div>
              <label className="label mb-1.5 block">Scopes</label>
              <div className="flex flex-wrap gap-2">
                {SCOPE_PRESETS.map((s) => (
                  <button
                    key={s}
                    type="button"
                    onClick={() => toggle(s)}
                    className={clsx(
                      "rounded-md border px-2 py-1 font-mono text-2xs transition-colors",
                      scopes.includes(s)
                        ? "border-brass/50 bg-brass/10 text-brass"
                        : "border-hairline-2 text-ink-mid hover:text-ink-hi",
                    )}
                  >
                    {s}
                  </button>
                ))}
              </div>
            </div>
            <button onClick={submit} disabled={pending} className="btn-brass w-full">
              {pending ? "Creating…" : "Create key"}
            </button>
            {error && <div className="text-2xs text-fail">{error}</div>}
            {minted?.secret && <CopyOnce label={`Secret for “${minted.name}”`} secret={minted.secret} />}
          </div>
        </div>
      </div>

      {/* List */}
      <div className="lg:col-span-3">
        <div className="panel overflow-hidden">
          <div className="hairline-b px-5 py-3">
            <span className="label">Active keys · {keys.length}</span>
          </div>
          {keys.length === 0 ? (
            <div className="px-5 py-12 text-center text-sm text-ink-mid">No active keys.</div>
          ) : (
            <table className="w-full text-sm">
              <tbody>
                {keys.map((k) => {
                  const isOwner = !!ownerPrefix && k.prefix === ownerPrefix;
                  return (
                    <tr key={k.id} className="border-b border-hairline/60 last:border-0">
                      <td className="px-5 py-3">
                        <div className="flex items-center gap-2">
                          <span className="font-medium text-ink-hi">{k.name}</span>
                          {isOwner && (
                            <span className="rounded border border-brass/40 px-1.5 py-0.5 text-[9px] font-mono uppercase tracking-wider text-brass">
                              dashboard
                            </span>
                          )}
                        </div>
                        <div className="mt-0.5 font-mono text-2xs text-ink-lo">{k.prefix}…</div>
                      </td>
                      <td className="px-3 py-3">
                        <div className="flex flex-wrap gap-1">
                          {k.scopes.slice(0, 4).map((s) => (
                            <span key={s} className="rounded border border-hairline-2 px-1 py-0.5 font-mono text-2xs text-ink-mid">
                              {s}
                            </span>
                          ))}
                          {k.scopes.length > 4 && (
                            <span className="font-mono text-2xs text-ink-lo">+{k.scopes.length - 4}</span>
                          )}
                        </div>
                      </td>
                      <td className="tnum px-3 py-3 text-2xs text-ink-lo">{absTime(k.created_at)}</td>
                      <td className="px-5 py-3 text-right">
                        {isOwner ? (
                          <span className="text-2xs font-mono uppercase text-ink-lo" title="Used by the dashboard — can't be revoked here">
                            managed
                          </span>
                        ) : (
                          <button
                            onClick={() => revoke(k.id)}
                            disabled={pending}
                            className="text-2xs font-mono uppercase text-ink-lo transition-colors hover:text-fail"
                          >
                            revoke
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}
