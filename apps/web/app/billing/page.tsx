/*
 * SCAFFOLD — NON-FUNCTIONAL. Do not build/deploy.
 *
 * Billing page (route: "/billing") for the Orpheus admin dashboard (gap #11).
 * Target: current plan/subscription, metered usage, invoices, payment method.
 * Reads via the Go API (GET /v1/billing/*, GET /v1/usage); charging is Stripe.
 * We store usage + invoice metadata + Stripe IDs only — never card data.
 * See docs/design/11-web-ui-and-billing.md §2.2, §4.
 */

export default function BillingPage() {
  // SCAFFOLD: no data fetching. Real version renders subscription + invoices
  // from the API and links to the Stripe Billing Portal / Checkout sessions.
  return (
    <main data-scaffold="billing">
      <h1>Billing (scaffold)</h1>
      <p>
        Placeholder for plan, usage, and invoices. Charging is handled by Stripe
        (PCI scope stays with Stripe). Not implemented — see{" "}
        <code>docs/design/11-web-ui-and-billing.md</code>.
      </p>
    </main>
  );
}
