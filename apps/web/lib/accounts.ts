import "server-only";
import { randomBytes, scryptSync, timingSafeEqual, createCipheriv, createDecipheriv, createHash } from "node:crypto";

/* ================================================================== *
 * User-account store (the app-level auth layer that sits in front of
 * the machine-to-machine API).
 *
 * A user logs in here with email + password; the account holds the
 * org's provisioned API key (encrypted at rest). The API key is never
 * exposed to the browser — the BFF reads it server-side to call /v1.
 *
 * Storage is Cloudflare D1 (serverless SQLite over HTTP) so the store
 * works on serverless hosts (Vercel) that have no persistent local
 * disk. All lookups are async. Config via CLOUDFLARE_ACCOUNT_ID,
 * CLOUDFLARE_API_TOKEN, D1_DATABASE_ID.
 * ================================================================== */

export interface Account {
  id: string;
  email: string;
  name: string;
  org_id: string;
  org_key: string; // decrypted; only returned by server-side lookups
  org_key_id?: string;
  is_platform_admin: boolean;
  created_at: string;
}

/* ---- Cloudflare D1 HTTP client ---- */

function d1Config(): { acct: string; token: string; db: string } {
  const acct = process.env.CLOUDFLARE_ACCOUNT_ID;
  const token = process.env.CLOUDFLARE_API_TOKEN;
  const db = process.env.D1_DATABASE_ID;
  if (!acct || !token || !db) {
    throw new Error(
      "Account store is not configured: set CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN, D1_DATABASE_ID.",
    );
  }
  return { acct, token, db };
}

async function d1Query<T = Record<string, unknown>>(sql: string, params: unknown[] = []): Promise<T[]> {
  const { acct, token, db } = d1Config();
  const res = await fetch(
    `https://api.cloudflare.com/client/v4/accounts/${acct}/d1/database/${db}/query`,
    {
      method: "POST",
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ sql, params }),
      cache: "no-store",
    },
  );
  const body = (await res.json()) as {
    success: boolean;
    result?: { results?: T[] }[];
    errors?: { message: string }[];
  };
  if (!res.ok || !body.success) {
    const msg = body.errors?.map((e) => e.message).join("; ") || `HTTP ${res.status}`;
    throw new Error(`D1 query failed: ${msg}`);
  }
  return body.result?.[0]?.results ?? [];
}

let _schemaReady: Promise<void> | null = null;
function ensureSchema(): Promise<void> {
  if (!_schemaReady) {
    _schemaReady = d1Query(
      `CREATE TABLE IF NOT EXISTS accounts (
         id                TEXT PRIMARY KEY,
         email             TEXT NOT NULL UNIQUE,
         password_hash     TEXT NOT NULL,
         name              TEXT NOT NULL,
         org_id            TEXT NOT NULL,
         org_key_enc       TEXT NOT NULL,
         org_key_id        TEXT,
         is_platform_admin INTEGER NOT NULL DEFAULT 0,
         created_at        TEXT NOT NULL
       );`,
    ).then(() => undefined);
  }
  return _schemaReady;
}

/* ---- password hashing (scrypt, no native dep) ---- */

export function hashPassword(password: string): string {
  const salt = randomBytes(16);
  const derived = scryptSync(password, salt, 64);
  return `scrypt$${salt.toString("hex")}$${derived.toString("hex")}`;
}

export function verifyPassword(password: string, stored: string): boolean {
  const [scheme, saltHex, hashHex] = stored.split("$");
  if (scheme !== "scrypt" || !saltHex || !hashHex) return false;
  const derived = scryptSync(password, Buffer.from(saltHex, "hex"), 64);
  const expected = Buffer.from(hashHex, "hex");
  return derived.length === expected.length && timingSafeEqual(derived, expected);
}

/* ---- org-key encryption at rest (AES-256-GCM) ---- */

function encKey(): Buffer {
  const secret = process.env.SESSION_SECRET ?? "orpheus-dev-session-secret-change-me-please-32b";
  return createHash("sha256").update(`orpheus-orgkey:${secret}`).digest();
}

function encrypt(plain: string): string {
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", encKey(), iv);
  const ct = Buffer.concat([cipher.update(plain, "utf8"), cipher.final()]);
  const tag = cipher.getAuthTag();
  return `${iv.toString("hex")}:${tag.toString("hex")}:${ct.toString("hex")}`;
}

function decrypt(blob: string): string {
  const [ivHex, tagHex, ctHex] = blob.split(":");
  const decipher = createDecipheriv("aes-256-gcm", encKey(), Buffer.from(ivHex, "hex"));
  decipher.setAuthTag(Buffer.from(tagHex, "hex"));
  return Buffer.concat([decipher.update(Buffer.from(ctHex, "hex")), decipher.final()]).toString("utf8");
}

/* ---- platform-admin policy ---- */

function adminEmails(): Set<string> {
  return new Set(
    (process.env.ORPHEUS_PLATFORM_ADMIN_EMAILS ?? "")
      .split(",")
      .map((e) => e.trim().toLowerCase())
      .filter(Boolean),
  );
}

async function countAccounts(): Promise<number> {
  const rows = await d1Query<{ n: number }>("SELECT COUNT(*) AS n FROM accounts");
  return rows[0]?.n ?? 0;
}

/** The first account created, or any configured email, is a platform admin. */
export async function resolvePlatformAdmin(email: string): Promise<boolean> {
  if (adminEmails().has(email.toLowerCase())) return true;
  return (await countAccounts()) === 0;
}

/* ---- account CRUD ---- */

interface Row {
  id: string;
  email: string;
  name: string;
  org_id: string;
  org_key_enc: string;
  org_key_id: string | null;
  is_platform_admin: number;
  created_at: string;
  password_hash: string;
}

function rowToAccount(r: Row): Account {
  return {
    id: r.id,
    email: r.email,
    name: r.name,
    org_id: r.org_id,
    org_key: decrypt(r.org_key_enc),
    org_key_id: r.org_key_id ?? undefined,
    is_platform_admin: Number(r.is_platform_admin) === 1,
    created_at: r.created_at,
  };
}

export async function emailExists(email: string): Promise<boolean> {
  await ensureSchema();
  const rows = await d1Query("SELECT 1 AS ok FROM accounts WHERE email = ?", [email.toLowerCase()]);
  return rows.length > 0;
}

export async function createAccount(input: {
  id: string;
  email: string;
  password: string;
  name: string;
  org_id: string;
  org_key: string;
  org_key_id?: string;
  is_platform_admin: boolean;
  created_at: string;
}): Promise<Account> {
  await ensureSchema();
  const row: Row = {
    id: input.id,
    email: input.email.toLowerCase(),
    name: input.name,
    org_id: input.org_id,
    org_key_enc: encrypt(input.org_key),
    org_key_id: input.org_key_id ?? null,
    is_platform_admin: input.is_platform_admin ? 1 : 0,
    created_at: input.created_at,
    password_hash: hashPassword(input.password),
  };
  await d1Query(
    `INSERT INTO accounts (id, email, password_hash, name, org_id, org_key_enc, org_key_id, is_platform_admin, created_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [row.id, row.email, row.password_hash, row.name, row.org_id, row.org_key_enc, row.org_key_id, row.is_platform_admin, row.created_at],
  );
  return rowToAccount(row);
}

/** Backfill/repair the stored owner-key id for an account. */
export async function setOrgKeyId(accountId: string, keyId: string): Promise<void> {
  await d1Query("UPDATE accounts SET org_key_id = ? WHERE id = ?", [keyId, accountId]);
}

export async function getAccountById(id: string): Promise<Account | null> {
  try {
    await ensureSchema();
    const rows = await d1Query<Row>("SELECT * FROM accounts WHERE id = ?", [id]);
    return rows[0] ? rowToAccount(rows[0]) : null;
  } catch {
    // Corrupt org_key_enc or a rotated SESSION_SECRET makes decrypt() throw.
    // Treat as "no account" so callers run the stale-session recovery path.
    return null;
  }
}

/** Verify email + password; returns the account on success, null otherwise. */
export async function authenticate(email: string, password: string): Promise<Account | null> {
  await ensureSchema();
  const rows = await d1Query<Row>("SELECT * FROM accounts WHERE email = ?", [email.toLowerCase()]);
  const r = rows[0];
  if (!r) return null;
  if (!verifyPassword(password, r.password_hash)) return null;
  return rowToAccount(r);
}

export async function findByEmail(email: string): Promise<Account | null> {
  await ensureSchema();
  const rows = await d1Query<Row>("SELECT * FROM accounts WHERE email = ?", [email.toLowerCase()]);
  return rows[0] ? rowToAccount(rows[0]) : null;
}
