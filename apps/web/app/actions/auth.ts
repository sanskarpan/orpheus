"use server";

import { randomUUID } from "node:crypto";
import { redirect } from "next/navigation";
import { apiRequest, OrpheusError, type APIKey, type ProvisionResponse } from "@/lib/orpheus";
import { getSession } from "@/lib/session";
import {
  authenticate,
  createAccount,
  emailExists,
  findByEmail,
  resolvePlatformAdmin,
} from "@/lib/accounts";

type FormResult = { error: string } | void;

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

function adminKey(): string | null {
  return process.env.ORPHEUS_ADMIN_KEY?.trim() || null;
}

/**
 * Provision a real Orpheus org + owner API key for a new account, using the
 * BFF's platform-admin key. The owner key holds "*" (org-scoped) so the user
 * can manage their own keys from the dashboard.
 */
async function provisionOrg(orgName: string, email: string): Promise<ProvisionResponse> {
  const key = adminKey();
  if (!key) {
    throw new Error(
      "Server is not configured to provision tenants (ORPHEUS_ADMIN_KEY missing). Run `make bootstrap-admin`.",
    );
  }
  return apiRequest<ProvisionResponse>("/v1/onboarding/provision", {
    method: "POST",
    apiKey: key,
    body: { org_name: orgName, user_email: email, scopes: ["*"] },
  });
}

/**
 * Resolve the id of the just-provisioned owner key. A freshly provisioned org
 * has exactly one API key (the owner key), so listing under that key uniquely
 * identifies it — no reliance on the ambiguous 9-char prefix. Best-effort:
 * returns undefined on any failure, and the keys UI falls back to prefix match.
 */
async function resolveOwnerKeyId(apiKey: string, keyPrefix: string): Promise<string | undefined> {
  try {
    const res = await apiRequest<{ data: APIKey[] }>("/v1/api-keys", { apiKey, query: { limit: 100 } });
    const keys = res.data ?? [];
    if (keys.length === 1) return keys[0].id;
    return keys.find((k) => k.prefix === keyPrefix)?.id;
  } catch {
    return undefined;
  }
}

async function openSession(userId: string) {
  const session = await getSession();
  session.userId = userId;
  await session.save();
}

export async function signUp(_prev: unknown, formData: FormData): Promise<FormResult> {
  const name = String(formData.get("name") ?? "").trim();
  const email = String(formData.get("email") ?? "").trim().toLowerCase();
  const password = String(formData.get("password") ?? "");
  const workspace = String(formData.get("workspace") ?? "").trim();
  const next = String(formData.get("next") ?? "/dashboard") || "/dashboard";

  if (!name) return { error: "Enter your name." };
  if (!EMAIL_RE.test(email)) return { error: "Enter a valid email address." };
  if (password.length < 8) return { error: "Password must be at least 8 characters." };
  if (emailExists(email)) return { error: "An account with that email already exists — try logging in." };

  const orgName = workspace || `${name}'s Workspace`;

  let provisioned: ProvisionResponse;
  try {
    provisioned = await provisionOrg(orgName, email);
  } catch (e) {
    if (e instanceof OrpheusError) {
      if (e.status >= 500) return { error: "The API is unreachable right now. Is it running on :8090?" };
      return { error: `Couldn't create your workspace: ${e.detail || e.message}` };
    }
    return { error: e instanceof Error ? e.message : "Couldn't create your workspace." };
  }

  const orgKeyId = await resolveOwnerKeyId(provisioned.api_key, provisioned.key_prefix);

  const account = createAccount({
    id: randomUUID(),
    email,
    password,
    name,
    org_id: provisioned.org_id,
    org_key: provisioned.api_key,
    org_key_id: orgKeyId,
    is_platform_admin: resolvePlatformAdmin(email),
    created_at: new Date().toISOString(),
  });

  await openSession(account.id);
  redirect(next);
}

export async function signIn(_prev: unknown, formData: FormData): Promise<FormResult> {
  const email = String(formData.get("email") ?? "").trim().toLowerCase();
  const password = String(formData.get("password") ?? "");
  const next = String(formData.get("next") ?? "/dashboard") || "/dashboard";

  if (!EMAIL_RE.test(email)) return { error: "Enter a valid email address." };
  if (!password) return { error: "Enter your password." };

  const account = authenticate(email, password);
  if (!account) return { error: "Incorrect email or password." };

  await openSession(account.id);
  redirect(next);
}

/**
 * One-click local access: signs into a demo account, creating + provisioning it
 * the first time. Purely for local development convenience.
 */
export async function demoLogin(): Promise<FormResult> {
  const email = "demo@orpheus.local";
  let account = findByEmail(email);

  if (!account) {
    let provisioned: ProvisionResponse;
    try {
      provisioned = await provisionOrg("Demo Workspace", email);
    } catch (e) {
      if (e instanceof OrpheusError && e.status >= 500) {
        return { error: "The API is unreachable right now. Is it running on :8090?" };
      }
      return { error: e instanceof Error ? e.message : "Couldn't set up the demo account." };
    }
    const orgKeyId = await resolveOwnerKeyId(provisioned.api_key, provisioned.key_prefix);
    account = createAccount({
      id: randomUUID(),
      email,
      password: `demo-${randomUUID()}`, // random; demo is entered via this button only
      name: "Demo User",
      org_id: provisioned.org_id,
      org_key: provisioned.api_key,
      org_key_id: orgKeyId,
      is_platform_admin: resolvePlatformAdmin(email),
      created_at: new Date().toISOString(),
    });
  }

  await openSession(account.id);
  redirect("/dashboard");
}

export async function signOut(): Promise<void> {
  const session = await getSession();
  session.destroy();
  redirect("/login");
}
