import "server-only";
import { randomBytes, scryptSync, timingSafeEqual, createCipheriv, createDecipheriv, createHash } from "node:crypto";
import { mkdirSync } from "node:fs";
import path from "node:path";
import Database from "better-sqlite3";

/* ================================================================== *
 * Local user-account store (the app-level auth layer that sits in
 * front of the machine-to-machine API).
 *
 * A user logs in here with email + password; the account holds the
 * org's provisioned API key (encrypted at rest). The API key is never
 * exposed to the browser — the BFF reads it server-side to call /v1.
 *
 * SQLite keeps this fully local with zero extra infrastructure.
 * ================================================================== */

export interface Account {
  id: string;
  email: string;
  name: string;
  org_id: string;
  org_key: string; // decrypted; only returned by server-side lookups
  /** The API-key id of the owner key (org_key). Used to label/protect it in the
   * keys UI by exact id rather than an ambiguous 9-char prefix. May be absent on
   * accounts created before this column existed. */
  org_key_id?: string;
  is_platform_admin: boolean;
  created_at: string;
}

const DB_DIR = path.join(process.cwd(), ".data");
const DB_PATH = path.join(DB_DIR, "accounts.db");

let _db: Database.Database | null = null;

function db(): Database.Database {
  if (_db) return _db;
  mkdirSync(DB_DIR, { recursive: true });
  const d = new Database(DB_PATH);
  d.pragma("journal_mode = WAL");
  d.exec(`
    CREATE TABLE IF NOT EXISTS accounts (
      id                TEXT PRIMARY KEY,
      email             TEXT NOT NULL UNIQUE,
      password_hash     TEXT NOT NULL,
      name              TEXT NOT NULL,
      org_id            TEXT NOT NULL,
      org_key_enc       TEXT NOT NULL,
      is_platform_admin INTEGER NOT NULL DEFAULT 0,
      created_at        TEXT NOT NULL
    );
  `);
  // Additive migration for accounts created before the owner-key id was tracked.
  try {
    d.exec("ALTER TABLE accounts ADD COLUMN org_key_id TEXT");
  } catch {
    // Column already exists — SQLite has no idempotent ADD COLUMN.
  }
  _db = d;
  return d;
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

function countAccounts(): number {
  return (db().prepare("SELECT COUNT(*) AS n FROM accounts").get() as { n: number }).n;
}

/** The first account created, or any configured email, is a platform admin. */
export function resolvePlatformAdmin(email: string): boolean {
  return countAccounts() === 0 || adminEmails().has(email.toLowerCase());
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
    is_platform_admin: r.is_platform_admin === 1,
    created_at: r.created_at,
  };
}

export function emailExists(email: string): boolean {
  return !!db().prepare("SELECT 1 FROM accounts WHERE email = ?").get(email.toLowerCase());
}

export function createAccount(input: {
  id: string;
  email: string;
  password: string;
  name: string;
  org_id: string;
  org_key: string;
  org_key_id?: string;
  is_platform_admin: boolean;
  created_at: string;
}): Account {
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
  db()
    .prepare(
      `INSERT INTO accounts (id, email, password_hash, name, org_id, org_key_enc, org_key_id, is_platform_admin, created_at)
       VALUES (@id, @email, @password_hash, @name, @org_id, @org_key_enc, @org_key_id, @is_platform_admin, @created_at)`,
    )
    .run(row);
  return rowToAccount(row);
}

/** Backfill/repair the stored owner-key id for an account. */
export function setOrgKeyId(accountId: string, keyId: string): void {
  db().prepare("UPDATE accounts SET org_key_id = ? WHERE id = ?").run(keyId, accountId);
}

export function getAccountById(id: string): Account | null {
  try {
    const r = db().prepare("SELECT * FROM accounts WHERE id = ?").get(id) as Row | undefined;
    return r ? rowToAccount(r) : null;
  } catch {
    // Corrupt org_key_enc or a rotated SESSION_SECRET makes decrypt() throw.
    // Treat as "no account" so callers run the stale-session recovery path
    // instead of 500-ing with no way out.
    return null;
  }
}

/** Verify email + password; returns the account on success, null otherwise. */
export function authenticate(email: string, password: string): Account | null {
  const r = db().prepare("SELECT * FROM accounts WHERE email = ?").get(email.toLowerCase()) as Row | undefined;
  if (!r) return null;
  if (!verifyPassword(password, r.password_hash)) return null;
  return rowToAccount(r);
}

export function findByEmail(email: string): Account | null {
  const r = db().prepare("SELECT * FROM accounts WHERE email = ?").get(email.toLowerCase()) as Row | undefined;
  return r ? rowToAccount(r) : null;
}
