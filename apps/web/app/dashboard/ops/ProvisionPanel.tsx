"use client";

import { useState, useTransition } from "react";
import { provisionAction } from "@/app/actions/ops";
import { CopyOnce } from "@/components/CopyOnce";
import type { ProvisionResponse } from "@/lib/orpheus";

export function ProvisionPanel() {
  const [org, setOrg] = useState("");
  const [email, setEmail] = useState("");
  const [keyName, setKeyName] = useState("");
  const [result, setResult] = useState<ProvisionResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const submit = () =>
    start(async () => {
      setError(null);
      setResult(null);
      if (!org.trim()) return setError("Org name is required.");
      if (!/^[^@]+@[^@]+\.[^@]+$/.test(email)) return setError("Enter a valid email.");
      const r = await provisionAction({ org_name: org.trim(), user_email: email.trim(), key_name: keyName || undefined });
      if (!r.ok) return setError(r.error);
      setResult(r.data);
      setOrg("");
      setEmail("");
      setKeyName("");
    });

  return (
    <div className="panel p-5">
      <div className="label mb-4">Provision tenant</div>
      <div className="space-y-3">
        <div>
          <label className="label mb-1.5 block">Org name</label>
          <input value={org} onChange={(e) => setOrg(e.target.value)} placeholder="Acme Audio" className="input" />
        </div>
        <div>
          <label className="label mb-1.5 block">Admin email</label>
          <input value={email} onChange={(e) => setEmail(e.target.value)} placeholder="ops@acme.com" className="input" />
        </div>
        <div>
          <label className="label mb-1.5 block">Key name (optional)</label>
          <input value={keyName} onChange={(e) => setKeyName(e.target.value)} placeholder="default" className="input" />
        </div>
        <button onClick={submit} disabled={pending} className="btn-brass w-full">
          {pending ? "Provisioning…" : "Provision"}
        </button>
        {error && <div className="text-2xs text-fail">{error}</div>}
        {result && (
          <div className="space-y-2 pt-1">
            <div className="font-mono text-2xs text-ink-lo">
              org {result.org_id.slice(0, 8)} · user {result.user_id.slice(0, 8)}
            </div>
            <CopyOnce label="New tenant API key" secret={result.api_key} />
          </div>
        )}
      </div>
    </div>
  );
}
