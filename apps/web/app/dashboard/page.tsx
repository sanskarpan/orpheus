/*
 * SCAFFOLD — NON-FUNCTIONAL. Do not build/deploy.
 *
 * Jobs dashboard (route: "/dashboard") for the Orpheus admin dashboard (gap #11).
 * Target: filterable job list (processor, status, date); row → /jobs/[id].
 * Data comes from the Go API (GET /v1/jobs) under the user's token (RLS).
 * See docs/design/11-web-ui-and-billing.md §2.1.
 */

export default function DashboardPage() {
  // SCAFFOLD: no data fetching. Real version is an RSC listing jobs via the API.
  return (
    <main data-scaffold="dashboard">
      <h1>Jobs (scaffold)</h1>
      <p>
        Placeholder for the jobs list/detail views. Not implemented — see{" "}
        <code>docs/design/11-web-ui-and-billing.md</code>.
      </p>
    </main>
  );
}
