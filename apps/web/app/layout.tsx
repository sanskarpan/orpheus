/*
 * SCAFFOLD — NON-FUNCTIONAL. Do not build/deploy.
 *
 * Root layout for the Orpheus admin dashboard (gap #11).
 * Placeholder only: no auth, no navigation, no styling wired.
 * See docs/design/11-web-ui-and-billing.md.
 */

// NOTE: `next`/`react` are declared in package.json for shape only; this app is
// not installed or built yet, so these imports will not resolve until the
// design-11 checklist is done. That is expected for a scaffold.
import type { ReactNode } from "react";

export const metadata = {
  title: "Orpheus Admin (scaffold)",
  description: "SCAFFOLD — admin dashboard not implemented yet.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        {/* SCAFFOLD: real layout will add Keycloak-gated nav + RBAC-aware chrome. */}
        <div data-scaffold="orpheus-web-root">{children}</div>
      </body>
    </html>
  );
}
