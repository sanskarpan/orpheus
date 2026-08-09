import { orpheus, type MarketplaceSubmission } from "@/lib/orpheus";
import { PageHeader, ErrorNotice } from "@/components/layout";
import { StatusBadge } from "@/components/primitives";
import { absTime } from "@/lib/format";
import { ReviewButtons } from "./ReviewButtons";

export const dynamic = "force-dynamic";

export default async function ModerationPage() {
  let subs: MarketplaceSubmission[] = [];
  let error: string | null = null;
  try {
    subs = (await orpheus.listSubmissions()).data;
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  const pending = subs.filter((s) => s.status === "pending");
  const reviewed = subs.filter((s) => s.status !== "pending");

  return (
    <div className="reveal space-y-6">
      <PageHeader
        eyebrow="Operations"
        title="Marketplace Moderation"
        sub="Review third-party processor submissions before they reach the catalog."
      />

      {error ? (
        <ErrorNotice title="Couldn't load submissions" detail={error} />
      ) : subs.length === 0 ? (
        <div className="panel px-5 py-14 text-center text-sm text-ink-mid">No submissions to review.</div>
      ) : (
        <>
          <div className="panel overflow-hidden">
            <div className="hairline-b px-5 py-3">
              <span className="label">Pending · {pending.length}</span>
            </div>
            {pending.length === 0 ? (
              <div className="px-5 py-10 text-center text-sm text-ink-mid">Nothing awaiting review.</div>
            ) : (
              <table className="w-full text-sm">
                <tbody>
                  {pending.map((s) => (
                    <tr key={s.id} className="border-b border-hairline/60 last:border-0">
                      <td className="px-5 py-3">
                        <div className="font-medium text-ink-hi">{s.display_name || s.name}</div>
                        <div className="mt-0.5 text-2xs text-ink-lo">
                          {s.name} · {s.publisher}
                        </div>
                      </td>
                      <td className="max-w-xs px-3 py-3 text-ink-mid">
                        <span className="line-clamp-2">{s.description}</span>
                      </td>
                      <td className="tnum px-3 py-3 text-2xs text-ink-lo">{absTime(s.submitted_at)}</td>
                      <td className="px-5 py-3">
                        <ReviewButtons id={s.id} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>

          {reviewed.length > 0 && (
            <div className="panel overflow-hidden">
              <div className="hairline-b px-5 py-3">
                <span className="label">Reviewed · {reviewed.length}</span>
              </div>
              <table className="w-full text-sm">
                <tbody>
                  {reviewed.map((s) => (
                    <tr key={s.id} className="border-b border-hairline/60 last:border-0">
                      <td className="px-5 py-3">
                        <div className="font-medium text-ink-hi">{s.display_name || s.name}</div>
                        <div className="mt-0.5 text-2xs text-ink-lo">{s.publisher}</div>
                      </td>
                      <td className="px-3 py-3">
                        <StatusBadge status={s.status} />
                      </td>
                      <td className="px-3 py-3 text-2xs text-ink-mid">{s.review_notes || "—"}</td>
                      <td className="tnum px-5 py-3 text-right text-2xs text-ink-lo">{absTime(s.reviewed_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}
