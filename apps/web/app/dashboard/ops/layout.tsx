import { redirect } from "next/navigation";
import { getAccount } from "@/lib/session";

/** Platform-operator gate: only platform-admin accounts may enter /dashboard/ops. */
export default async function OpsLayout({ children }: { children: React.ReactNode }) {
  const account = await getAccount();
  if (!account) redirect("/login");
  if (!account.is_platform_admin) redirect("/dashboard");
  return <>{children}</>;
}
