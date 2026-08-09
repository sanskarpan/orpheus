import { redirect } from "next/navigation";
import { Shell } from "@/components/Shell";
import { getAccount } from "@/lib/session";

export default async function DashboardLayout({ children }: { children: React.ReactNode }) {
  const account = await getAccount();
  // A cookie may be present but resolve to no account (stale session). Send such
  // requests through /logout to CLEAR the cookie — redirecting straight to
  // /login would loop, since the cookie would still look "present".
  if (!account) redirect("/logout");

  return (
    <Shell
      name={account.name}
      email={account.email}
      orgId={account.org_id}
      admin={account.is_platform_admin}
    >
      {children}
    </Shell>
  );
}
