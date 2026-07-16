/*
 * SCAFFOLD — NON-FUNCTIONAL. Do not build/deploy.
 *
 * Overview page (route: "/") for the Orpheus admin dashboard (gap #11).
 * Target: usage-this-period, spend, recent jobs, DLQ count, SLO summary —
 * all fetched server-side from the Go API under the user's token (RLS).
 * See docs/design/11-web-ui-and-billing.md §2.1.
 */

export default function OverviewPage() {
  // SCAFFOLD: no data fetching yet. A real implementation is a React Server
  // Component that calls the Go API (e.g. GET /v1/usage, GET /v1/jobs?limit=10).
  return (
    <main data-scaffold="overview">
      <h1>Orpheus Admin — scaffold</h1>
      <p>
        This page is a placeholder for gap #11. See{" "}
        <code>docs/design/11-web-ui-and-billing.md</code> for the target
        information architecture and build checklist.
      </p>
    </main>
  );
}
