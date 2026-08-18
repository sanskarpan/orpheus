import { orpheus, type APIKey } from "@/lib/orpheus";
import { getAccount } from "@/lib/session";
import { setOrgKeyId } from "@/lib/accounts";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { KeysManager } from "./KeysManager";

export const dynamic = "force-dynamic";

export default async function ApiKeysPage() {
  const account = await getAccount();
  // The owner key (used by the dashboard itself) must be labelled and protected
  // from revocation — revoking it locks the account out of the API. It's matched
  // by its exact key id; the 9-char prefix is only a last-resort fallback since
  // prefixes collide across keys (~1/64).
  const ownerPrefix = account ? account.org_key.slice(0, 9) : "";

  let keys: APIKey[] = [];
  let error: string | null = null;
  try {
    keys = (await orpheus.listKeys(100)).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  // Prefer the stored owner-key id. For legacy accounts that predate the column,
  // backfill it when exactly one listed key matches the owner prefix (unambiguous).
  let ownerKeyId = account?.org_key_id ?? "";
  if (account && !ownerKeyId && ownerPrefix) {
    const matches = keys.filter((k) => k.prefix === ownerPrefix);
    if (matches.length === 1) {
      ownerKeyId = matches[0].id;
      try {
        setOrgKeyId(account.id, ownerKeyId);
      } catch {
        // Non-fatal: labelling still works this render via ownerKeyId.
      }
    }
  }

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Platform"
        title="API Keys"
        sub="Machine credentials for the Orpheus API. Create scoped keys for your own integrations — secrets are shown once."
      />
      {error ? (
        <ErrorNotice title="Couldn't load keys" detail={error} />
      ) : (
        <KeysManager initial={keys} ownerKeyId={ownerKeyId} ownerPrefix={ownerPrefix} />
      )}
    </div>
  );
}
