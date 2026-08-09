import Link from "next/link";
import { redirect } from "next/navigation";
import { AuthLayout } from "@/components/auth/AuthLayout";
import { getAccount } from "@/lib/session";
import { LoginForm } from "./LoginForm";

export const dynamic = "force-dynamic";

export default async function LoginPage({ searchParams }: { searchParams: { next?: string } }) {
  // Already signed in with a *valid* account → go to the dashboard.
  if (await getAccount()) redirect("/dashboard");
  const next = searchParams.next && searchParams.next.startsWith("/") ? searchParams.next : "/dashboard";
  return (
    <AuthLayout
      title="Log in"
      subtitle="Welcome back. Sign in to your Orpheus workspace."
      footer={
        <>
          New here?{" "}
          <Link href="/signup" className="text-brass hover:underline">
            Create an account
          </Link>
        </>
      }
    >
      <LoginForm next={next} />
    </AuthLayout>
  );
}
