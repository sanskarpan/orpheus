import { NextResponse, type NextRequest } from "next/server";

/**
 * Edge auth guard for the user session.
 *
 * The dashboard lives under /dashboard and requires a signed-in account; the
 * landing page (/), /login and /signup are public. The session cookie is
 * encrypted (iron-session) so this only checks its presence — the account is
 * loaded and verified server-side in the dashboard layout.
 */
export function middleware(req: NextRequest) {
  const { pathname } = req.nextUrl;
  const hasSession = req.cookies.has("orpheus_session");

  const isProtected = pathname === "/dashboard" || pathname.startsWith("/dashboard/");

  if (isProtected && !hasSession) {
    const url = req.nextUrl.clone();
    url.pathname = "/login";
    url.searchParams.set("next", pathname);
    return NextResponse.redirect(url);
  }

  // NOTE: we deliberately do NOT bounce authenticated users off /login|/signup
  // here. Cookie *presence* is not proof of a valid account (the cookie can be
  // stale — e.g. its account was removed), and bouncing on presence alone
  // creates a redirect loop with the dashboard's account check. Those pages
  // validate the account server-side and redirect to /dashboard themselves.
  return NextResponse.next();
}

export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon.ico|.*\\.(?:svg|png|jpg|jpeg|gif|webp|ico|woff2?)$).*)"],
};
