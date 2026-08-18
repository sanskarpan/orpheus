"use client";

import { useMemo, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { createAnalysisJobAction } from "@/app/actions/jobs";
import { ProcessorParamsForm } from "@/components/ProcessorParamsForm";
import {
  buildParams,
  fieldsForProcessor,
  initValues,
  validateValues,
  type ParamValues,
} from "@/lib/processorParams";

/* "Run analysis" — chains a text/analysis processor onto a completed job's
 * transcript. The new job carries `params.source_job_id = sourceJobId` and
 * reuses the source artifact. */

export interface AnalysisProcessor {
  name: string;
  display_name: string;
  version: string;
  input_schema?: unknown;
}

export function RunAnalysis({
  sourceJobId,
  artifactId,
  processors,
}: {
  sourceJobId: string;
  artifactId: string;
  processors: AnalysisProcessor[];
}) {
  const router = useRouter();
  const [pending, start] = useTransition();
  const [proc, setProc] = useState<string>(processors[0]?.name ?? "");
  const selected = processors.find((p) => p.name === proc) ?? processors[0];
  const [error, setError] = useState<string | null>(null);

  const fields = useMemo(
    () => fieldsForProcessor(selected?.name ?? "", selected?.input_schema),
    [selected?.name, selected?.input_schema],
  );
  const [params, setParams] = useState<ParamValues>(() =>
    initValues(fieldsForProcessor(processors[0]?.name ?? "", processors[0]?.input_schema)),
  );

  const pick = (name: string) => {
    setProc(name);
    const p = processors.find((x) => x.name === name);
    setParams(initValues(fieldsForProcessor(name, p?.input_schema)));
    setError(null);
  };

  const submit = () =>
    start(async () => {
      if (!selected) return;
      setError(null);
      const missing = validateValues(fields, params);
      if (missing.length) {
        setError(`Please fill required: ${missing.join(", ")}.`);
        return;
      }
      const built = buildParams(fields, params);
      const r = await createAnalysisJobAction({
        source_job_id: sourceJobId,
        artifact_id: artifactId,
        processor: { name: selected.name, version: selected.version },
        params: built,
      });
      if (!r.ok) {
        setError(r.error);
        return;
      }
      router.push(`/dashboard/jobs/${r.data.id}`);
    });

  if (processors.length === 0) return null;

  return (
    <div className="panel p-5">
      <div className="label mb-3">Run analysis</div>
      <p className="mb-4 text-xs text-ink-lo">
        Chain a text or audio-analysis processor onto this transcript. It runs on the same source and reads this
        job&apos;s result.
      </p>

      <div className="space-y-4">
        <div>
          <label className="label mb-1.5 block">Processor</label>
          <select value={proc} onChange={(e) => pick(e.target.value)} disabled={pending} className="input">
            {processors.map((p) => (
              <option key={p.name} value={p.name}>
                {p.display_name || p.name}
              </option>
            ))}
          </select>
        </div>

        <ProcessorParamsForm fields={fields} values={params} onChange={setParams} disabled={pending} />

        <button onClick={submit} disabled={pending} className="btn-brass w-full">
          {pending ? "Submitting…" : `Run ${selected?.display_name ?? "analysis"}`}
        </button>
        {error && <div className="text-2xs text-fail">{error}</div>}
      </div>
    </div>
  );
}
