"use client";

import type { ParamField, ParamValue, ParamValues } from "@/lib/processorParams";
import { clsx } from "@/lib/clsx";

/* Controlled parameter form for a processor. The parent owns `values` and the
 * field spec (from lib/processorParams.fieldsForProcessor); this just renders
 * inputs in the warm-paper style and reports changes. */

export function ProcessorParamsForm({
  fields,
  values,
  onChange,
  disabled,
}: {
  fields: ParamField[];
  values: ParamValues;
  onChange: (next: ParamValues) => void;
  disabled?: boolean;
}) {
  if (fields.length === 0) {
    return <p className="text-xs text-ink-lo">No parameters — this processor runs on the input as-is.</p>;
  }

  const set = (name: string, v: ParamValue) => onChange({ ...values, [name]: v });

  return (
    <div className="space-y-3.5">
      {fields.map((f) => (
        <Field key={f.name} field={f} value={values[f.name]} onChange={(v) => set(f.name, v)} disabled={disabled} />
      ))}
    </div>
  );
}

function Field({
  field: f,
  value,
  onChange,
  disabled,
}: {
  field: ParamField;
  value: ParamValue;
  onChange: (v: ParamValue) => void;
  disabled?: boolean;
}) {
  if (f.type === "bool") {
    return (
      <label className="flex cursor-pointer items-start gap-2.5">
        <input
          type="checkbox"
          checked={value === true}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
          className="mt-0.5 h-4 w-4 accent-brass"
        />
        <span>
          <span className="text-sm text-ink-hi">
            {f.label}
            {(f.required || f.mustBeTrue) && <span className="ml-1 text-brass">*</span>}
          </span>
          {f.help && <span className="mt-0.5 block text-2xs text-ink-lo">{f.help}</span>}
        </span>
      </label>
    );
  }

  return (
    <div>
      <label className="label mb-1.5 flex items-center gap-1.5">
        {f.label}
        {f.required && <span className="text-brass">*</span>}
      </label>

      {f.type === "select" ? (
        <select
          value={typeof value === "string" ? value : ""}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
          className="input"
        >
          {f.options?.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : f.type === "multiselect" ? (
        <div className="flex flex-wrap gap-2">
          {f.options?.map((o) => {
            const arr = Array.isArray(value) ? value : [];
            const on = arr.includes(o.value);
            return (
              <button
                key={o.value}
                type="button"
                disabled={disabled}
                onClick={() => onChange(on ? arr.filter((x) => x !== o.value) : [...arr, o.value])}
                className={clsx(
                  "rounded-md border px-2 py-1 font-mono text-2xs transition-colors disabled:opacity-50",
                  on ? "border-brass/50 bg-brass/10 text-brass" : "border-hairline-2 text-ink-mid hover:text-ink-hi",
                )}
              >
                {o.label}
              </button>
            );
          })}
        </div>
      ) : (
        <input
          type={f.type === "number" ? "number" : "text"}
          value={value === undefined ? "" : String(value)}
          placeholder={f.placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
          className="input"
        />
      )}

      {f.help && <p className="mt-1 text-2xs text-ink-lo">{f.help}</p>}
    </div>
  );
}
