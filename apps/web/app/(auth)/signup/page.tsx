import Link from "next/link";
import { redirect } from "next/navigation";
import { AuthLayout } from "@/components/auth/AuthLayout";
import { getAccount } from "@/lib/session";
import { SignupForm } from "./SignupForm";

export const dynamic = "force-dynamic";

export default async function SignupPage({ searchParams }: { searchParams: { next?: string } }) {
  if (await getAccount()) redirect("/dashboard");
  const next = searchParams.next && searchParams.next.startsWith("/") ? searchParams.next : "/dashboard";
  return (
    <AuthLayout
      title="Create your account"
      subtitle="Start processing audio in under a minute — no API key required."
      footer={
        <>
          Already have an account?{" "}
          <Link href="/login" className="text-brass hover:underline">
            Log in
          </Link>
        </>
      }
    >
      <SignupForm next={next} />
    </AuthLayout>
  );
}
