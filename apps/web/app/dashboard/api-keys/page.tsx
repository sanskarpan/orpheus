import { orpheus, type APIKey } from "@/lib/orpheus";
import { getAccount } from "@/lib/session";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { KeysManager } from "./KeysManager";

export const dynamic = "force-dynamic";

export default async function ApiKeysPage() {
  const account = await getAccount();
  // The owner key (used by the dashboard itself) is identified by its prefix so
  // the UI can label it and prevent its revocation — revoking it would lock the
  // account out of the API.
  const ownerPrefix = account ? account.org_key.slice(0, 9) : "";

  let keys: APIKey[] = [];
  let error: string | null = null;
  try {
    keys = (await orpheus.listKeys(100)).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
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
        <KeysManager initial={keys} ownerPrefix={ownerPrefix} />
      )}
    </div>
  );
}
