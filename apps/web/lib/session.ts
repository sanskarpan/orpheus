import "server-only";
import { getIronSession, type SessionOptions } from "iron-session";
import { cookies } from "next/headers";
import { getAccountById, type Account } from "@/lib/accounts";

/**
 * The BFF session. This is a *user* session now — it carries only the account
 * id. The org's API key is a machine credential and lives encrypted in the
 * local account store; it never touches the cookie or the browser.
 */
export interface OrpheusSession {
  userId?: string;
}

const SESSION_SECRET =
  process.env.SESSION_SECRET ??
  // Dev fallback so the app boots without a .env.local. Never used in prod:
  // set SESSION_SECRET (>= 32 chars) in the environment.
  "orpheus-dev-session-secret-change-me-please-32b";

export const sessionOptions: SessionOptions = {
  password: SESSION_SECRET,
  cookieName: "orpheus_session",
  cookieOptions: {
    httpOnly: true,
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: 60 * 60 * 24 * 7, // 7-day user session
  },
};

export async function getSession() {
  return getIronSession<OrpheusSession>(cookies(), sessionOptions);
}

/** The signed-in account (with decrypted org key), or null when signed out. */
export async function getAccount(): Promise<Account | null> {
  try {
    const s = await getSession();
    if (!s.userId) return null;
    return await getAccountById(s.userId);
  } catch {
    // A malformed/undecryptable cookie must resolve to "signed out", never a crash.
    return null;
  }
}

/**
 * The org API key for the current session, or null when signed out. Resolved
 * from the account store, never from the cookie.
 */
export async function getApiKey(): Promise<string | null> {
  const account = await getAccount();
  return account?.org_key ?? null;
}
