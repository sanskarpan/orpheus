import { NextResponse } from "next/server";
import { getSession } from "@/lib/session";

export const dynamic = "force-dynamic";

/**
 * Clears the session cookie and returns to /login. Used both for explicit
 * sign-out and to recover from a *stale* cookie (one whose account no longer
 * resolves) — a route handler can delete cookies, a Server Component can't,
 * so the dashboard layout redirects here instead of looping back to /login.
 */
export async function GET(req: Request) {
  const session = await getSession();
  session.destroy();
  return NextResponse.redirect(new URL("/login", req.url));
}
