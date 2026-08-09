"use server";

import { revalidatePath } from "next/cache";
import { apiRequest, OrpheusError, type ProvisionResponse } from "@/lib/orpheus";
import { getAccount } from "@/lib/session";
import type { ActionResult } from "@/app/actions/jobs";

function wrap(e: unknown): string {
  if (e instanceof OrpheusError) return e.detail || e.message;
  return String(e);
}

/**
 * Platform-operator actions run with the BFF's platform-admin key (a normal
 * org owner key holds org-scoped "*" but NOT the cross-tenant platform:admin
 * role). Every action re-checks the caller is a platform admin — the /ops
 * layout already gates the route, this is defense in depth.
 */
async function requirePlatformAdmin(): Promise<string | { error: string }> {
  const account = await getAccount();
  if (!account?.is_platform_admin) return { error: "Not authorized." };
  const key = process.env.ORPHEUS_ADMIN_KEY?.trim();
  if (!key) return { error: "Server missing ORPHEUS_ADMIN_KEY (run `make bootstrap-admin`)." };
  return key;
}

export async function provisionAction(input: {
  org_name: string;
  user_email: string;
  key_name?: string;
}): Promise<ActionResult<ProvisionResponse>> {
  const adminKey = await requirePlatformAdmin();
  if (typeof adminKey !== "string") return { ok: false, error: adminKey.error };
  try {
    const res = await apiRequest<ProvisionResponse>("/v1/onboarding/provision", {
      method: "POST",
      apiKey: adminKey,
      body: { ...input, scopes: ["*"] },
    });
    revalidatePath("/dashboard/ops");
    return { ok: true, data: res };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}

export async function reviewSubmissionAction(
  id: string,
  decision: "approve" | "reject",
  notes?: string,
): Promise<ActionResult<{ id: string; status: string }>> {
  const adminKey = await requirePlatformAdmin();
  if (typeof adminKey !== "string") return { ok: false, error: adminKey.error };
  try {
    const res = await apiRequest<{ id: string; status: string }>(`/v1/marketplace/submissions/${id}/review`, {
      method: "POST",
      apiKey: adminKey,
      body: { decision, notes },
    });
    revalidatePath("/dashboard/ops/marketplace");
    return { ok: true, data: res };
  } catch (e) {
    return { ok: false, error: wrap(e) };
  }
}
