/* ================================================================== *
 * Per-processor job-parameter specs.
 *
 * The API's `POST /v1/jobs` takes an opaque `params` object per processor.
 * `GET /v1/processors/{name}` may return a JSON-Schema `input_schema` — when it
 * does (and it's a non-empty object schema) we drive the form from it. When the
 * schema is empty (`{}`) we fall back to the CURATED map below so the 26
 * processors are all runnable from the console.
 *
 * Pure module (no React / no server-only) so it can be shared by the upload
 * flow, the job-chaining "Run analysis" panel, and validation helpers.
 * ================================================================== */

export type ParamFieldType = "text" | "number" | "bool" | "select" | "multiselect";

export type ParamValue = string | number | boolean | string[] | undefined;
export type ParamValues = Record<string, ParamValue>;

export interface ParamOption {
  value: string;
  label: string;
}

export interface ParamField {
  name: string;
  label: string;
  type: ParamFieldType;
  required?: boolean;
  /** A checkbox that MUST be checked to submit (e.g. biometric consent). */
  mustBeTrue?: boolean;
  default?: ParamValue;
  placeholder?: string;
  help?: string;
  options?: ParamOption[];
}

/* ---- curated fallback specs, keyed by exact processor name ---- */

const ENTITY_OPTIONS: ParamOption[] = [
  { value: "EMAIL", label: "Email" },
  { value: "PHONE", label: "Phone" },
  { value: "SSN", label: "SSN" },
  { value: "CREDIT_CARD", label: "Credit card" },
  { value: "IP", label: "IP" },
  { value: "PERSON", label: "Person" },
  { value: "ADDRESS", label: "Address" },
];

const REDACT_FIELDS: ParamField[] = [
  {
    name: "entities",
    label: "Entities",
    type: "multiselect",
    options: ENTITY_OPTIONS,
    help: "PII entity types to redact.",
  },
  {
    name: "mask",
    label: "Mask",
    type: "select",
    options: [
      { value: "type", label: "type (label)" },
      { value: "char", label: "char (•••)" },
      { value: "hash", label: "hash" },
    ],
  },
];

export const CURATED_PARAMS: Record<string, ParamField[]> = {
  transcribe: [
    { name: "model", label: "Model", type: "text", default: "tiny.en", placeholder: "tiny.en" },
    { name: "language", label: "Language", type: "text", placeholder: "blank = auto-detect" },
    { name: "word_timestamps", label: "Word timestamps", type: "bool" },
    {
      name: "chunking",
      label: "Chunking",
      type: "select",
      options: [
        { value: "vad", label: "vad" },
        { value: "fixed", label: "fixed" },
      ],
    },
    { name: "multilang", label: "Multilingual", type: "bool" },
    {
      name: "alignment",
      label: "Alignment",
      type: "select",
      options: [
        { value: "", label: "none" },
        { value: "forced", label: "forced" },
      ],
    },
    { name: "per_channel", label: "Per channel", type: "bool" },
  ],
  slice: [
    { name: "start_seconds", label: "Start (s)", type: "number", required: true },
    { name: "end_seconds", label: "End (s)", type: "number", required: true },
  ],
  "text.translate": [
    { name: "target_language", label: "Target language", type: "text", required: true, placeholder: "es" },
    { name: "source_language", label: "Source language", type: "text", default: "auto" },
  ],
  "text.summarize": [
    {
      name: "mode",
      label: "Mode",
      type: "select",
      options: [
        { value: "abstract", label: "abstract" },
        { value: "bullets", label: "bullets" },
        { value: "chapters", label: "chapters" },
        { value: "action_items", label: "action_items" },
      ],
    },
    { name: "max_tokens", label: "Max tokens", type: "number", default: 512 },
  ],
  "audio.chapters": [],
  "text.moderate": [
    {
      name: "engine",
      label: "Engine",
      type: "select",
      options: [
        { value: "lexicon", label: "lexicon" },
        { value: "llm", label: "llm" },
      ],
    },
    { name: "mask_profanity", label: "Mask profanity", type: "bool" },
  ],
  "audio.redact": REDACT_FIELDS,
  "text.redact": REDACT_FIELDS,
  "audio.enhance": [
    {
      name: "mode",
      label: "Mode",
      type: "select",
      options: [
        { value: "denoise", label: "denoise" },
        { value: "dereverb", label: "dereverb" },
        { value: "isolate", label: "isolate" },
        { value: "background_voice", label: "background_voice" },
        { value: "telephony", label: "telephony" },
        { value: "accent", label: "accent" },
      ],
    },
  ],
  "audio.edit": [
    {
      name: "mode",
      label: "Mode",
      type: "select",
      options: [{ value: "remove_fillers", label: "remove_fillers" }],
    },
  ],
  "speaker.enroll": [
    { name: "name", label: "Speaker name", type: "text", required: true },
    { name: "consent", label: "I have consent to enroll this voiceprint", type: "bool", required: true, mustBeTrue: true },
  ],
};

/* ---- job chaining: processors that read a prior transcript ---- */

const CHAINABLE_EXACT = new Set([
  "audio.diarize",
  "audio.emotion",
  "audio.events",
  "audio.chapters",
  "audio.redact",
  "audio.edit",
]);

/** Processors that operate on a prior job's transcript via `params.source_job_id`. */
export function isChainable(name: string): boolean {
  return name.startsWith("text.") || CHAINABLE_EXACT.has(name);
}

/* ---- JSON-Schema → fields ---- */

function humanize(s: string): string {
  return s
    .replace(/[_.]/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

function fieldsFromJsonSchema(schema: unknown): ParamField[] | null {
  if (!schema || typeof schema !== "object") return null;
  const s = schema as Record<string, unknown>;
  const props = s.properties;
  if (!props || typeof props !== "object") return null;
  const entries = Object.entries(props as Record<string, unknown>);
  if (entries.length === 0) return null;

  const required = new Set(Array.isArray(s.required) ? (s.required as unknown[]).map(String) : []);
  const fields: ParamField[] = [];

  for (const [key, raw] of entries) {
    if (!raw || typeof raw !== "object") continue;
    const p = raw as Record<string, unknown>;
    const enumVals = Array.isArray(p.enum) ? (p.enum as unknown[]).map(String) : undefined;
    const jtype = typeof p.type === "string" ? p.type : Array.isArray(p.type) ? String(p.type[0]) : undefined;
    const isArray = jtype === "array";
    const itemEnum =
      isArray && p.items && typeof p.items === "object" && Array.isArray((p.items as Record<string, unknown>).enum)
        ? ((p.items as Record<string, unknown>).enum as unknown[]).map(String)
        : undefined;

    let type: ParamFieldType = "text";
    if (itemEnum) type = "multiselect";
    else if (enumVals) type = "select";
    else if (jtype === "number" || jtype === "integer") type = "number";
    else if (jtype === "boolean") type = "bool";

    const options = (itemEnum ?? enumVals)?.map((v) => ({ value: v, label: v }));

    fields.push({
      name: key,
      label: humanize(typeof p.title === "string" ? p.title : key),
      type,
      required: required.has(key),
      help: typeof p.description === "string" ? p.description : undefined,
      default: p.default as ParamValue,
      options,
    });
  }
  return fields.length > 0 ? fields : null;
}

/** Resolve the param fields for a processor: JSON-Schema first, curated fallback. */
export function fieldsForProcessor(name: string, inputSchema?: unknown): ParamField[] {
  return fieldsFromJsonSchema(inputSchema) ?? CURATED_PARAMS[name] ?? [];
}

/* ---- values: init / validate / build ---- */

export function initValues(fields: ParamField[]): ParamValues {
  const v: ParamValues = {};
  for (const f of fields) {
    if (f.default !== undefined) v[f.name] = f.default;
    else if (f.type === "multiselect") v[f.name] = [];
    else if (f.type === "bool") v[f.name] = false;
    // A select with no explicit default must seed to its first option — otherwise
    // the browser shows option[0] as selected while state stays undefined and the
    // (visibly selected) value is never submitted.
    else if (f.type === "select") v[f.name] = f.options?.[0]?.value;
  }
  return v;
}

function isEmpty(v: ParamValue): boolean {
  if (v === undefined || v === null) return true;
  if (typeof v === "string") return v.trim() === "";
  if (Array.isArray(v)) return v.length === 0;
  return false;
}

/** Returns the labels of unmet required fields; empty array means valid. */
export function validateValues(fields: ParamField[], values: ParamValues): string[] {
  const missing: string[] = [];
  for (const f of fields) {
    if (f.mustBeTrue && values[f.name] !== true) {
      missing.push(f.label);
      continue;
    }
    if (f.required && f.type !== "bool" && isEmpty(values[f.name])) missing.push(f.label);
  }
  return missing;
}

/** Build the wire `params` object, dropping empty / redundant values. */
export function buildParams(fields: ParamField[], values: ParamValues): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const v = values[f.name];
    switch (f.type) {
      case "bool":
        // Emit when on, or always for required / must-be-true / default-on fields
        // (so a default-on boolean can actually be turned OFF from the UI).
        if (v === true || f.required || f.mustBeTrue || f.default === true) out[f.name] = v === true;
        break;
      case "number": {
        if (isEmpty(v)) break;
        const n = typeof v === "number" ? v : Number(v);
        if (!Number.isNaN(n)) out[f.name] = n;
        break;
      }
      case "multiselect":
        if (Array.isArray(v) && v.length > 0) out[f.name] = v;
        break;
      default:
        if (!isEmpty(v)) out[f.name] = v;
    }
  }
  return out;
}
